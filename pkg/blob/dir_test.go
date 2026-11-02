package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newDir(t *testing.T) (*Dir, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	d, err := NewDir(t.TempDir(), srv.URL, []byte("0123456789abcdef0123"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	mux.Handle(DownloadPrefix, d.Handler())
	return d, srv
}

func TestDirRoundTrip(t *testing.T) {
	d, _ := newDir(t)
	ctx := context.Background()
	key := "run 1/art#2/名字.zip"

	up, err := d.BeginUpload(ctx, key, "application/zip")
	if err != nil {
		t.Fatal(err)
	}
	chunks := [][]byte{[]byte("hello, "), []byte("wor"), []byte("ld")}
	parts := make([]Part, len(chunks))
	var wg sync.WaitGroup
	for i := len(chunks) - 1; i >= 0; i-- {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := up.PutPart(ctx, i+1, bytes.NewReader(chunks[i]), int64(len(chunks[i])))
			if err != nil {
				t.Error(err)
			}
			parts[i] = p
		}()
	}
	wg.Wait()
	stray, _ := up.PutPart(ctx, 9, strings.NewReader("stray"), 5)
	_ = stray
	if _, err := d.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("object visible before Complete: %v", err)
	}
	if err := up.Complete(ctx, []Part{parts[2], parts[0], parts[1]}); err != nil {
		t.Fatal(err)
	}
	info, err := d.Stat(ctx, key)
	if err != nil || info.Size != 12 {
		t.Fatalf("Stat = %+v, %v", info, err)
	}

	u, err := d.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello, world" {
		t.Fatalf("GET = %d %q", resp.StatusCode, body)
	}

	if err := d.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, key); err != nil {
		t.Fatalf("deleting a missing object: %v", err)
	}
	if _, err := d.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after Delete = %v", err)
	}
}

func TestDirRejects(t *testing.T) {
	d, _ := newDir(t)
	ctx := context.Background()
	for _, key := range []string{"", "/abs", "a/../b", "a//b", "..", "a/."} {
		if _, err := d.BeginUpload(ctx, key, ""); err == nil {
			t.Errorf("key %q accepted", key)
		}
	}
	up, _ := d.BeginUpload(ctx, "k", "")
	if _, err := up.PutPart(ctx, 1, strings.NewReader("abc"), 5); err == nil {
		t.Error("short part accepted")
	}
	if _, err := up.PutPart(ctx, 1, strings.NewReader("abcdef"), 5); err == nil {
		t.Error("long part accepted")
	}
	p, _ := up.PutPart(ctx, 1, strings.NewReader("abc"), 3)
	if err := up.Complete(ctx, []Part{p, p}); err == nil {
		t.Error("duplicate part accepted")
	}
	if err := up.Complete(ctx, []Part{{Number: 1, ETag: `"other"`}}); err == nil {
		t.Error("part with a wrong ETag accepted")
	}
	if err := up.Complete(ctx, nil); err == nil {
		t.Error("empty part list accepted")
	}
	// A replaced part invalidates the ETag the caller holds.
	up.PutPart(ctx, 1, strings.NewReader("xyz"), 3)
	if err := up.Complete(ctx, []Part{p}); err == nil {
		t.Error("stale part accepted")
	}
}

func TestDirAbort(t *testing.T) {
	d, _ := newDir(t)
	ctx := context.Background()
	up, _ := d.BeginUpload(ctx, "k", "")
	up.PutPart(ctx, 1, strings.NewReader("x"), 1)
	if err := up.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if err := up.Abort(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Abort = %v", err)
	}
}

func TestDirDownloadLinks(t *testing.T) {
	d, srv := newDir(t)
	ctx := context.Background()
	up, _ := d.BeginUpload(ctx, "a/b.zip", "")
	p, _ := up.PutPart(ctx, 1, strings.NewReader("data"), 4)
	up.Complete(ctx, []Part{p})

	u, _ := d.PresignGet(ctx, "a/b.zip", time.Minute)
	other, _ := d.PresignGet(ctx, "a/c.zip", time.Minute)

	// A link handed out on another address of the server points there and
	// carries the same signature.
	elsewhere, _ := d.PresignGet(WithBaseURL(ctx, "http://172.18.0.1:4000/"), "a/b.zip", time.Minute)
	if want := "http://172.18.0.1:4000" + u[len(srv.URL):]; elsewhere != want {
		t.Errorf("link with another base = %q, want %q", elsewhere, want)
	}
	get := func(u string) int {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get(u); c != 200 {
		t.Fatalf("valid link: %d", c)
	}
	if c := get(strings.Replace(u, "b.zip", "c.zip", 1)); c != 403 {
		t.Errorf("link reused for another key: %d", c)
	}
	if c := get(strings.Replace(u, "exp=", "exp=9", 1)); c != 403 {
		t.Errorf("tampered expiry: %d", c)
	}
	if c := get(srv.URL + "/_dl/a/b.zip"); c != 403 {
		t.Errorf("no signature: %d", c)
	}
	if c := get(other); c != 404 {
		t.Errorf("signed link to a missing object: %d", c)
	}
	d.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if c := get(u); c != 403 {
		t.Errorf("expired link: %d", c)
	}
}
