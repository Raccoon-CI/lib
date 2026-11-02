package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/artifact"
	"github.com/Raccoon-CI/raccoonci/pkg/blob"
)

type env struct {
	t      *testing.T
	svc    *Service
	signer *artifact.Signer
	store  *blob.Dir
	srv    *httptest.Server
	tok    string
	index  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return start(t, t.TempDir(), false)
}

// start brings up a service on dir. Passing a previous env's dir restarts it.
// advertiseV2 makes Env point clients at the v2 protocol.
func start(t *testing.T, dir string, advertiseV2 bool) *env {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	store, err := blob.NewDir(filepath.Join(dir, "objects"), srv.URL, []byte("0123456789abcdef0123"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	signer, _ := artifact.NewSigner([]byte("0123456789abcdef0123"))
	index := filepath.Join(dir, "index.json")
	svc, err := New(Config{Store: store, Signer: signer, BaseURL: srv.URL, IndexPath: index, AdvertiseV2: advertiseV2})
	if err != nil {
		t.Fatal(err)
	}
	svc.partSize = 16
	t.Cleanup(svc.Close)
	mux.Handle(blob.DownloadPrefix, store.Handler())
	mux.Handle("/", svc.Handler())
	tok, _ := signer.Token(artifact.Scope{Run: "r1", Job: "j1"}, time.Hour)
	return &env{t: t, svc: svc, signer: signer, store: store, srv: srv, tok: tok, index: index}
}

func (e *env) do(method, path, token string, hdr map[string]string, body io.Reader) (*http.Response, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+apiPrefix+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) reserve(key, version string) (int, int64) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"key": key, "version": version})
	resp, body := e.do("POST", "caches", e.tok, map[string]string{"Content-Type": "application/json"}, bytes.NewReader(b))
	var out struct{ CacheID int64 }
	json.Unmarshal(body, &out)
	if resp.StatusCode == 201 {
		var o2 struct {
			CacheId int64 `json:"cacheId"`
		}
		json.Unmarshal(body, &o2)
		return resp.StatusCode, o2.CacheId
	}
	return resp.StatusCode, 0
}

// streamBody hides the length, so the request goes out chunked, like the
// toolkit's streamed PATCH.
type streamBody struct{ io.Reader }

func (e *env) patch(id int64, start int, data string) int {
	e.t.Helper()
	resp, _ := e.do("PATCH", fmt.Sprintf("caches/%d", id), e.tok,
		map[string]string{"Content-Type": "application/octet-stream",
			"Content-Range": fmt.Sprintf("bytes %d-%d/*", start, start+len(data)-1)},
		streamBody{strings.NewReader(data)})
	return resp.StatusCode
}

func (e *env) commit(id int64, size int) (int, string) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]int{"size": size})
	resp, body := e.do("POST", fmt.Sprintf("caches/%d", id), e.tok, map[string]string{"Content-Type": "application/json"}, bytes.NewReader(b))
	return resp.StatusCode, string(body)
}

// save stores data under key and version in chunks of the given size.
func (e *env) save(key, version, data string, chunk int) int64 {
	e.t.Helper()
	code, id := e.reserve(key, version)
	if code != 201 {
		e.t.Fatalf("reserve %q = %d", key, code)
	}
	for off := 0; off < len(data); off += chunk {
		if c := e.patch(id, off, data[off:min(off+chunk, len(data))]); c != 204 {
			e.t.Fatalf("patch at %d = %d", off, c)
		}
	}
	if c, body := e.commit(id, len(data)); c != 204 {
		e.t.Fatalf("commit = %d %s", c, body)
	}
	return id
}

type hit struct {
	Key, Version, Location string
}

func (e *env) lookup(version string, keys ...string) (*hit, int) {
	e.t.Helper()
	resp, body := e.do("GET", "cache?keys="+strings.Join(keys, ",")+"&version="+version, e.tok, nil, nil)
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode
	}
	var m map[string]string
	json.Unmarshal(body, &m)
	return &hit{m["cacheKey"], m["cacheVersion"], m["archiveLocation"]}, 200
}

func fetch(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s = %d", u, resp.StatusCode)
	}
	return string(b)
}

