package instabox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartLimitLetsOnlyPerMinuteStartsThroughPerWindow(t *testing.T) {
	l := &StartLimit{PerMinute: 3, Window: time.Minute}
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if ok, _ := l.reserve(t0.Add(time.Duration(i) * time.Second)); !ok {
			t.Fatalf("start %d refused", i)
		}
	}
	ok, wait := l.reserve(t0.Add(10 * time.Second))
	if ok || wait != 50*time.Second { // the first start leaves the window at 12:01:00
		t.Fatalf("fourth: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.reserve(t0.Add(61 * time.Second)); !ok {
		t.Fatal("a start must be free once the first has left the window")
	}
	if ok, _ := l.reserve(t0.Add(61 * time.Second)); ok {
		t.Fatal("only one has left")
	}
}

func TestStartLimitHoldsEveryStartAfterARefusal(t *testing.T) {
	l := &StartLimit{PerMinute: 100, Hold: time.Hour}
	l.Refused()
	if ok, wait := l.reserve(time.Now()); ok || wait < 59*time.Minute {
		t.Fatalf("ok=%v wait=%v", ok, wait)
	}
}

func TestNoLimitAllowsEverything(t *testing.T) {
	var none *StartLimit
	if _, err := none.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	none.Refused() // nothing happens
	l := &StartLimit{}
	for i := 0; i < 50; i++ {
		if ok, _ := l.reserve(time.Now()); !ok {
			t.Fatal("a zero PerMinute means no limit of our own")
		}
	}
}

func TestStartLimitWaitEndsWithTheContext(t *testing.T) {
	l := &StartLimit{PerMinute: 1, Window: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Wait(ctx); err == nil {
		t.Fatal("the second start must wait until the context ends")
	}
}

// A 429 from the create call is not a failure of the job: the start is asked
// again after the hold.
func TestProviderTriesAgainAfter429(t *testing.T) {
	var mu sync.Mutex
	var creates int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/machines":
			if atomic.AddInt32(&creates, 1) <= 2 {
				w.WriteHeader(429)
				io.WriteString(w, `{"error":{"code":"rate_limited","message":"too many machine starts"}}`)
				return
			}
			w.WriteHeader(202)
			io.WriteString(w, `{"id":"op_1","machineId":"m_1","type":"create","state":"queued"}`)
		case r.URL.Path == "/v1/operations/op_1":
			io.WriteString(w, `{"id":"op_1","machineId":"m_1","type":"create","state":"succeeded"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	var logs []string
	p := &Provider{Client: &Client{URL: api.URL, Key: "k", Poll: time.Millisecond}, Snapshot: "sn_runner",
		Limit: &StartLimit{PerMinute: 10, Hold: 30 * time.Millisecond},
		Logf:  func(f string, a ...any) { logs = append(logs, f) }}
	began := time.Now()
	id, _, err := p.Acquire(context.Background(), "rc-x")
	if err != nil || id != "m_1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if creates != 3 || time.Since(began) < 50*time.Millisecond {
		t.Fatalf("creates=%d after %v: want 3 attempts with the hold between", creates, time.Since(began))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "429") {
		t.Fatalf("the refusal must be logged: %v", logs)
	}
}

func TestProviderWithoutALimitReportsA429(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"code":"rate_limited","message":"too many machine starts"}}`)
	}))
	defer api.Close()
	p := &Provider{Client: &Client{URL: api.URL, Key: "k"}, Snapshot: "sn_runner"}
	_, _, err := p.Acquire(context.Background(), "rc-x")
	if !IsRateLimit(err) {
		t.Fatalf("err = %v", err)
	}
}
