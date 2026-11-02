package instabox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFleet is InstaBox's machine API, enough for spares: machines with a
// state and metadata, operations that succeed at once, resume that only works
// on a paused machine.
type fakeFleet struct {
	mu       sync.Mutex
	machines map[string]*fakeMachine
	n        int
	calls    []string
}

type fakeMachine struct {
	ID, Name, State string
	Metadata        map[string]string
}

func newFleet() (*fakeFleet, *Client) {
	f := &fakeFleet{machines: map[string]*fakeMachine{}}
	srv := httptest.NewServer(f)
	c := &Client{URL: srv.URL, Key: "k", Poll: time.Millisecond}
	return f, c
}

func (f *fakeFleet) add(name, state string, md map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("m_%d", f.n)
	f.machines[id] = &fakeMachine{ID: id, Name: name, State: state, Metadata: md}
	return id
}

func (f *fakeFleet) get(id string) *fakeMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.machines[id]
}

func (f *fakeFleet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	spec := `{"vcpus":4,"memMiB":8192,"persistentDiskGiB":20,"rootDiskGiB":16}`
	op := func(id string) {
		w.WriteHeader(202)
		fmt.Fprintf(w, `{"id":"op_%s","machineId":"%s","state":"queued"}`, id, id)
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1 machines {id} verb
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/machines":
		var out []map[string]any
		for _, m := range f.machines {
			ok := true
			for _, kv := range r.URL.Query()["metadata"] {
				k, v, _ := strings.Cut(kv, ":")
				if m.Metadata[k] != v {
					ok = false
				}
			}
			if ok {
				out = append(out, map[string]any{"id": m.ID, "name": m.Name, "state": m.State, "metadata": m.Metadata,
					"spec": json.RawMessage(spec)})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"machines": out})
	case r.Method == "GET" && len(parts) == 3 && parts[1] == "machines":
		m := f.machines[parts[2]]
		if m == nil {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"not_found","message":"no machine"}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": m.ID, "name": m.Name, "state": m.State, "metadata": m.Metadata,
			"spec": json.RawMessage(spec)})
	case r.Method == "POST" && r.URL.Path == "/v1/machines":
		var req CreateRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.n++
		id := fmt.Sprintf("m_%d", f.n)
		f.machines[id] = &fakeMachine{ID: id, Name: req.Name, State: "running", Metadata: req.Metadata}
		op(id)
	case strings.HasPrefix(r.URL.Path, "/v1/operations/op_"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/operations/op_")
		fmt.Fprintf(w, `{"id":"op_%s","machineId":"%s","state":"succeeded","machine":{"spec":%s}}`, id, id, spec)
	case r.Method == "POST" && len(parts) == 4 && parts[3] == "suspend":
		f.machines[parts[2]].State = "suspended"
		op(parts[2])
	case r.Method == "POST" && len(parts) == 4 && parts[3] == "resume":
		// Like the real API: resuming a machine that already runs succeeds too.
		if m := f.machines[parts[2]]; m != nil {
			m.State = "running"
		}
		op(parts[2])
	case r.Method == "POST" && len(parts) == 4 && parts[3] == "exec":
		io.WriteString(w, `{"exitCode":0,"stdout":"active\n","stderr":""}`)
	case r.Method == "PATCH" && len(parts) == 3:
		var b struct {
			Metadata map[string]string `json:"metadata"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		f.machines[parts[2]].Metadata = b.Metadata
		io.WriteString(w, `{}`)
	case r.Method == "DELETE" && len(parts) == 3:
		delete(f.machines, parts[2])
		op(parts[2])
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"not_found","message":"no route"}}`)
	}
}

func spareMD(snap string, pausedAgo time.Duration) map[string]string {
	return map[string]string{metaWarm: "warm", metaSnap: snap, metaPaused: fmt.Sprint(time.Now().Add(-pausedAgo).Unix())}
}

func TestClaimResumesAPausedSpareOfTheRightSnapshot(t *testing.T) {
	f, c := newFleet()
	f.add("other", "suspended", spareMD("sn_old", time.Minute))                       // another snapshot's
	f.add("busy", "running", map[string]string{metaWarm: "warm", metaSnap: "sn_run"}) // not paused
	want := f.add("rc-warm-1", "suspended", spareMD("sn_run", 90*time.Second))
	p := &Provider{Client: c, Snapshot: "sn_run"}
	s := &Spares{P: p, Want: 1}

	id, spec, paused, ok := s.Claim(context.Background())
	if !ok || id != want || spec.VCPUs != 4 {
		t.Fatalf("claimed %q %+v ok=%v", id, spec, ok)
	}
	if paused < 89 || paused > 95 {
		t.Errorf("paused %.1f s", paused)
	}
	m := f.get(id)
	if m.State != "running" || m.Metadata[metaWarm] != "in-use" {
		t.Errorf("the claimed machine: %+v", m)
	}
	if _, _, _, ok := s.Claim(context.Background()); ok {
		t.Error("a machine was claimed twice")
	}
}