func TestSaveAndRestore(t *testing.T) {
	e := newEnv(t)
	data := "the quick brown fox jumps over the lazy dog, twice over"
	if h, c := e.lookup("v1", "deps-1"); h != nil || c != 204 {
		t.Fatalf("lookup before save = %v %d, want a miss", h, c)
	}
	// The toolkit uploads in parallel, so chunks arrive in any order.
	code, id := e.reserve("deps-1", "v1")
	if code != 201 {
		t.Fatal(code)
	}
	var wg sync.WaitGroup
	for off := len(data) - 1; off >= 0; off -= 10 {
		start := off - off%10
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c := e.patch(id, start, data[start:min(start+10, len(data))]); c != 204 {
				t.Errorf("patch %d = %d", start, c)
			}
		}()
	}
	wg.Wait()
	if h, _ := e.lookup("v1", "deps-1"); h != nil {
		t.Fatal("entry visible before commit")
	}
	if c, body := e.commit(id, len(data)); c != 204 {
		t.Fatalf("commit = %d %s", c, body)
	}
	h, c := e.lookup("v1", "deps-1")
	if h == nil || h.Key != "deps-1" || h.Version != "v1" {
		t.Fatalf("lookup = %+v %d", h, c)
	}
	if got := fetch(t, h.Location); got != data {
		t.Fatalf("restored %q", got)
	}
	// Another version is another entry.
	if h, c := e.lookup("v2", "deps-1"); h != nil || c != 204 {
		t.Errorf("wrong version hit: %v %d", h, c)
	}
	if c, _ := e.commit(id, len(data)); c != 204 {
		t.Errorf("a retried commit = %d, want 204", c)
	}
}

func TestRestoreKeys(t *testing.T) {
	e := newEnv(t)
	e.save("npm-linux-aaa", "v", "old", 8)
	time.Sleep(2 * time.Millisecond)
	e.save("npm-linux-bbb", "v", "new", 8)
	e.save("npm-mac-ccc", "v", "mac", 8)
	e.save("other", "w", "other-version", 8)

	pick := func(keys ...string) string {
		h, _ := e.lookup("v", keys...)
		if h == nil {
			return ""
		}
		return h.Key
	}
	if k := pick("npm-linux-aaa", "npm-"); k != "npm-linux-aaa" {
		t.Errorf("exact key lost to a prefix: %q", k)
	}
	if k := pick("npm-linux-"); k != "npm-linux-bbb" {
		t.Errorf("prefix must pick the newest, got %q", k)
	}
	if k := pick("npm-mac-", "npm-linux-"); k != "npm-mac-ccc" {
		t.Errorf("earlier key must win: %q", k)
	}
	if k := pick("nothing-", "npm-linux-a"); k != "npm-linux-aaa" {
		t.Errorf("a miss must fall through to the next key: %q", k)
	}
	if k := pick("nothing-"); k != "" {
		t.Errorf("hit on %q", k)
	}
	if k := pick("other"); k != "" {
		t.Errorf("an entry of another version matched: %q", k)
	}
}

func TestReserveConflicts(t *testing.T) {
	e := newEnv(t)
	c, first := e.reserve("k", "v")
	if c != 201 {
		t.Fatal(c)
	}
	if c, _ := e.reserve("k", "v"); c != 409 {
		t.Errorf("second reservation while one is open = %d, want 409", c)
	}
	if c, _ := e.reserve("k", "other"); c != 201 {
		t.Errorf("same key, other version = %d", c)
	}
	// A reservation nobody finished is replaced once it is stale.
	e.svc.now = func() time.Time { return time.Now().Add(2 * reserveTTL) }
	c, id := e.reserve("k", "v")
	if c != 201 || id == first {
		t.Fatalf("reserve after the TTL = %d, id %d (first was %d)", c, id, first)
	}
	if code := e.patch(first, 0, "x"); code != 404 {
		t.Errorf("the replaced reservation still takes chunks: %d", code)
	}
	e.svc.now = time.Now

	// Entries are immutable once committed.
	e.patch(id, 0, "data")
	if c, _ := e.commit(id, 4); c != 204 {
		t.Fatal(c)
	}
	if c, _ := e.reserve("k", "v"); c != 409 {
		t.Errorf("reserving a committed entry = %d, want 409", c)
	}
}

