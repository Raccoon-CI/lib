package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memIndex is an Index in memory that outlives the Service using it.
type memIndex struct {
	mu      sync.Mutex
	recs    map[int64]Record
	failing bool
}

func newMemIndex() *memIndex { return &memIndex{recs: map[int64]Record{}} }

func (m *memIndex) Load(context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.recs {
		out = append(out, r)
	}
	return out, nil
}

func (m *memIndex) Save(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing {
		return errors.New("disk full")
	}
	m.recs[r.ID] = r
	return nil
}

func (m *memIndex) Delete(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing {
		return errors.New("disk full")
	}
	delete(m.recs, id)
	return nil
}

func (m *memIndex) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.recs)
}

// upload makes a finished one-block artifact.
func (e *env) upload(name, content string) {
	e.t.Helper()
	u := e.create(name)
	if c := putBlock(e.t, u, azureID(uuidPrefix, 0), []byte(content)); c != 201 {
		e.t.Fatalf("PutBlock = %d", c)
	}
	if c, _ := putList(e.t, u, azureID(uuidPrefix, 0)); c != 201 {
		e.t.Fatalf("PutBlockList = %d", c)
	}
	if c, out := e.finalize(name, len(content), "sha256:abc"); c != 200 {
		e.t.Fatalf("FinalizeArtifact = %d %v", c, out)
	}
}

func TestFinishedArtifactsSurviveARestartWithAnIndex(t *testing.T) {
	idx := newMemIndex()
	e := newEnvWith(t, func(c *Config) { c.Index = idx })
	e.upload("first", "hello")
	e.upload("second", "world!")
	if idx.count() != 2 {
		t.Fatalf("%d records, want 2", idx.count())
	}
	// An upload that never finished is not remembered.
	e.create("unfinished")
	if idx.count() != 2 {
		t.Fatalf("%d records: an unfinished upload must not be remembered", idx.count())
	}

	// A new process: a new Service on the same store and index.
	svc2, err := New(Config{Store: e.dir, Signer: e.signer, BaseURL: e.srv.URL, Index: idx})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	infos := svc2.Artifacts(e.sc.Run)
	if len(infos) != 2 || infos[0].Name != "first" || infos[0].Size != 5 || infos[0].Digest != "sha256:abc" || infos[0].Job != "job-1" || infos[1].Name != "second" {
		t.Fatalf("after restart: %+v", infos)
	}
	if got := svc2.Artifacts("another-run"); len(got) != 0 {
		t.Fatalf("another run sees %+v", got)
	}
	u, err := svc2.DownloadURL(context.Background(), e.sc.Run, infos[1].ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "world!" {
		t.Fatalf("downloaded %q", b)
	}
	if _, err := svc2.DownloadURL(context.Background(), "another-run", infos[1].ID, time.Minute); !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("an artifact of another run: err = %v", err)
	}

}

func TestNoNewArtifactIsStartedOnANearlyFullDiskAndOneIsWhenSpaceReturns(t *testing.T) {
	free := int64(5 << 20)
	e := newEnvWith(t, func(c *Config) {
		c.MinFree = 100 << 20
		c.Free = func() (int64, error) { return free, nil }
	})
	code, out := e.rpc("CreateArtifact", e.tok, e.with(map[string]any{"name": "a"}))
	if code != 429 || !strings.Contains(fmt.Sprint(out["msg"]), "nearly full") {
		t.Fatalf("on a full disk: %d %v", code, out)
	}
	free = 1 << 30
	if code, out := e.rpc("CreateArtifact", e.tok, e.with(map[string]any{"name": "a"})); code != 200 {
		t.Fatalf("with space: %d %v", code, out)
	}
}

func TestARestartedServiceKeepsTheNamesTakenAndTheNumbering(t *testing.T) {
	idx := newMemIndex()
	e := newEnvWith(t, func(c *Config) { c.Index = idx })
	e.upload("kept", "abc")
	svc2, err := New(Config{Store: e.dir, Signer: e.signer, BaseURL: e.srv.URL, Index: idx})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	// The test server routes to the old service; serve the new one on its own.
	srv2 := httptest.NewServer(svc2.Handler())
	defer srv2.Close()
	if c, out := e.rpcAt(srv2.URL, "CreateArtifact", e.tok, e.with(map[string]any{"name": "kept"})); c != 409 {
		t.Fatalf("a name finished before the restart must stay taken; CreateArtifact = %d %v", c, out)
	}
	c, out := e.rpcAt(srv2.URL, "CreateArtifact", e.tok, e.with(map[string]any{"name": "fresh"}))
	if c != 200 {
		t.Fatalf("CreateArtifact = %d %v", c, out)
	}
	if infos := svc2.Artifacts(e.sc.Run); len(infos) != 1 {
		t.Fatalf("infos = %+v", infos)
	}
	svc2.mu.Lock()
	fresh := svc2.byName[key(e.sc.Run, "fresh")]
	svc2.mu.Unlock()
	if fresh == nil || fresh.id != 2 {
		t.Fatalf("the new artifact must be number 2, got %+v", fresh)
	}
}

