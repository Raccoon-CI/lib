package azblob

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raccoonci/internal/blob"
)

const prefix = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

func id(i int) string {
	return base64.StdEncoding.EncodeToString([]byte(prefix + fmt.Sprintf("%012d", i)))
}

type rig struct {
	t     *testing.T
	store *blob.Dir
	up    *Upload
	srv   *httptest.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	store, err := blob.NewDir(t.TempDir(), "http://x", []byte("0123456789abcdef0123"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := &rig{t: t, store: store, up: New(store, "k/1", "application/zip", filepath.Join(t.TempDir(), "spool"), 16)}
	r.srv = httptest.NewServer(r.up)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) put(query string, hdr map[string]string, body io.Reader) (int, string) {
	r.t.Helper()
	req, _ := http.NewRequest("PUT", r.srv.URL+"/blob?"+query, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("x-ms-error-code")
}

var blobType = map[string]string{"x-ms-blob-type": "BlockBlob"}

func (r *rig) stored() string {
	r.t.Helper()
	u, err := r.store.PresignGet("k/1", 60e9)
	if err != nil {
		r.t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(blob.DownloadPrefix, r.store.Handler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", u[len("http://x"):], nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		r.t.Fatalf("reading the stored blob: %d", rec.Code)
	}
	return rec.Body.String()
}

func TestPutBlob(t *testing.T) {
	r := newRig(t)
	if c, code := r.put("", blobType, strings.NewReader("a whole small file")); c != 201 {
		t.Fatalf("Put Blob = %d %s", c, code)
	}
	if !r.up.Committed() || r.up.Mode() != ModeSingle {
		t.Fatalf("committed=%v mode=%v", r.up.Committed(), r.up.Mode())
	}
	if got := r.stored(); got != "a whole small file" {
		t.Fatalf("stored %q", got)
	}
	// A retry after a lost reply is accepted and changes nothing.
	if c, code := r.put("", blobType, strings.NewReader("a whole small file")); c != 201 {
		t.Errorf("retried Put Blob = %d %s", c, code)
	}
	// Blocks can no longer be added, and a block list cannot adopt the blob.
	if c, _ := r.put("comp=block&blockid="+id(0), nil, strings.NewReader("x")); c != 409 {
		t.Errorf("Put Block after Put Blob = %d, want 409", c)
	}
	if c, _ := r.put("comp=blocklist", nil, strings.NewReader("<BlockList><Latest>"+id(0)+"</Latest></BlockList>")); c != 409 {
		t.Errorf("Put Block List after Put Blob = %d, want 409", c)
	}
}

func TestPutBlobAfterBlocks(t *testing.T) {
	r := newRig(t)
	r.put("comp=block&blockid="+id(0), nil, strings.NewReader("x"))
	if c, code := r.put("", blobType, strings.NewReader("whole")); c != 409 || code != "BlobHasBlocks" {
		t.Errorf("Put Blob over staged blocks = %d %s", c, code)
	}
}

func TestPutBlobChecks(t *testing.T) {
	r := newRig(t)
	if c, _ := r.put("", blobType, strings.NewReader("")); c != 400 {
		t.Errorf("empty blob = %d, want 400", c)
	}
	if c, code := r.put("", nil, strings.NewReader("x")); c != 400 || code != "InvalidHeaderValue" {
		t.Errorf("no blob type = %d %s", c, code)
	}
	if c, _ := r.put("", map[string]string{"x-ms-blob-type": "PageBlob"}, strings.NewReader("x")); c != 400 {
		t.Errorf("page blob = %d, want 400", c)
	}
	// A body of unknown length (chunked) cannot be checked against anything.
	type hidden struct{ io.Reader }
	if c, code := r.put("", blobType, hidden{strings.NewReader("x")}); c != 411 {
		t.Errorf("chunked Put Blob = %d %s, want 411", c, code)
	}
	// None of that wrote anything.
	if r.up.Committed() || r.up.Mode() != ModeNone {
		t.Errorf("rejected requests changed the upload: committed=%v mode=%v", r.up.Committed(), r.up.Mode())
	}
}

func TestOversizedRequest(t *testing.T) {
	r := newRig(t)
	for _, q := range []string{"", "comp=block&blockid=" + id(0)} {
		req := httptest.NewRequest("PUT", "/blob?"+q, strings.NewReader("x"))
		req.Header.Set("x-ms-blob-type", "BlockBlob")
		req.ContentLength = MaxRequestSize + 1
		rec := httptest.NewRecorder()
		r.up.ServeHTTP(rec, req)
		if rec.Code != 413 {
			t.Errorf("%q: %d, want 413", q, rec.Code)
		}
	}
}

func TestUnsupportedCall(t *testing.T) {
	r := newRig(t)
	if c, _ := r.put("comp=appendblock", nil, strings.NewReader("x")); c != 501 {
		t.Errorf("unknown comp = %d, want 501", c)
	}
}

func TestDiscard(t *testing.T) {
	r := newRig(t)
	r.put("", blobType, strings.NewReader("data"))
	r.up.Discard(context.Background())
	if _, err := r.store.Stat(context.Background(), "k/1"); err == nil {
		t.Error("a discarded blob is still in the store")
	}
	if c, _ := r.put("", blobType, strings.NewReader("data")); c != 409 {
		t.Errorf("Put Blob after Discard = %d, want 409", c)
	}

	// Discarding while blocks wait on disk removes them.
	r2 := newRig(t)
	r2.put("comp=block&blockid=QUFB", nil, strings.NewReader("odd"))
	dir := r2.up.SpoolDir()
	if dir == "" {
		t.Fatal("spool path not taken")
	}
	r2.up.Discard(context.Background())
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("spool left behind: %v", err)
	}
}