// Resume cannot tell who came first, so several jobs asking at once for the one
// spare must not all be given it.
func TestConcurrentClaimsOfOneSpareGiveItOnce(t *testing.T) {
	f, c := newFleet()
	f.add("rc-warm-1", "suspended", spareMD("sn_run", time.Minute))
	p := &Provider{Client: c, Snapshot: "sn_run"}
	s := &Spares{P: p, Want: 1}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, _, ok := s.Claim(context.Background()); ok {
				mu.Lock()
				got++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got != 1 {
		t.Errorf("one spare was handed to %d callers", got)
	}
}

func TestClaimSkipsASpareThatWillNotResume(t *testing.T) {
	f, c := newFleet()
	a := f.add("rc-warm-a", "suspended", spareMD("sn_run", time.Minute))
	b := f.add("rc-warm-b", "suspended", spareMD("sn_run", time.Minute))
	// Someone else resumed a between our listing and our resume.
	f.get(a).State = "running"
	f.get(a).Metadata[metaWarm] = "in-use"
	p := &Provider{Client: c, Snapshot: "sn_run"}
	id, _, _, ok := (&Spares{P: p}).Claim(context.Background())
	if !ok || id != b {
		t.Errorf("claimed %q, want %q", id, b)
	}
}

func TestTopUpMakesPausedSparesAndDropsStaleOnes(t *testing.T) {
	f, c := newFleet()
	stale := f.add("rc-warm-old", "suspended", spareMD("sn_old", time.Hour))
	f.add("rc-warm-keep", "suspended", spareMD("sn_run", time.Hour))
	p := &Provider{Client: c, Snapshot: "sn_run"}
	if err := (&Spares{P: p, Want: 3}).TopUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.get(stale) != nil {
		t.Error("a spare of an older snapshot was kept")
	}
	paused := 0
	for _, m := range f.machines {
		if m.State == "suspended" && m.Metadata[metaSnap] == "sn_run" && m.Metadata[metaPaused] != "" {
			paused++
		}
	}
	if paused != 3 {
		t.Errorf("%d paused spares, want 3: %+v", paused, f.machines)
	}
	// The making of a spare is on the books, running part only.
	us := p.Usage()
	if len(us) != 2 || !us[0].Spare || us[0].Deleted.IsZero() {
		t.Errorf("usage = %+v", us)
	}
	// Topped up already: nothing more is made.
	before := len(f.machines)
	if err := (&Spares{P: p, Want: 3}).TopUp(context.Background()); err != nil || len(f.machines) != before {
		t.Errorf("a second top-up changed the fleet: %v", err)
	}
}

func TestRunStartsFromASpareWithoutCreatingAMachine(t *testing.T) {
	f, c := newFleet()
	id := f.add("rc-warm-1", "suspended", spareMD("sn_run", 30*time.Second))
	p := &Provider{Client: c, Snapshot: "sn_run"}
	p.Spares = &Spares{P: p, Want: 1}
	got, use, err := p.obtain(context.Background(), "job", "sn_run", time.Hour, Size{})
	if err != nil || got != id || !use.Resumed || use.PausedSec < 29 {
		t.Fatalf("%q %+v %v", got, use, err)
	}
	for _, call := range f.calls {
		if call == "POST /v1/machines" {
			t.Error("a machine was created although a spare was waiting")
		}
	}
	// A machine from another snapshot (an environment snapshot) never takes a spare.
	f.add("rc-warm-2", "suspended", spareMD("sn_run", time.Second))
	_, use2, err := p.obtain(context.Background(), "job2", "sn_env", time.Hour, Size{})
	if err != nil || use2.Resumed {
		t.Errorf("an environment-snapshot machine took a spare: %+v %v", use2, err)
	}
}

// A paused spare costs its disks and nothing else.
func TestPausedTimeIsPricedAtTheStorageRate(t *testing.T) {
	u := Usage{Spec: Spec{VCPUs: 4, MemMiB: 8192, RootGiB: 16, DiskGiB: 20}, PausedSec: 3600,
		Requested: time.Unix(0, 0), Deleted: time.Unix(10, 0)}
	want := 10*InstaBoxPrices.PerSecond(u.Spec) + 3600*36*InstaBoxPrices.StorageGiBSec
	if got := InstaBoxPrices.Cost(u); got < want*0.9999 || got > want*1.0001 {
		t.Errorf("cost %.9f, want %.9f", got, want)
	}
}
