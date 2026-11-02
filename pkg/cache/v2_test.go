package cache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/artifact"
	"github.com/Raccoon-CI/raccoonci/pkg/blob"
)

const sdkPrefix = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

// blockID builds a block id the way @azure/storage-blob does.
func blockID(i int) string {
	return base64.StdEncoding.EncodeToString([]byte(sdkPrefix + fmt.Sprintf("%012d", i)))
}

var blobHeaders = map[string]string{"x-ms-blob-type": "BlockBlob"}

// v2 calls a method of the Twirp CacheService.
func (e *env) v2(method, token string, body map[string]any) (int, map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.srv.URL+twirpPrefix+method, bytes.NewReader(b))
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

func (e *env) create(key, version string) (ok bool, url string, msg string) {
	e.t.Helper()
	code, out := e.v2("CreateCacheEntry", e.tok, map[string]any{"key": key, "version": version})
	if code != 200 {
		e.t.Fatalf("CreateCacheEntry = %d %v", code, out)
	}
	url, _ = out["signed_upload_url"].(string)
	msg, _ = out["message"].(string)
	return out["ok"] == true, url, msg
}

func (e *env) finalizeV2(key, version string, size int) (bool, string, string) {
	e.t.Helper()
	code, out := e.v2("FinalizeCacheEntryUpload", e.tok, map[string]any{"key": key, "version": version, "size_bytes": fmt.Sprint(size)})
	if code != 200 {
		e.t.Fatalf("FinalizeCacheEntryUpload = %d %v", code, out)
	}
	id, _ := out["entry_id"].(string)
	msg, _ := out["message"].(string)
	return out["ok"] == true, id, msg
}

func (e *env) download(key, version string, restore ...string) (matched, url string) {
	e.t.Helper()
	code, out := e.v2("GetCacheEntryDownloadURL", e.tok, map[string]any{"key": key, "version": version, "restore_keys": restore})
	if code != 200 {
		e.t.Fatalf("GetCacheEntryDownloadURL = %d %v", code, out)
	}
	if out["ok"] != true {
		return "", ""
	}
	return out["matched_key"].(string), out["signed_download_url"].(string)
}