func TestDeleteForgetsTheRecordAndTheData(t *testing.T) {
	idx := newMemIndex()
	e := newEnvWith(t, func(c *Config) { c.Index = idx })
	e.upload("doomed", "bye")
	if c, _ := e.rpc("DeleteArtifact", e.tok, e.with(map[string]any{"name": "doomed"})); c != 200 {
		t.Fatalf("DeleteArtifact = %d", c)
	}
	if idx.count() != 0 {
		t.Fatal("the record must go with the artifact")
	}
	// If forgetting the record fails, nothing is lost.
	e.upload("kept", "stay")
	idx.failing = true
	if c, _ := e.rpc("DeleteArtifact", e.tok, e.with(map[string]any{"name": "kept"})); c != 500 {
		t.Fatalf("DeleteArtifact with a failing index = %d, want an error", c)
	}
	idx.failing = false
	if c, body := e.download("kept"); c != 200 || body != "stay" {
		t.Fatalf("the artifact must survive: %d %q", c, body)
	}
}

func TestAFailingIndexStopsFinalize(t *testing.T) {
	idx := newMemIndex()
	e := newEnvWith(t, func(c *Config) { c.Index = idx })
	idx.failing = true
	u := e.create("a")
	putBlock(t, u, azureID(uuidPrefix, 0), []byte("x"))
	putList(t, u, azureID(uuidPrefix, 0))
	if c, _ := e.finalize("a", 1, ""); c != 500 {
		t.Fatalf("FinalizeArtifact with a failing index = %d: an artifact that cannot be remembered must not look finished", c)
	}
	if got := e.svc.Artifacts(e.sc.Run); len(got) != 0 {
		t.Fatalf("listed %+v", got)
	}
}

func TestExpireDeletesWhatIsPastItsTime(t *testing.T) {
	idx := newMemIndex()
	e := newEnvWith(t, func(c *Config) { c.Index = idx })
	soon := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	// A client asks for a time with expires_at; two artifacts, two times.
	code, out := e.rpc("CreateArtifact", e.tok, e.with(map[string]any{"name": "short-lived", "expires_at": soon}))
	if code != 200 {
		t.Fatalf("CreateArtifact = %d %v", code, out)
	}
	url2 := out["signed_upload_url"].(string)
	putBlock(t, url2, azureID(uuidPrefix, 0), []byte("s"))
	putList(t, url2, azureID(uuidPrefix, 0))
	e.finalize("short-lived", 1, "")
	e.upload("long-lived", "l")

	n, err := e.svc.Expire(context.Background(), time.Now())
	if err != nil || n != 0 {
		t.Fatalf("Expire now = %d, %v: nothing is due yet", n, err)
	}
	n, err = e.svc.Expire(context.Background(), time.Now().Add(2*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("Expire in two hours = %d, %v: only the short-lived one", n, err)
	}
	if idx.count() != 1 {
		t.Fatalf("%d records left, want 1", idx.count())
	}
	if c, _ := e.download("short-lived"); c != 404 {
		t.Fatalf("an expired artifact downloads with %d", c)
	}
	if c, body := e.download("long-lived"); c != 200 || body != "l" {
		t.Fatalf("the other one: %d %q", c, body)
	}
	n, _ = e.svc.Expire(context.Background(), time.Now().Add(200*24*time.Hour))
	if n != 1 {
		t.Fatalf("Expire after 200 days = %d, want the last one", n)
	}
}

func TestMaxKeepCapsWhatAClientAsksFor(t *testing.T) {
	e := newEnvWith(t, func(c *Config) { c.MaxKeep = 24 * time.Hour })
	far := time.Now().Add(400 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	code, out := e.rpc("CreateArtifact", e.tok, e.with(map[string]any{"name": "greedy", "expires_at": far}))
	if code != 200 {
		t.Fatalf("CreateArtifact = %d %v", code, out)
	}
	e.svc.mu.Lock()
	a := e.svc.byName[key(e.sc.Run, "greedy")]
	e.svc.mu.Unlock()
	if left := time.Until(a.expires); left > 25*time.Hour || left < 23*time.Hour {
		t.Fatalf("kept for %v, want about 24h", left)
	}
	// Without a request the default is Keep.
	e2 := newEnvWith(t, func(c *Config) { c.Keep = 3 * time.Hour })
	e2.create("plain")
	e2.svc.mu.Lock()
	p := e2.svc.byName[key(e2.sc.Run, "plain")]
	e2.svc.mu.Unlock()
	if left := time.Until(p.expires); left > 4*time.Hour || left < 2*time.Hour {
		t.Fatalf("kept for %v, want about 3h", left)
	}
}
