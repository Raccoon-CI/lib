package instabox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildAPI answers like InstaBox for BuildRunnerSnapshot. version is what the
// "uploaded binary" prints; ldd is what ldd says about it.
type buildAPI struct {
	mu      sync.Mutex
	calls   []string
	cmds    []string
	files   map[string]string
	version string
	ldd     string
}

func (f *buildAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/machines":
		var req CreateRequest
		json.Unmarshal(body, &req)
		if req.TemplateID != "universal" || req.SnapshotID != "" || req.DeleteAfterSeconds == 0 {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"bad_request","message":"unexpected create body"}}`)
			return
		}
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"op_1","machineId":"m_b","type":"create","state":"queued"}`)
	case r.URL.Path == "/v1/operations/op_1":
		io.WriteString(w, `{"id":"op_1","state":"succeeded"}`)
	case r.URL.Path == "/v1/operations/op_2":
		io.WriteString(w, `{"id":"op_2","state":"succeeded"}`)
	case r.Method == "GET" && r.URL.Path == "/v1/machines/m_b":
		io.WriteString(w, `{"id":"m_b","template":{"id":"universal","version":"2026-11-01.1"}}`)
	case r.Method == "PUT" && r.URL.Path == "/v1/machines/m_b/files":
		f.mu.Lock()
		if f.files == nil {
			f.files = map[string]string{}
		}
		f.files[r.URL.Query().Get("path")] = string(body)
		f.mu.Unlock()
	case r.Method == "POST" && r.URL.Path == "/v1/machines/m_b/exec":
		var req struct{ Cmd string }
		json.Unmarshal(body, &req)
		f.mu.Lock()
		f.cmds = append(f.cmds, req.Cmd)
		f.mu.Unlock()
		out := ""
		switch {
		case strings.HasSuffix(req.Cmd, "-version") && !strings.Contains(req.Cmd, "test !"):
			out = f.version + "\n"
		case strings.HasPrefix(req.Cmd, "ldd"):
			out = f.ldd
		case strings.Contains(req.Cmd, "test !"):
			out = f.version + "\n"
		}
		json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": out, "stderr": ""})
	case r.Method == "POST" && r.URL.Path == "/v1/machines/m_b/snapshots":
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"sn_runner2","status":"ready"}`)
	case r.Method == "DELETE" && r.URL.Path == "/v1/machines/m_b":
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"op_2","machineId":"m_b","type":"delete","state":"queued"}`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"not_found","message":"`+r.Method+" "+r.URL.Path+`"}}`)
	}
}

func buildClient(t *testing.T, f *buildAPI) *Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Key: "k", Poll: time.Millisecond}
}

func TestBuildRunnerSnapshotInstallsAndSnapshotsACleanMachine(t *testing.T) {
	f := &buildAPI{version: "abc123", ldd: "\tnot a dynamic executable\n"}
	c := buildClient(t, f)
	id, err := c.BuildRunnerSnapshot(context.Background(), RunnerSpec{Binary: []byte("ELF"), Version: "abc123", Name: "raccoonci-runner-abc123"}, nil)
	if err != nil || id != "sn_runner2" {
		t.Fatalf("id = %q, err = %v", id, err)
	}
	if f.files["/tmp/raccoonci-new"] != "ELF" {
		t.Errorf("the binary was not uploaded: %v", f.files)
	}
	var manifest map[string]string
	if err := json.Unmarshal([]byte(f.files["/tmp/raccoonci-runner.json"]), &manifest); err != nil ||
		manifest["raccoonci"] != "abc123" || manifest["template"] != "universal 2026-11-01.1" {
		t.Errorf("manifest = %v, %v", manifest, err)
	}
	if !strings.Contains(f.files["/tmp/raccoonci-agent.service"], "ExecStart=/usr/local/bin/raccoonci agent -loop -slots 8 -wait 24h -config /data/home/.raccoonci-agent.json") {
		t.Errorf("unit = %q", f.files["/tmp/raccoonci-agent.service"])
	}
	calls := strings.Join(f.calls, "\n")
	for _, pair := range [][2]string{
		{"POST /v1/machines/m_b/exec", "POST /v1/machines/m_b/snapshots"},
		{"POST /v1/machines/m_b/snapshots", "DELETE /v1/machines/m_b"},
	} {
		if a, b := strings.Index(calls, pair[0]), strings.Index(calls, pair[1]); a < 0 || b < a {
			t.Errorf("%q must come before %q:\n%s", pair[0], pair[1], calls)
		}
	}
	if !strings.Contains(strings.Join(f.cmds, "\n"), "systemctl enable raccoonci-agent") {
		t.Errorf("the service was not enabled: %v", f.cmds)
	}
}

func TestBuildRunnerSnapshotRefusesAWrongOrDynamicBinary(t *testing.T) {
	for name, tc := range map[string]struct{ version, ldd, want string }{
		"version": {"other", "not a dynamic executable", "reports version"},
		"dynamic": {"abc123", "\tlibc.so.6 => /lib/libc.so.6\n", "dynamically linked"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &buildAPI{version: tc.version, ldd: tc.ldd}
			c := buildClient(t, f)
			id, err := c.BuildRunnerSnapshot(context.Background(), RunnerSpec{Binary: []byte("ELF"), Version: "abc123", Name: "n"}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || id != "" {
				t.Fatalf("id = %q, err = %v", id, err)
			}
			calls := strings.Join(f.calls, "\n")
			if strings.Contains(calls, "/snapshots") {
				t.Error("a snapshot was taken of a rejected build")
			}
			if !strings.Contains(calls, "DELETE /v1/machines/m_b") {
				t.Errorf("the build machine was not deleted:\n%s", calls)
			}
		})
	}
}
