package instabox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/dispatch"
)

// fakeAPI records the calls the provider makes and answers like InstaBox.
type fakeAPI struct {
	mu    sync.Mutex
	calls []string
	files map[string]string
	keys  []string
	// onFile is called when the config file arrives (the "machine" then dials in).
	onFile func(cfg map[string]string)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.keys = append(f.keys, r.Header.Get("X-API-Key"))
	f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	if r.Method != "GET" && r.Header.Get("Idempotency-Key") == "" {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"code":"bad_request","message":"Idempotency-Key header is required"}}`)
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/machines":
		var req CreateRequest
		json.Unmarshal(body, &req)
		if req.SnapshotID != "sn_runner" || req.DeleteAfterSeconds != 7200 {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"bad_request","message":"unexpected create body"}}`)
			return
		}
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"op_1","machineId":"m_1","type":"create","state":"queued"}`)
	case r.URL.Path == "/v1/operations/op_1":
		io.WriteString(w, `{"id":"op_1","machineId":"m_1","type":"create","state":"succeeded","machine":{"spec":{"vcpus":4,"memMiB":8192,"persistentDiskGiB":20,"rootDiskGiB":16}}}`)
	case r.URL.Path == "/v1/operations/op_2":
		io.WriteString(w, `{"id":"op_2","machineId":"m_1","type":"delete","state":"succeeded"}`)
	case r.Method == "PUT" && r.URL.Path == "/v1/machines/m_1/files":
		var cfg map[string]string
		json.Unmarshal(body, &cfg)
		f.mu.Lock()
		f.files = map[string]string{r.URL.Query().Get("path"): string(body)}
		f.mu.Unlock()
		w.WriteHeader(201)
		if f.onFile != nil {
			go f.onFile(cfg)
		}
	case r.Method == "POST" && r.URL.Path == "/v1/machines/m_1/exec":
		io.WriteString(w, `{"exitCode":0,"stdout":"","stderr":""}`)
	case r.Method == "POST" && r.URL.Path == "/v1/machines/m_1/snapshots":
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"sn_new","status":"pending"}`)
	case r.URL.Path == "/v1/snapshots/sn_new":
		io.WriteString(w, `{"id":"sn_new","status":"ready"}`)
	case r.Method == "DELETE" && r.URL.Path == "/v1/machines/m_1":
		if r.Header.Get("X-Confirm-Delete") != "m_1" {
			w.WriteHeader(409)
			return
		}
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"op_2","machineId":"m_1","type":"delete","state":"queued"}`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"not_found","message":"route not found"}}`)
	}
}

