package dispatch

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeRaccoonci stands in for the binary the agent runs.
func fakeRaccoonci(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "raccoonci")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func sourceDir(t *testing.T) string {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte("on: push\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	os.Symlink("main.go", filepath.Join(dir, "link.go"))
	return dir
}

func setup(t *testing.T, c *Controller, job Job) (*Run, *Agent) {
	t.Helper()
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	run, err := c.Submit(job)
	if err != nil {
		t.Fatal(err)
	}
	return run, &Agent{URL: srv.URL, Token: run.Token, Dir: t.TempDir(), Heartbeat: 50 * time.Millisecond}
}

func TestJobRunsOnTheAgentAndReportsBack(t *testing.T) {
	src := sourceDir(t)
	var log syncBuf
	c := &Controller{}
	run, ag := setup(t, c, Job{
		Args:    []string{"-job", "build", ".github/workflows/ci.yml"},
		Env:     map[string]string{"RC_ENV": "plain"},
		Secrets: map[string]string{"TOKEN": "s3cret"},
		Source:  func(w io.Writer) error { return TarDir(w, src) },
		Log:     &log,
	})
	ag.Exe = fakeRaccoonci(t, `echo "args: $*"; echo "env: $RC_ENV $TOKEN"; ls; readlink link.go; echo oops >&2; exit 3`)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ag.Run(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := run.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Error != "" {
		t.Errorf("result = %+v", res)
	}
	out := log.String()
	for _, want := range []string{
		"args: run -secret TOKEN -job build .github/workflows/ci.yml",
		"env: plain s3cret",
		"main.go", // ran inside the unpacked tree
		"oops",    // stderr is shipped too
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestTokenOpensOnlyItsOwnJob(t *testing.T) {
	c := &Controller{}
	src := sourceDir(t)
	a, _ := setup(t, c, Job{Source: func(w io.Writer) error { return TarDir(w, src) }})
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()
	for _, tok := range []string{"", "nope", a.Token + "x"} {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/job", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: %s", tok, resp.Status)
		}
	}
	// After the run is forgotten its token is dead too.
	c.Forget(a)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/job", nil)
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("forgotten token: %s", resp.Status)
	}
}

func TestMachineThatNeverCallsIsReported(t *testing.T) {
	c := &Controller{StartTimeout: 200 * time.Millisecond}
	run, err := c.Submit(Job{Source: func(w io.Writer) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = run.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not contact") {
		t.Errorf("err = %v", err)
	}
}

func TestMachineThatGoesSilentIsReported(t *testing.T) {
	c := &Controller{Lease: 300 * time.Millisecond}
	src := sourceDir(t)
	run, ag := setup(t, c, Job{Source: func(w io.Writer) error { return TarDir(w, src) }})
	// One request, then nothing: the machine died.
	req, _ := http.NewRequest("GET", ag.URL+"/v1/job", nil)
	req.Header.Set("Authorization", "Bearer "+run.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, err = run.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "silent") {
		t.Errorf("err = %v", err)
	}
}

func TestHeartbeatKeepsALongQuietJobAlive(t *testing.T) {
	c := &Controller{Lease: 400 * time.Millisecond}
	src := sourceDir(t)
	run, ag := setup(t, c, Job{Source: func(w io.Writer) error { return TarDir(w, src) }})
	ag.Exe = fakeRaccoonci(t, `sleep 1.5`) // no output for several leases
	go ag.Run(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := run.Wait(ctx)
	if err != nil || res.ExitCode != 0 {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestLogChunksAreIdempotentAndGapsRefused(t *testing.T) {
	var log syncBuf
	c := &Controller{}
	src := sourceDir(t)
	run, ag := setup(t, c, Job{Source: func(w io.Writer) error { return TarDir(w, src) }, Log: &log})
	post := func(off, body string) (int, string) {
		req, _ := http.NewRequest("POST", ag.URL+"/v1/log", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+run.Token)
		req.Header.Set("X-Offset", off)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("X-Expected-Offset")
	}
	if code, _ := post("0", "abc"); code != 204 {
		t.Fatal(code)
	}
	if code, _ := post("0", "abc"); code != 204 { // the reply was lost, the chunk is sent again
		t.Fatal(code)
	}
	if code, _ := post("2", "cdef"); code != 204 { // overlaps: only "def" is new
		t.Fatal(code)
	}
	if code, want := post("9", "x"); code != 409 || want != "6" {
		t.Errorf("gap: %d, expected offset %q", code, want)
	}
	if log.String() != "abcdef" {
		t.Errorf("log = %q", log.String())
	}
}

func TestAgentRefusesAnUnsafeControllerURL(t *testing.T) {
	for _, u := range []string{"http://example.com", "http://10.0.0.1:80", "ftp://x", "", "https://"} {
		if err := (&Agent{URL: u}).check(); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	for _, u := range []string{"https://ctl.example.com/x", "http://127.0.0.1:8080", "http://[::1]:1"} {
		if err := (&Agent{URL: u}).check(); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
}

func TestAgentReportsAnUnusableAssignment(t *testing.T) {
	c := &Controller{}
	src := sourceDir(t)
	run, ag := setup(t, c, Job{
		Secrets: map[string]string{"--evil": "x"},
		Source:  func(w io.Writer) error { return TarDir(w, src) },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ag.Run(ctx); err == nil {
		t.Error("a secret named --evil was accepted")
	}
	res, err := run.Wait(ctx)
	if err != nil || res.ExitCode != -1 || !strings.Contains(res.Error, "secret name") {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}