func TestCommitChecks(t *testing.T) {
	e := newEnv(t)
	_, id := e.reserve("k", "v")
	if c, _ := e.commit(id, 4); c != 400 {
		t.Errorf("commit without chunks = %d", c)
	}
	e.patch(id, 0, "aaaa")
	e.patch(id, 8, "cccc")
	if c, body := e.commit(id, 12); c != 400 || !strings.Contains(body, "gap at byte 4") {
		t.Errorf("gap: %d %s", c, body)
	}
	e.patch(id, 4, "bbbb")
	if c, body := e.commit(id, 99); c != 400 || !strings.Contains(body, "12 bytes") {
		t.Errorf("wrong size: %d %s", c, body)
	}
	e.patch(id, 2, "xxxx")
	if c, body := e.commit(id, 12); c != 400 || !strings.Contains(body, "overlap") {
		t.Errorf("overlap: %d %s", c, body)
	}
	if c, _ := e.commit(12345, 1); c != 404 {
		t.Errorf("unknown id = %d", c)
	}
}

func TestChunkChecks(t *testing.T) {
	e := newEnv(t)
	_, id := e.reserve("k", "v")
	path := fmt.Sprintf("caches/%d", id)
	patch := func(rng, body string) int {
		resp, _ := e.do("PATCH", path, e.tok, map[string]string{"Content-Range": rng}, streamBody{strings.NewReader(body)})
		return resp.StatusCode
	}
	for name, c := range map[string][2]string{
		"no range":        {"", "abcd"},
		"not bytes":       {"items 0-3/*", "abcd"},
		"backwards":       {"bytes 5-2/*", "abcd"},
		"garbage":         {"bytes a-b/*", "abcd"},
		"short body":      {"bytes 0-9/*", "abcd"},
		"long body":       {"bytes 0-1/*", "abcd"},
		"over the limit":  {fmt.Sprintf("bytes %d-%d/*", int64(maxEntrySize), int64(maxEntrySize)+3), "abcd"},
		"oversized chunk": {fmt.Sprintf("bytes 0-%d/*", int64(maxChunkSize)), "abcd"},
	} {
		if got := patch(c[0], c[1]); got != 400 {
			t.Errorf("%s: %d, want 400", name, got)
		}
	}
	if c := e.patch(999, 0, "x"); c != 404 {
		t.Errorf("unknown reservation: %d", c)
	}
	// None of that left a chunk behind.
	if c, _ := e.commit(id, 4); c != 400 {
		t.Errorf("commit after rejected chunks = %d", c)
	}
	// A retried chunk replaces the first attempt.
	e.patch(id, 0, "aaaa")
	e.patch(id, 0, "bbbb")
	if c, _ := e.commit(id, 4); c != 204 {
		t.Fatal(c)
	}
	h, _ := e.lookup("v", "k")
	if got := fetch(t, h.Location); got != "bbbb" {
		t.Errorf("restored %q, want the retried chunk", got)
	}
}

