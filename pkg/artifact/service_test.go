package artifact

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/azblob"
	"github.com/Raccoon-CI/raccoonci/pkg/blob"
)

type env struct {
	t      *testing.T
	svc    *Service
	signer *Signer
	srv    *httptest.Server
	dir    *blob.Dir
	tok    string
	sc     Scope
}

func newEnv(t *testing.T) *env { return newEnvWith(t, func(*Config) {}) }

// newEnvWith is newEnv with a say in the service's Config.
func newEnvWith(t *testing.T, edit func(*Config)) *env {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dir, err := blob.NewDir(t.TempDir(), srv.URL, []byte("0123456789abcdef0123"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	signer, _ := NewSigner([]byte("0123456789abcdef0123"))
	cfg := Config{Store: dir, Signer: signer, BaseURL: srv.URL}
	edit(&cfg)
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc.partSize = 16
	t.Cleanup(svc.Close)
	mux.Handle(blob.DownloadPrefix, dir.Handler())
	mux.Handle("/", svc.Handler())
	sc := Scope{Run: "run-1", Job: "job-1"}
	tok, _ := signer.Token(sc, time.Hour)
	return &env{t: t, svc: svc, signer: signer, srv: srv, dir: dir, tok: tok, sc: sc}
}

// rpc calls a Twirp method and returns the status and decoded body.
func (e *env) rpc(method, token string, body map[string]any) (int, map[string]any) {
	e.t.Helper()
	return e.rpcAt(e.srv.URL, method, token, body)
}

func (e *env) rpcAt(base, method, token string, body map[string]any) (int, map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+twirpPrefix+method, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) ids() map[string]any {
	return map[string]any{"workflow_run_backend_id": e.sc.Run, "workflow_job_run_backend_id": e.sc.Job}
}

func (e *env) with(extra map[string]any) map[string]any {
	m := e.ids()
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (e *env) create(name string) string {
	e.t.Helper()
	code, out := e.rpc("CreateArtifact", e.tok, e.with(map[string]any{"name": name, "version": 7}))
	if code != 200 || out["ok"] != true {
		e.t.Fatalf("CreateArtifact = %d %v", code, out)
	}
	return out["signed_upload_url"].(string)
}

// azureID builds a block id the way @azure/storage-blob does.
func azureID(prefix string, i int) string {
	return base64.StdEncoding.EncodeToString([]byte(prefix + fmt.Sprintf("%012d", i)))
}

const uuidPrefix = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

func putBlock(t *testing.T, uploadURL, blockID string, data []byte) int {
	t.Helper()
	req, _ := http.NewRequest("PUT", uploadURL+"&comp=block&blockid="+url.QueryEscape(blockID), bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func putList(t *testing.T, uploadURL string, ids ...string) (int, string) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, id := range ids {
		b.WriteString("<Latest>" + id + "</Latest>")
	}
	b.WriteString("</BlockList>")
	req, _ := http.NewRequest("PUT", uploadURL+"&comp=blocklist", strings.NewReader(b.String()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("x-ms-error-code")
}

func (e *env) finalize(name string, size int, hash string) (int, map[string]any) {
	return e.rpc("FinalizeArtifact", e.tok, e.with(map[string]any{"name": name, "size": fmt.Sprint(size), "hash": hash}))
}

func (e *env) download(name string) (int, string) {
	e.t.Helper()
	code, out := e.rpc("GetSignedArtifactURL", e.tok, e.with(map[string]any{"name": name}))
	if code != 200 {
		return code, ""
	}
	resp, err := http.Get(out["signed_url"].(string))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestDirectUploadAndDownload(t *testing.T) {
	e := newEnv(t)
	u := e.create("my-artifact")
	chunks := []string{"0123456789abcdef", "ghijklmnopqrstuv", "wxyz"}

	// Blocks arrive concurrently and in any order, and one is never listed.
	var wg sync.WaitGroup
	for i := len(chunks) - 1; i >= 0; i-- {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c := putBlock(t, u, azureID(uuidPrefix, i), []byte(chunks[i])); c != 201 {
				t.Errorf("Put Block %d = %d", i, c)
			}
		}()
	}
	wg.Wait()
	if c := putBlock(t, u, azureID(uuidPrefix, 3), []byte("never committed")); c != 201 {
		t.Fatalf("Put Block 3 = %d", c)
	}
	if e.svc.byID[1].data.Mode() != azblob.ModeDirect {
		t.Fatal("toolkit-style ids did not take the direct path")
	}
	if c, code := putList(t, u, azureID(uuidPrefix, 0), azureID(uuidPrefix, 1), azureID(uuidPrefix, 2)); c != 201 {
		t.Fatalf("Put Block List = %d %s", c, code)
	}

	// Not visible until finalized.
	if c, _ := e.download("my-artifact"); c != 404 {
		t.Fatalf("download before finalize = %d", c)
	}
	whole := strings.Join(chunks, "")
	if c, out := e.finalize("my-artifact", len(whole), "sha256:abc"); c != 200 || out["artifact_id"] != "1" {
		t.Fatalf("FinalizeArtifact = %d %v", c, out)
	}
	if c, body := e.download("my-artifact"); c != 200 || body != whole {
		t.Fatalf("download = %d %q, want %q", c, body, whole)
	}

	c, out := e.rpc("ListArtifacts", e.tok, e.ids())
	arts := out["artifacts"].([]any)
	if c != 200 || len(arts) != 1 {
		t.Fatalf("ListArtifacts = %d %v", c, out)
	}
	a := arts[0].(map[string]any)
	if a["name"] != "my-artifact" || a["database_id"] != "1" || a["size"] != fmt.Sprint(len(whole)) || a["digest"] != "sha256:abc" {
		t.Errorf("listed %v", a)
	}
	c, out = e.rpc("ListArtifacts", e.tok, e.with(map[string]any{"name_filter": "other"}))
	if c != 200 || len(out["artifacts"].([]any)) != 0 {
		t.Errorf("name filter: %d %v", c, out)
	}
	c, out = e.rpc("ListArtifacts", e.tok, e.with(map[string]any{"id_filter": "1"}))
	if c != 200 || len(out["artifacts"].([]any)) != 1 {
		t.Errorf("id filter: %d %v", c, out)
	}
}

// A server that also listens on another address (a docker network's gateway,
// for a job container) hands out links to the address each request came in on.
func TestLinksFollowTheListener(t *testing.T) {
	e := newEnv(t)
	other := httptest.NewUnstartedServer(e.srv.Config.Handler)
	other.Config.BaseContext = func(net.Listener) context.Context {
		return blob.WithBaseURL(context.Background(), other.URL)
	}
	other.Start()
	t.Cleanup(other.Close)

	code, out := e.rpcAt(other.URL, "CreateArtifact", e.tok, e.with(map[string]any{"name": "a", "version": 4}))
	up, _ := out["signed_upload_url"].(string)
	if code != 200 || !strings.HasPrefix(up, other.URL+"/") {
		t.Fatalf("CreateArtifact on the other address = %d %v", code, out)
	}
	id := azureID(uuidPrefix, 0)
	if c := putBlock(t, up, id, []byte("payload")); c != 201 {
		t.Fatalf("Put Block = %d", c)
	}
	if c, _ := putList(t, up, id); c != 201 {
		t.Fatalf("Put Block List = %d", c)
	}
	if c, out := e.finalize("a", 7, ""); c != 200 {
		t.Fatalf("FinalizeArtifact = %d %v", c, out)
	}
	for _, base := range []string{other.URL, e.srv.URL} {
		_, out := e.rpcAt(base, "GetSignedArtifactURL", e.tok, e.with(map[string]any{"name": "a"}))
		if u, _ := out["signed_url"].(string); !strings.HasPrefix(u, base+"/") {
			t.Errorf("asked at %s, got link %q", base, u)
		}
	}
	if c, body := e.download("a"); c != 200 || body != "payload" {
		t.Errorf("download = %d %q", c, body)
	}
}

func TestSpoolUploadForOtherClients(t *testing.T) {
	e := newEnv(t)
	u := e.create("odd")
	// Ids that are not the toolkit's pattern, uploaded out of order; sizes
	// do not line up with partSize (16).
	blocks := map[string]string{"QUFB": "hello, ", "QkJC": "spooled ", "Q0ND": "world"}
	for id, data := range blocks {
		if c := putBlock(t, u, id, []byte(data)); c != 201 {
			t.Fatalf("Put Block %s = %d", id, c)
		}
	}
	if e.svc.byID[1].data.Mode() != azblob.ModeSpool {
		t.Fatal("other ids did not take the spool path")
	}
	if c, code := putList(t, u, "QUFB", "QkJC", "Q0ND"); c != 201 {
		t.Fatalf("Put Block List = %d %s", c, code)
	}
	want := "hello, spooled world"
	if c, _ := e.finalize("odd", len(want), ""); c != 200 {
		t.Fatalf("finalize = %d", c)
	}
	if c, body := e.download("odd"); c != 200 || body != want {
		t.Fatalf("download = %d %q", c, body)
	}
	if sp := e.svc.byID[1].data.SpoolDir(); sp == "" {
		t.Error("spool path was not used")
	} else if _, err := os.Stat(sp); !os.IsNotExist(err) {
		t.Errorf("spool directory left behind after commit: %v", err)
	}
}

func TestInvalidBlockLists(t *testing.T) {
	e := newEnv(t)
	u := e.create("a")
	putBlock(t, u, azureID(uuidPrefix, 0), []byte("0123456789abcdef"))
	putBlock(t, u, azureID(uuidPrefix, 2), []byte("x"))

	for name, ids := range map[string][]string{
		"empty":            {},
		"gap":              {azureID(uuidPrefix, 0), azureID(uuidPrefix, 2)},
		"wrong order":      {azureID(uuidPrefix, 2), azureID(uuidPrefix, 0)},
		"not uploaded":     {azureID(uuidPrefix, 0), azureID(uuidPrefix, 1)},
		"another prefix":   {azureID("ffffffff-4b5a-6978-8796-a5b4c3d2e1f0", 0)},
		"starts at one":    {azureID(uuidPrefix, 2)},
		"not a block id":   {"QUFB"},
		"duplicate number": {azureID(uuidPrefix, 0), azureID(uuidPrefix, 0)},
	} {
		if c, code := putList(t, u, ids...); c != 400 || code != "InvalidBlockList" {
			t.Errorf("%s: %d %s, want 400 InvalidBlockList", name, c, code)
		}
	}
	// None of that committed anything; the right list still works.
	if c, code := putList(t, u, azureID(uuidPrefix, 0)); c != 201 {
		t.Fatalf("valid list after bad ones = %d %s", c, code)
	}
	// A repeat of the committed request is accepted, anything else is not.
	if c, _ := putList(t, u, azureID(uuidPrefix, 0)); c != 201 {
		t.Errorf("retried Put Block List = %d", c)
	}
	if c := putBlock(t, u, azureID(uuidPrefix, 1), []byte("late")); c != 409 {
		t.Errorf("Put Block after commit = %d, want 409", c)
	}
}

func TestMixedBlockIDs(t *testing.T) {
	e := newEnv(t)
	u := e.create("a")
	putBlock(t, u, azureID(uuidPrefix, 0), []byte("x"))
	if c := putBlock(t, u, "QUFB", []byte("y")); c != 400 {
		t.Errorf("odd id in a direct upload = %d, want 400", c)
	}
	if c := putBlock(t, u, azureID("ffffffff-4b5a-6978-8796-a5b4c3d2e1f0", 1), []byte("y")); c != 400 {
		t.Errorf("another prefix = %d, want 400", c)
	}
}

func TestUploadURLChecks(t *testing.T) {
	e := newEnv(t)
	u := e.create("a")
	id := azureID(uuidPrefix, 0)
	for name, bad := range map[string]string{
		"tampered signature": u[:len(u)-2] + "xx",
		"another artifact":   strings.Replace(u, "/1?", "/2?", 1),
		"another run":        strings.Replace(u, "/run-1/", "/run-2/", 1),
		"longer expiry":      strings.Replace(u, "exp=", "exp=9", 1),
		"no signature":       u[:strings.Index(u, "&sig=")],
	} {
		if c := putBlock(t, bad, id, []byte("x")); c != 403 && c != 404 {
			t.Errorf("%s: %d, want 403 or 404", name, c)
		}
	}
	e.signer.now = func() time.Time { return time.Now().Add(7 * time.Hour) }
	if c := putBlock(t, u, id, []byte("x")); c != 403 {
		t.Errorf("expired URL: %d", c)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	body := e.with(map[string]any{"name": "a"})
	if c, _ := e.rpc("CreateArtifact", "", body); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c, _ := e.rpc("CreateArtifact", e.tok+"x", body); c != 401 {
		t.Errorf("bad signature: %d", c)
	}
	other, _ := e.signer.Token(Scope{Run: "run-2", Job: "job-1"}, time.Hour)
	if c, out := e.rpc("CreateArtifact", other, body); c != 403 {
		t.Errorf("token for another run: %d %v", c, out)
	}
	otherJob, _ := e.signer.Token(Scope{Run: "run-1", Job: "job-2"}, time.Hour)
	if c, _ := e.rpc("CreateArtifact", otherJob, body); c != 403 {
		t.Errorf("token for another job creating: %d", c)
	}
	// Any job of the run may read.
	if c, _ := e.rpc("ListArtifacts", otherJob, e.ids()); c != 200 {
		t.Errorf("another job listing: %d", c)
	}
	expired, _ := func() (string, error) {
		s, _ := NewSigner([]byte("0123456789abcdef0123"))
		s.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
		return s.Token(e.sc, time.Hour)
	}()
	if c, _ := e.rpc("ListArtifacts", expired, e.ids()); c != 401 {
		t.Errorf("expired token: %d", c)
	}
	// Another run's artifacts are invisible even by name.
	u := e.create("secret")
	putBlock(t, u, azureID(uuidPrefix, 0), []byte("x"))
	putList(t, u, azureID(uuidPrefix, 0))
	e.finalize("secret", 1, "")
	tok2, _ := e.signer.Token(Scope{Run: "run-2", Job: "job-1"}, time.Hour)
	ids2 := map[string]any{"workflow_run_backend_id": "run-2", "workflow_job_run_backend_id": "job-1", "name": "secret"}
	if c, _ := e.rpc("GetSignedArtifactURL", tok2, ids2); c != 404 {
		t.Errorf("another run fetching by name: %d", c)
	}
	// A JSON token that claims to need no signature.
	none := b64.EncodeToString([]byte(`{"alg":"none"}`)) + "." + strings.Split(e.tok, ".")[1] + "."
	if c, _ := e.rpc("ListArtifacts", none, e.ids()); c != 401 {
		t.Errorf("alg none: %d", c)
	}
}

func TestDuplicateAndRetry(t *testing.T) {
	e := newEnv(t)
	u := e.create("dup")
	putBlock(t, u, azureID(uuidPrefix, 0), []byte("old"))
	// The client retries from the start before finishing: the first
	// attempt is replaced, not a conflict.
	u2 := e.create("dup")
	if u2 == u {
		t.Fatal("retry got the same upload URL")
	}
	if c := putBlock(t, u, azureID(uuidPrefix, 1), []byte("x")); c != 404 && c != 409 {
		t.Errorf("the replaced upload still accepts blocks: %d", c)
	}
	putBlock(t, u2, azureID(uuidPrefix, 0), []byte("new"))
	putList(t, u2, azureID(uuidPrefix, 0))
	if c, _ := e.finalize("dup", 3, ""); c != 200 {
		t.Fatalf("finalize = %d", c)
	}
	// Once finalized, the name is taken, the way v4 upload-artifact expects.
	code, out := e.rpc("CreateArtifact", e.tok, e.with(map[string]any{"name": "dup"}))
	if code != 409 || out["code"] != "already_exists" {
		t.Errorf("duplicate name: %d %v", code, out)
	}
	if c, body := e.download("dup"); c != 200 || body != "new" {
		t.Errorf("download = %d %q", c, body)
	}
}

func TestFinalizeChecks(t *testing.T) {
	e := newEnv(t)
	if c, _ := e.finalize("nope", 1, ""); c != 404 {
		t.Errorf("unknown artifact: %d", c)
	}
	u := e.create("a")
	if c, out := e.finalize("a", 1, ""); c != 412 {
		t.Errorf("finalize without content: %d %v", c, out)
	}
	putBlock(t, u, azureID(uuidPrefix, 0), []byte("12345"))
	putList(t, u, azureID(uuidPrefix, 0))
	if c, out := e.finalize("a", 99, ""); c != 412 {
		t.Errorf("wrong size: %d %v", c, out)
	}
	if c, _ := e.finalize("a", 5, ""); c != 200 {
		t.Errorf("right size: %d", c)
	}
	if c, _ := e.finalize("a", 5, ""); c != 200 {
		t.Errorf("repeated finalize: %d", c)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	u := e.create("gone")
	putBlock(t, u, azureID(uuidPrefix, 0), []byte("x"))
	putList(t, u, azureID(uuidPrefix, 0))
	e.finalize("gone", 1, "")
	if c, out := e.rpc("DeleteArtifact", e.tok, e.with(map[string]any{"name": "gone"})); c != 200 || out["artifact_id"] != "1" {
		t.Fatalf("DeleteArtifact = %d %v", c, out)
	}
	if c, _ := e.download("gone"); c != 404 {
		t.Errorf("download after delete: %d", c)
	}
	if c, _ := e.rpc("DeleteArtifact", e.tok, e.with(map[string]any{"name": "gone"})); c != 404 {
		t.Errorf("second delete: %d", c)
	}
	// The name is free again.
	e.create("gone")
}

func TestRequestShapes(t *testing.T) {
	e := newEnv(t)
	// camelCase names, as a protobuf-JSON client may send them.
	code, out := e.rpc("CreateArtifact", e.tok, map[string]any{
		"workflowRunBackendId": e.sc.Run, "workflowJobRunBackendId": e.sc.Job,
		"name": "camel", "expiresAt": "2030-01-01T00:00:00Z",
	})
	if code != 200 {
		t.Fatalf("camelCase request: %d %v", code, out)
	}
	if c, out := e.rpc("CreateArtifact", e.tok, e.with(map[string]any{"name": "x", "expires_at": "yesterday"})); c != 400 {
		t.Errorf("bad expiry: %d %v", c, out)
	}
	if c, _ := e.rpc("CreateArtifact", e.tok, e.with(map[string]any{"name": ""})); c != 400 {
		t.Errorf("empty name: %d", c)
	}
	if c, _ := e.rpc("CreateArtifact", e.tok, e.with(map[string]any{"name": strings.Repeat("n", 256)})); c != 400 {
		t.Errorf("long name: %d", c)
	}
	if c, out := e.rpc("Nope", e.tok, e.ids()); c != 404 || out["code"] != "bad_route" {
		t.Errorf("unknown method: %d %v", c, out)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+twirpPrefix+"ListArtifacts", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+e.tok)
	req.Header.Set("Content-Type", "application/protobuf")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("protobuf content type: %d", resp.StatusCode)
	}
}

func TestTokenIsReadableByToolkit(t *testing.T) {
	e := newEnv(t)
	// @actions/artifact splits the scp claim of the (unverified) JWT.
	raw, _ := b64.DecodeString(strings.Split(e.tok, ".")[1])
	var c struct{ Scp string }
	json.Unmarshal(raw, &c)
	if c.Scp != "Actions.Results:run-1:job-1" {
		t.Errorf("scp = %q", c.Scp)
	}
	if _, err := e.signer.Token(Scope{Run: "a:b", Job: "j"}, time.Hour); err == nil {
		t.Error("run id with a colon accepted")
	}
	env, err := e.svc.Env(e.sc, time.Hour)
	if err != nil || env["ACTIONS_RESULTS_URL"] != e.srv.URL+"/" || env["ACTIONS_RUNTIME_TOKEN"] == "" {
		t.Errorf("Env = %v, %v", env, err)
	}
}

func TestCloseAbortsUnfinished(t *testing.T) {
	e := newEnv(t)
	u := e.create("half")
	putBlock(t, u, azureID(uuidPrefix, 0), []byte("x"))
	e.svc.Close()
	if !e.svc.byID[1].gone {
		t.Error("an unfinished artifact survived Close")
	}
}