func put(t *testing.T, u, query string, hdr map[string]string, body string) int {
	t.Helper()
	if query != "" {
		u += "&" + query
	}
	req, _ := http.NewRequest("PUT", u, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func blockList(ids ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, id := range ids {
		b.WriteString("<Latest>" + id + "</Latest>")
	}
	return b.String() + "</BlockList>"
}

func TestV2SingleShotUpload(t *testing.T) {
	e := start(t, t.TempDir(), true)
	data := "hello v2 world"
	ok, u, msg := e.create("deps", "ver")
	if !ok || u == "" {
		t.Fatalf("create refused: %q", msg)
	}
	if m, _ := e.download("deps", "ver"); m != "" {
		t.Fatal("entry visible before the upload")
	}
	if c := put(t, u, "", blobHeaders, data); c != 201 {
		t.Fatalf("Put Blob = %d", c)
	}
	if m, _ := e.download("deps", "ver"); m != "" {
		t.Fatal("entry visible before finalize")
	}
	ok, id, msg := e.finalizeV2("deps", "ver", len(data))
	if !ok || id != "1" {
		t.Fatalf("finalize = %v %q %q", ok, id, msg)
	}
	m, url := e.download("deps", "ver")
	if m != "deps" {
		t.Fatalf("matched %q", m)
	}
	if got := fetch(t, url); got != data {
		t.Fatalf("restored %q", got)
	}
}

func TestV2BlockUpload(t *testing.T) {
	e := start(t, t.TempDir(), true)
	chunks := []string{"0123456789abcdef", "ghijklmnopqrstuv", "wxyz"}
	_, u, _ := e.create("big", "ver")
	for i := len(chunks) - 1; i >= 0; i-- { // any order
		if c := put(t, u, "comp=block&blockid="+blockID(i), nil, chunks[i]); c != 201 {
			t.Fatalf("Put Block %d = %d", i, c)
		}
	}
	if c := put(t, u, "comp=blocklist", nil, blockList(blockID(0), blockID(1), blockID(2))); c != 201 {
		t.Fatalf("Put Block List = %d", c)
	}
	whole := strings.Join(chunks, "")
	if ok, _, msg := e.finalizeV2("big", "ver", len(whole)); !ok {
		t.Fatalf("finalize refused: %s", msg)
	}
	_, url := e.download("big", "ver")
	if got := fetch(t, url); got != whole {
		t.Fatalf("restored %q", got)
	}
}

func TestV2Conflicts(t *testing.T) {
	e := start(t, t.TempDir(), true)
	if ok, _, _ := e.create("k", "v"); !ok {
		t.Fatal("first create refused")
	}
	if ok, _, msg := e.create("k", "v"); ok || !strings.Contains(msg, "another job") {
		t.Errorf("second create while one is open: ok=%v %q", ok, msg)
	}
	if ok, _, _ := e.create("k", "other"); !ok {
		t.Error("same key, other version refused")
	}
	// A reservation nobody finished is replaced once stale, and the old URL stops working.
	_, oldURL, _ := e.create("s", "v")
	e.svc.now = func() time.Time { return time.Now().Add(2 * reserveTTL) }
	ok, newURL, _ := e.create("s", "v")
	e.svc.now = time.Now
	if !ok || newURL == oldURL {
		t.Fatal("stale reservation not replaced")
	}
	if c := put(t, oldURL, "", blobHeaders, "x"); c != 404 {
		t.Errorf("the replaced upload still takes data: %d", c)
	}

	// Entries are immutable once finalized.
	put(t, newURL, "", blobHeaders, "data")
	if ok, _, msg := e.finalizeV2("s", "v", 4); !ok {
		t.Fatal(msg)
	}
	if ok, _, msg := e.create("s", "v"); ok || !strings.Contains(msg, "already exists") {
		t.Errorf("create over a finalized entry: ok=%v %q", ok, msg)
	}
}

func TestV2FinalizeChecks(t *testing.T) {
	e := start(t, t.TempDir(), true)
	if ok, _, msg := e.finalizeV2("nope", "v", 5); ok || !strings.Contains(msg, "no upload") {
		t.Errorf("no reservation: ok=%v %q", ok, msg)
	}
	_, u, _ := e.create("k", "v")
	if ok, _, msg := e.finalizeV2("k", "v", 5); ok || !strings.Contains(msg, "no committed content") {
		t.Errorf("before the upload: ok=%v %q", ok, msg)
	}
	put(t, u, "", blobHeaders, "12345")
	if ok, _, msg := e.finalizeV2("k", "v", 99); ok || !strings.Contains(msg, "5 bytes") {
		t.Errorf("wrong size: ok=%v %q", ok, msg)
	}
	// The refusal leaves the upload alive, so the right size still works.
	ok, id, msg := e.finalizeV2("k", "v", 5)
	if !ok {
		t.Fatal(msg)
	}
	// A retried finalize gets the same answer; one with another size is not the same upload.
	if ok, id2, _ := e.finalizeV2("k", "v", 5); !ok || id2 != id {
		t.Errorf("retried finalize: ok=%v id=%q, want %q", ok, id2, id)
	}
	if ok, _, _ := e.finalizeV2("k", "v", 6); ok {
		t.Error("finalize with another size accepted for a finished entry")
	}
	for name, body := range map[string]map[string]any{
		"no key":      {"version": "v", "size_bytes": "1"},
		"no version":  {"key": "k", "size_bytes": "1"},
		"no size":     {"key": "k", "version": "v"},
		"zero size":   {"key": "k", "version": "v", "size_bytes": "0"},
		"bad size":    {"key": "k", "version": "v", "size_bytes": "x"},
		"negative":    {"key": "k", "version": "v", "size_bytes": "-4"},
		"empty strng": {"key": "", "version": "", "size_bytes": "1"},
	} {
		if c, _ := e.v2("FinalizeCacheEntryUpload", e.tok, body); c != 400 {
			t.Errorf("%s: %d, want 400", name, c)
		}
	}
}

func TestV2SharesEntriesWithV1(t *testing.T) {
	e := start(t, t.TempDir(), true)
	e.save("from-v1", "ver", "saved by v1", 8)
	m, url := e.download("from-v1", "ver")
	if m != "from-v1" || fetch(t, url) != "saved by v1" {
		t.Fatalf("v2 could not read a v1 entry: %q", m)
	}

	_, u, _ := e.create("from-v2", "ver")
	put(t, u, "", blobHeaders, "saved by v2")
	e.finalizeV2("from-v2", "ver", 11)
	h, c := e.lookup("ver", "from-v2")
	if h == nil || fetch(t, h.Location) != "saved by v2" {
		t.Fatalf("v1 could not read a v2 entry: %d", c)
	}
	// And v1 sees the finished entry as taken.
	if c, _ := e.reserve("from-v2", "ver"); c != 409 {
		t.Errorf("v1 reserve over a v2 entry = %d, want 409", c)
	}
}

func TestV2RestoreKeys(t *testing.T) {
	e := start(t, t.TempDir(), true)
	e.save("npm-linux-aaa", "v", "old", 8)
	time.Sleep(2 * time.Millisecond)
	e.save("npm-linux-bbb", "v", "new", 8)

	if m, _ := e.download("npm-linux-zzz", "v", "npm-linux-"); m != "npm-linux-bbb" {
		t.Errorf("restore key picked %q, want the newest", m)
	}
	if m, _ := e.download("npm-linux-aaa", "v", "npm-"); m != "npm-linux-aaa" {
		t.Errorf("exact key lost: %q", m)
	}
	if m, _ := e.download("nothing", "v", "also-nothing"); m != "" {
		t.Errorf("hit on %q", m)
	}
	if m, _ := e.download("npm-linux-aaa", "other"); m != "" {
		t.Errorf("another version matched: %q", m)
	}
	// camelCase field names, as a protobuf-JSON client may send them.
	code, out := e.v2("GetCacheEntryDownloadURL", e.tok, map[string]any{"key": "zzz", "version": "v", "restoreKeys": []string{"npm-linux-a"}})
	if code != 200 || out["matched_key"] != "npm-linux-aaa" {
		t.Errorf("camelCase: %d %v", code, out)
	}
}

func TestV2Auth(t *testing.T) {
	e := start(t, t.TempDir(), true)
	body := map[string]any{"key": "k", "version": "v"}
	for name, tok := range map[string]string{"none": "", "garbage": "x.y.z", "tampered": e.tok + "x"} {
		if c, _ := e.v2("CreateCacheEntry", tok, body); c != 401 {
			t.Errorf("%s token: %d", name, c)
		}
	}
	expired, _ := e.signer.Token(artifact.Scope{Run: "r1", Job: "j1"}, -time.Hour)
	if c, _ := e.v2("GetCacheEntryDownloadURL", expired, body); c != 401 {
		t.Errorf("expired token: %d", c)
	}
	if c, out := e.v2("Nope", e.tok, body); c != 404 || out["code"] != "bad_route" {
		t.Errorf("unknown method: %d %v", c, out)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+twirpPrefix+"CreateCacheEntry", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+e.tok)
	req.Header.Set("Content-Type", "application/protobuf")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("protobuf content type: %d", resp.StatusCode)
	}
	for name, b := range map[string]map[string]any{
		"no key":     {"version": "v"},
		"no version": {"key": "k"},
		"long key":   {"key": strings.Repeat("k", maxKeyLen+1), "version": "v"},
	} {
		if c, _ := e.v2("CreateCacheEntry", e.tok, b); c != 400 {
			t.Errorf("create, %s: %d, want 400", name, c)
		}
	}
	keys := make([]string, maxKeys) // with the primary key, one more than allowed
	for i := range keys {
		keys[i] = fmt.Sprintf("restore-%d", i)
	}
	if c, _ := e.v2("GetCacheEntryDownloadURL", e.tok, map[string]any{"key": "k", "version": "v", "restore_keys": keys}); c != 400 {
		t.Errorf("too many restore keys: %d", c)
	}
}

func TestV2UploadURLChecks(t *testing.T) {
	e := start(t, t.TempDir(), true)
	_, u, _ := e.create("k", "v")
	exp := time.Now().Add(time.Hour).Unix()
	url := func(id string, sig string, exp int64) string {
		return fmt.Sprintf("%s%s%s?exp=%d&sig=%s", e.srv.URL, blobPath, id, exp, sig)
	}
	for name, bad := range map[string]string{
		"tampered signature": u[:len(u)-2] + "xx",
		"no signature":       u[:strings.Index(u, "&sig=")],
		"longer expiry":      strings.Replace(u, "exp=", "exp=9", 1),
		"another upload":     url("2", e.signer.SignUpload(uploadRealm, "1", exp), exp),
		// A URL signed for an artifact upload must not write into the cache.
		"artifact realm": url("1", e.signer.SignUpload("r1", "1", exp), exp),
		"expired":        url("1", e.signer.SignUpload(uploadRealm, "1", time.Now().Add(-time.Hour).Unix()), time.Now().Add(-time.Hour).Unix()),
	} {
		if c := put(t, bad, "", blobHeaders, "x"); c != 403 && c != 404 {
			t.Errorf("%s: %d, want 403 or 404", name, c)
		}
	}
	if c := put(t, u, "", blobHeaders, "x"); c != 201 {
		t.Fatalf("the genuine URL: %d", c)
	}
	// Once finalized, the URL is dead.
	e.finalizeV2("k", "v", 1)
	if c := put(t, u, "", blobHeaders, "x"); c != 404 {
		t.Errorf("a finalized upload still takes data: %d", c)
	}
}

func TestV2EnvAndClose(t *testing.T) {
	e := start(t, t.TempDir(), true)
	env, err := e.svc.Env(artifact.Scope{Run: "r", Job: "j"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if env["ACTIONS_CACHE_SERVICE_V2"] != "true" || env["ACTIONS_RESULTS_URL"] != e.srv.URL+"/" || env["ACTIONS_CACHE_URL"] != e.srv.URL+"/" {
		t.Errorf("env = %v", env)
	}

	// Closing the service throws away an upload that was not finalized.
	_, u, _ := e.create("k", "v")
	put(t, u, "", blobHeaders, "data")
	if _, err := e.store.Stat(context.Background(), objectKey(1)); err != nil {
		t.Fatalf("uploaded object missing: %v", err)
	}
	e.svc.Close()
	if _, err := e.store.Stat(context.Background(), objectKey(1)); err == nil {
		t.Error("an unfinished upload survived Close")
	} else if !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat after Close: %v", err)
	}
}