func TestLargeEntryIsCutIntoParts(t *testing.T) {
	e := newEnv(t)
	data := strings.Repeat("0123456789", 11) // 110 bytes: 7 parts of 16
	e.save("big", "v", data, 25)
	h, _ := e.lookup("v", "big")
	if got := fetch(t, h.Location); got != data {
		t.Errorf("restored %q", got)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	b, _ := json.Marshal(map[string]string{"key": "k", "version": "v"})
	for name, tok := range map[string]string{"none": "", "garbage": "x.y.z", "tampered": e.tok + "x"} {
		resp, _ := e.do("POST", "caches", tok, nil, bytes.NewReader(b))
		if resp.StatusCode != 401 {
			t.Errorf("%s token: %d", name, resp.StatusCode)
		}
	}
	expired, _ := e.signer.Token(artifact.Scope{Run: "r1", Job: "j1"}, -time.Hour)
	if resp, _ := e.do("GET", "cache?keys=k&version=v", expired, nil, nil); resp.StatusCode != 401 {
		t.Errorf("expired token: %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "cache?keys=k&version=v", "", nil, nil); resp.StatusCode != 401 {
		t.Errorf("lookup without a token: %d", resp.StatusCode)
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t)
	get := func(q string) int {
		resp, _ := e.do("GET", "cache?"+q, e.tok, nil, nil)
		return resp.StatusCode
	}
	for name, q := range map[string]string{
		"no version": "keys=a",
		"no keys":    "version=v",
		"empty keys": "keys=,&version=v",
		"too many":   "keys=1,2,3,4,5,6,7,8,9,10,11&version=v",
		"long key":   "keys=" + strings.Repeat("k", maxKeyLen+1) + "&version=v",
	} {
		if c := get(q); c != 400 {
			t.Errorf("%s: %d, want 400", name, c)
		}
	}
	post := func(body string) int {
		resp, _ := e.do("POST", "caches", e.tok, nil, strings.NewReader(body))
		return resp.StatusCode
	}
	for name, body := range map[string]string{
		"not json":     "{",
		"no key":       `{"version":"v"}`,
		"no version":   `{"key":"k"}`,
		"too large":    fmt.Sprintf(`{"key":"k","version":"v","cacheSize":%d}`, int64(maxEntrySize)+1),
		"key too long": `{"version":"v","key":"` + strings.Repeat("k", maxKeyLen+1) + `"}`,
	} {
		if c := post(body); c != 400 {
			t.Errorf("reserve, %s: %d, want 400", name, c)
		}
	}
}

func TestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	e := start(t, dir, false)
	e.save("persisted", "v", "kept across runs", 8)
	e.svc.Close()
	e.srv.Close()

	e2 := start(t, dir, false)
	h, c := e2.lookup("v", "persisted")
	if h == nil {
		t.Fatalf("entry lost over a restart: %d", c)
	}
	if got := fetch(t, h.Location); got != "kept across runs" {
		t.Errorf("restored %q", got)
	}
	// New ids do not collide with old ones.
	id := e2.save("second", "v", "more", 8)
	if id <= 1 {
		t.Errorf("reused id %d", id)
	}
}

func TestEntryWhoseObjectIsGone(t *testing.T) {
	e := newEnv(t)
	id := e.save("k", "v", "data", 8)
	if err := e.store.Delete(context.Background(), objectKey(id)); err != nil {
		t.Fatal(err)
	}
	if h, c := e.lookup("v", "k"); h != nil || c != 204 {
		t.Fatalf("lookup of a vanished object = %v %d, want a miss", h, c)
	}
	// The stale entry is dropped, so the key can be saved again.
	e.save("k", "v", "again", 8)
	h, _ := e.lookup("v", "k")
	if got := fetch(t, h.Location); got != "again" {
		t.Errorf("restored %q", got)
	}
	b, _ := os.ReadFile(e.index)
	if strings.Count(string(b), `"key":"k"`) != 1 {
		t.Errorf("index = %s", b)
	}
}

func TestCorruptIndexIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, _ := artifact.NewSigner([]byte("0123456789abcdef0123"))
	store, _ := blob.NewDir(filepath.Join(dir, "objects"), "http://x", []byte("0123456789abcdef0123"))
	defer store.Close()
	_, err := New(Config{Store: store, Signer: signer, BaseURL: "http://x", IndexPath: filepath.Join(dir, "index.json")})
	if err == nil {
		t.Fatal("a corrupt index was accepted")
	}
}

func TestListAndEnv(t *testing.T) {
	e := newEnv(t)
	e.save("abc-1", "v", "x", 8)
	e.save("abd-2", "v", "y", 8)
	resp, body := e.do("GET", "caches?key=ab", e.tok, nil, nil)
	var out struct{ TotalCount int }
	json.Unmarshal(body, &out)
	if resp.StatusCode != 200 || out.TotalCount != 2 {
		t.Errorf("list = %d %s", resp.StatusCode, body)
	}
	env, err := e.svc.Env(artifact.Scope{Run: "r", Job: "j"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if env["ACTIONS_CACHE_URL"] != e.srv.URL+"/" || env["ACTIONS_RUNTIME_TOKEN"] == "" {
		t.Errorf("env = %v", env)
	}
	if v, ok := env["ACTIONS_CACHE_SERVICE_V2"]; !ok || v != "" {
		t.Errorf("ACTIONS_CACHE_SERVICE_V2 must be set to empty, got %q (set: %v)", v, ok)
	}
}
