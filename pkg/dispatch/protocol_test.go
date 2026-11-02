package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// tap records the protocol headers of the traffic between an agent and a
// controller.
type tap struct {
	inner http.Handler
	mu    sync.Mutex
	sent  []string // what agents said
	reply []string // what the controller said
}

func (t *tap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	t.inner.ServeHTTP(rec, r)
	t.mu.Lock()
	t.sent = append(t.sent, r.Header.Get(ProtocolHeader))
	t.reply = append(t.reply, rec.Header().Get(ProtocolHeader))
	t.mu.Unlock()
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes())
}

func tapped(t *testing.T, c *Controller, job Job) (*Run, *Agent, *tap) {
	t.Helper()
	tp := &tap{inner: c.Handler()}
	srv := httptest.NewServer(tp)
	t.Cleanup(srv.Close)
	run, err := c.Submit(job)
	if err != nil {
		t.Fatal(err)
	}
	return run, &Agent{URL: srv.URL, Token: run.Token, Dir: t.TempDir(), Heartbeat: 50 * time.Millisecond}, tp
}

func quickJob(t *testing.T) Job {
	src := sourceDir(t)
	return Job{Args: []string{"x"}, Source: func(w io.Writer) error { return TarDir(w, src) }}
}

func TestAgentAndControllerSayWhichProtocolTheySpeak(t *testing.T) {
	run, ag, tp := tapped(t, &Controller{}, quickJob(t))
	ag.Exe = fakeRaccoonci(t, `exit 0`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ag.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if res, err := run.Wait(ctx); err != nil || res.ExitCode != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if len(tp.sent) < 3 {
		t.Fatalf("only %d requests seen", len(tp.sent))
	}
	for i := range tp.sent {
		if tp.sent[i] != "1" || tp.reply[i] != "1" {
			t.Errorf("request %d: the agent said %q, the controller %q; want 1 and 1", i, tp.sent[i], tp.reply[i])
		}
	}
}

func TestAnAgentThatPredatesTheHeaderIsServedAsProtocolOne(t *testing.T) {
	c := &Controller{}
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()
	run, _ := c.Submit(quickJob(t))
	req, _ := http.NewRequest("GET", srv.URL+"/v1/job", nil)
	req.Header.Set("Authorization", "Bearer "+run.Token) // no protocol header: every agent built so far
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get(ProtocolHeader) != "1" {
		t.Fatalf("legacy agent: %d, controller says protocol %q", resp.StatusCode, resp.Header.Get(ProtocolHeader))
	}
}

func TestAnAgentNewerThanTheControllerIsRefusedAndTheRunEndsWithTheReason(t *testing.T) {
	run, ag, _ := tapped(t, &Controller{}, quickJob(t))
	ag.version = ProtocolVersion + 1
	ag.Exe = fakeRaccoonci(t, `exit 0`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ag.Run(ctx); err == nil {
		t.Fatal("a newer agent was served")
	}
	// Whoever waits for the run hears why at once, not after a timeout.
	res, err := run.Wait(ctx)
	if err != nil || res.ExitCode != -1 || !strings.Contains(res.Error, "speaks protocol 2") || !strings.Contains(res.Error, "must be upgraded") {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestAnOlderAgentIsStillServedByANewerController(t *testing.T) {
	c := &Controller{version: ProtocolVersion + 1} // a later build of the controller
	run, ag, tp := tapped(t, c, quickJob(t))
	ag.Exe = fakeRaccoonci(t, `exit 0`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ag.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if res, err := run.Wait(ctx); err != nil || res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if tp.reply[0] != "2" {
		t.Errorf("the newer controller says %q", tp.reply[0])
	}
}

func TestAMalformedProtocolHeaderIsRefused(t *testing.T) {
	for _, bad := range []string{"two", "0", "-1", "1.5"} {
		c := &Controller{}
		srv := httptest.NewServer(c.Handler())
		run, _ := c.Submit(quickJob(t))
		req, _ := http.NewRequest("GET", srv.URL+"/v1/job", nil)
		req.Header.Set("Authorization", "Bearer "+run.Token)
		req.Header.Set(ProtocolHeader, bad)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		srv.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("header %q: %d, want 400", bad, resp.StatusCode)
		}
	}
}
