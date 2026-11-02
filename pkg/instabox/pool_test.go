package instabox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/dispatch"
)

// multiAPI is an InstaBox that can make many machines. A "machine" reports the
// job done as soon as a config file is written to it.
type multiAPI struct {
	mu      sync.Mutex
	made    int
	deleted map[string]bool
	files   []string // "machine path"
	vcpus   int
}

func (f *multiAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && p == "/v1/machines":
		f.made++
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"op_c`+strconv.Itoa(f.made)+`","machineId":"m_`+strconv.Itoa(f.made)+`","type":"create","state":"queued"}`)
	case strings.HasPrefix(p, "/v1/operations/op_c"):
		n := strings.TrimPrefix(p, "/v1/operations/op_c")
		io.WriteString(w, `{"id":"op_c`+n+`","machineId":"m_`+n+`","type":"create","state":"succeeded","machine":{"spec":{"vcpus":`+strconv.Itoa(f.vcpus)+`,"memMiB":4096,"persistentDiskGiB":10,"rootDiskGiB":10}}}`)
	case strings.HasPrefix(p, "/v1/operations/op_d"):
		io.WriteString(w, `{"id":"x","type":"delete","state":"succeeded"}`)
	case r.Method == "PUT" && strings.HasSuffix(p, "/files"):
		m := strings.Split(p, "/")[3]
		f.files = append(f.files, m+" "+r.URL.Query().Get("path"))
		var cfg map[string]string
		json.Unmarshal(body, &cfg)
		w.WriteHeader(201)
		go func() {
			time.Sleep(20 * time.Millisecond)
			req, _ := http.NewRequest("POST", cfg["url"]+"/v1/done", strings.NewReader(`{"exit_code":0}`))
			req.Header.Set("Authorization", "Bearer "+cfg["token"])
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
	case r.Method == "DELETE" && strings.HasPrefix(p, "/v1/machines/"):
		id := strings.TrimPrefix(p, "/v1/machines/")
		if f.deleted == nil {
			f.deleted = map[string]bool{}
		}
		f.deleted[id] = true
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"op_d`+id+`","machineId":"`+id+`","type":"delete","state":"queued"}`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"not_found","message":"route not found"}}`)
	}
}

func newPoolFor(t *testing.T, api *multiAPI, slots int, linger time.Duration) *Pool {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	ctl := &dispatch.Controller{}
	csrv := httptest.NewServer(ctl.Handler())
	t.Cleanup(csrv.Close)
	return &Pool{
		P: &Provider{Client: &Client{URL: srv.URL, Key: "k", Poll: time.Millisecond}, Controller: ctl,
			ControllerURL: csrv.URL, Snapshot: "sn"},
		Slots: slots, Linger: linger, Prefix: "t",
	}
}

func job() dispatch.Job { return dispatch.Job{Source: func(io.Writer) error { return nil }} }

func runN(t *testing.T, p *Pool, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, err := p.Run(ctx, job()); err != nil || res.ExitCode != 0 {
				t.Errorf("run: %+v, %v", res, err)
			}
		}()
	}
	wg.Wait()
}

// Jobs that arrive together fill a machine up to its slots before another is
// made, and each slot of a machine reads its own config file.
func TestPoolFillsAMachineBeforeMakingAnother(t *testing.T) {
	api := &multiAPI{vcpus: 4}
	p := newPoolFor(t, api, 2, time.Minute)
	runN(t, p, 3)
	if got := p.Machines(); got != 2 {
		t.Errorf("3 jobs with 2 slots made %d machines, want 2", got)
	}
	seen := map[string]bool{}
	for _, f := range api.files {
		seen[f] = true
	}
	var slot0, slot1 bool
	for f := range seen {
		slot0 = slot0 || strings.HasSuffix(f, ".raccoonci-agent.json")
		slot1 = slot1 || strings.HasSuffix(f, ".raccoonci-agent.json.1")
	}
	if !slot0 || !slot1 {
		t.Errorf("slots did not use their own config files: %v", api.files)
	}
	p.Close()
	p.P.Wait()
	if len(api.deleted) != 2 {
		t.Errorf("deleted %v, want both machines", api.deleted)
	}
}

// A job that follows another within the linger time reuses its machine.
func TestPoolReusesAMachineAfterAJobEnds(t *testing.T) {
	api := &multiAPI{vcpus: 2}
	p := newPoolFor(t, api, 0, time.Minute)
	runN(t, p, 1)
	runN(t, p, 1)
	if got := p.Machines(); got != 1 {
		t.Errorf("made %d machines for two jobs in a row, want 1", got)
	}
	if len(api.deleted) != 0 {
		t.Errorf("a machine inside its linger time was deleted: %v", api.deleted)
	}
	p.Close()
	p.P.Wait()
	if len(api.deleted) != 1 {
		t.Errorf("Close left machines: %v", api.deleted)
	}
}

// With no slot count, a machine takes as many jobs as it has vCPUs.
func TestPoolDefaultsToOneJobPerVCPU(t *testing.T) {
	api := &multiAPI{vcpus: 1}
	p := newPoolFor(t, api, 0, time.Minute)
	runN(t, p, 1) // learns the machine's size
	runN(t, p, 3)
	if got := p.Machines(); got != 3 {
		t.Errorf("1-vCPU machines: made %d for 1+3 concurrent jobs, want 3", got)
	}
	p.Close()
	p.P.Wait()
}

// An empty machine is deleted after the linger time.
func TestPoolDeletesAnIdleMachineAfterTheLingerTime(t *testing.T) {
	api := &multiAPI{vcpus: 2}
	p := newPoolFor(t, api, 2, 50*time.Millisecond)
	runN(t, p, 1)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		api.mu.Lock()
		n := len(api.deleted)
		api.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.Close()
	p.P.Wait()
	if len(api.deleted) != 1 {
		t.Fatalf("deleted = %v", api.deleted)
	}
	runN(t, p2(t, api), 1) // a new pool still works against the same account
}

func p2(t *testing.T, api *multiAPI) *Pool { return newPoolFor(t, api, 2, time.Millisecond) }

// A pool that is closed refuses new jobs.
func TestClosedPoolRefusesJobs(t *testing.T) {
	p := newPoolFor(t, &multiAPI{vcpus: 2}, 2, time.Minute)
	p.Close()
	if _, err := p.Run(context.Background(), job()); err == nil {
		t.Fatal("a closed pool took a job")
	}
}

func TestStartupTimeIsTheMedian(t *testing.T) {
	p := &Provider{}
	if p.StartupTime() != 0 {
		t.Fatal("startup time without machines")
	}
	now := time.Now()
	for _, d := range []time.Duration{3, 9, 5} {
		p.usage.add(&Usage{Requested: now, Ready: now.Add(d * time.Second)})
	}
	if got := p.StartupTime(); got != 5*time.Second {
		t.Errorf("startup = %v, want 5s", got)
	}
}