func TestProviderCreatesRunsAndDeletes(t *testing.T) {
	var fake fakeAPI
	api := httptest.NewServer(&fake)
	defer api.Close()

	ctl := &dispatch.Controller{}
	ctlSrv := httptest.NewServer(ctl.Handler())
	defer ctlSrv.Close()

	// The "machine": when its config arrives it reports the job done.
	fake.onFile = func(cfg map[string]string) {
		req, _ := http.NewRequest("POST", cfg["url"]+"/v1/done", strings.NewReader(`{"exit_code":0}`))
		req.Header.Set("Authorization", "Bearer "+cfg["token"])
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
	p := &Provider{
		Client:        &Client{URL: api.URL, Key: "k-test", Poll: time.Millisecond},
		Controller:    ctl,
		ControllerURL: ctlSrv.URL,
		Snapshot:      "sn_runner",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := p.Run(ctx, "rc-test", dispatch.Job{Source: func(io.Writer) error { return nil }}, nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	got := strings.Join(fake.calls, "\n")
	order := []string{"POST /v1/machines", "PUT /v1/machines/m_1/files", "DELETE /v1/machines/m_1"}
	last := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i <= last {
			t.Fatalf("calls out of order, want %v:\n%s", order, got)
		}
		last = i
	}
	us := p.Usage()
	if len(us) != 1 || us[0].Machine != "m_1" || us[0].Spec.VCPUs != 4 || us[0].Spec.RootGiB+us[0].Spec.DiskGiB != 36 {
		t.Fatalf("usage = %+v", us)
	}
	if us[0].Ready.IsZero() || us[0].Deleted.IsZero() || us[0].Deleted.Before(us[0].Ready) {
		t.Errorf("usage times: %+v", us[0])
	}
	if _, ok := fake.files["/data/home/.raccoonci-agent.json"]; !ok {
		t.Errorf("config not written where the agent looks: %v", fake.files)
	}
	for _, k := range fake.keys {
		if k != "k-test" {
			t.Errorf("a call went without the key: %q", k)
		}
	}
}

// A job that Keep approves leaves a snapshot: the machine is cleaned, saved,
// and deleted only after the snapshot is ready.
func TestProviderSavesASnapshotBeforeDeleting(t *testing.T) {
	var fake fakeAPI
	api := httptest.NewServer(&fake)
	defer api.Close()
	ctl := &dispatch.Controller{}
	ctlSrv := httptest.NewServer(ctl.Handler())
	defer ctlSrv.Close()
	fake.onFile = func(cfg map[string]string) {
		req, _ := http.NewRequest("POST", cfg["url"]+"/v1/done", strings.NewReader(`{"exit_code":0}`))
		req.Header.Set("Authorization", "Bearer "+cfg["token"])
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	p := &Provider{Client: &Client{URL: api.URL, Key: "k", Poll: time.Millisecond},
		Controller: ctl, ControllerURL: ctlSrv.URL, Snapshot: "sn_runner"}
	var gotID string
	var gotErr error
	_, err := p.Run(context.Background(), "rc-test", dispatch.Job{Source: func(io.Writer) error { return nil }}, &RunOptions{
		From: "sn_runner",
		Keep: func(r dispatch.Result) string {
			if r.ExitCode != 0 {
				return ""
			}
			return "rc-env-abc"
		},
		Kept: func(id string, err error) { gotID, gotErr = id, err },
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Wait()
	if gotID != "sn_new" || gotErr != nil {
		t.Errorf("kept %q, %v", gotID, gotErr)
	}
	calls := strings.Join(fake.calls, "\n")
	for _, pair := range [][2]string{
		{"POST /v1/machines/m_1/exec", "POST /v1/machines/m_1/snapshots"},
		{"POST /v1/machines/m_1/snapshots", "GET /v1/snapshots/sn_new"},
		{"GET /v1/snapshots/sn_new", "DELETE /v1/machines/m_1"},
	} {
		if a, b := strings.Index(calls, pair[0]), strings.Index(calls, pair[1]); a < 0 || b < a {
			t.Errorf("%q must come before %q:\n%s", pair[0], pair[1], calls)
		}
	}
	if strings.Count(calls, "DELETE /v1/machines/m_1") != 1 {
		t.Errorf("machine deleted more than once:\n%s", calls)
	}
}

// A machine that never reports back is still deleted.
func TestProviderDeletesTheMachineWhenTheRunFails(t *testing.T) {
	var fake fakeAPI
	api := httptest.NewServer(&fake)
	defer api.Close()
	ctl := &dispatch.Controller{StartTimeout: 100 * time.Millisecond}
	p := &Provider{Client: &Client{URL: api.URL, Key: "k", Poll: time.Millisecond},
		Controller: ctl, ControllerURL: "http://127.0.0.1:1", Snapshot: "sn_runner"}
	_, err := p.Run(context.Background(), "rc-test", dispatch.Job{Source: func(io.Writer) error { return nil }}, nil)
	if err == nil || !strings.Contains(err.Error(), "did not contact") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.Join(fake.calls, "\n"), "DELETE /v1/machines/m_1") {
		t.Errorf("machine left behind:\n%v", fake.calls)
	}
}

// Cost is worked out from the raw usage under whatever prices are given, so a
// price change needs no new measurement.
func TestCostFollowsThePricesGiven(t *testing.T) {
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	u := Usage{Spec: Spec{VCPUs: 4, MemMiB: 8192, RootGiB: 16, DiskGiB: 20}, Requested: start, Deleted: start.Add(100 * time.Second)}
	// 4 vCPUs, 8 GiB, 36 GiB of disk, at the prices on the page of 2026-10-29.
	want := 100 * (4*0.000001111 + 8*0.000000625 + 36*0.000000031)
	if got := InstaBoxPrices.Cost(u); got < want*0.9999 || got > want*1.0001 {
		t.Errorf("cost %.9f, want %.9f", got, want)
	}
	half := InstaBoxPrices
	half.VCPUSec, half.MemGiBSec, half.StorageGiBSec = half.VCPUSec/2, half.MemGiBSec/2, half.StorageGiBSec/2
	if got := half.Cost(u); got < want/2*0.9999 || got > want/2*1.0001 {
		t.Errorf("half price: %.9f", got)
	}
	s := InstaBoxPrices.Summarize([]Usage{u, u})
	if s.Machines != 2 || s.Seconds != 200 {
		t.Errorf("%+v", s)
	}
}

func TestErrorsCarryTheScopeTheKeyLacks(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"code":"permission_denied","message":"no","requiredScopes":["machines:delete"]}}`)
	}))
	defer api.Close()
	err := (&Client{URL: api.URL, Key: "k"}).DeleteMachine(context.Background(), "m_1")
	var e *Error
	if !errors.As(err, &e) || e.Status != 403 || !strings.Contains(err.Error(), "machines:delete") {
		t.Errorf("err = %v", err)
	}
}

// A run cancelled while its machine was still being created left the machine
// running: the delete was refused with operation_conflict and given up on.
func TestDeletingAMachineStillBeingCreatedWaitsAndTriesAgain(t *testing.T) {
	var mu sync.Mutex
	deletes := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "DELETE" && r.URL.Path == "/v1/machines/m_1":
			mu.Lock()
			deletes++
			n := deletes
			mu.Unlock()
			if n < 3 {
				w.WriteHeader(409)
				io.WriteString(w, `{"error":{"code":"operation_conflict","message":"machine has an active create operation"}}`)
				return
			}
			w.WriteHeader(202)
			io.WriteString(w, `{"id":"op_9","machineId":"m_1","type":"delete","state":"queued"}`)
		case r.URL.Path == "/v1/operations/op_9":
			io.WriteString(w, `{"id":"op_9","state":"succeeded"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	c := &Client{Key: "k", URL: api.URL, Poll: time.Millisecond}
	if err := c.DeleteMachine(context.Background(), "m_1"); err != nil {
		t.Fatal(err)
	}
	if deletes != 3 {
		t.Fatalf("%d delete calls, want 3 (two refused, one accepted)", deletes)
	}
	// A conflict that never clears ends with the context.
	forever := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		io.WriteString(w, `{"error":{"code":"operation_conflict","message":"busy"}}`)
	}))
	defer forever.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := (&Client{Key: "k", URL: forever.URL, Poll: time.Millisecond}).DeleteMachine(ctx, "m_1"); err == nil || time.Since(began) > 5*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(began))
	}
	// Other conflicts are not retried.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deletes++
		mu.Unlock()
		w.WriteHeader(409)
		io.WriteString(w, `{"error":{"code":"something_else","message":"no"}}`)
	}))
	defer other.Close()
	mu.Lock()
	deletes = 0
	mu.Unlock()
	if err := (&Client{Key: "k", URL: other.URL, Poll: time.Millisecond}).DeleteMachine(context.Background(), "m_1"); err == nil || deletes != 1 {
		t.Fatalf("err = %v after %d calls", err, deletes)
	}
}

func TestDeletingAGoneMachineIsFine(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"not_found","message":"gone"}}`)
	}))
	defer api.Close()
	if err := (&Client{URL: api.URL, Key: "k"}).DeleteMachine(context.Background(), "m_1"); err != nil {
		t.Error(err)
	}
}
