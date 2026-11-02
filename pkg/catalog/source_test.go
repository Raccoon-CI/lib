package catalog

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const zigCatalog = `
toolchains:
  zig: {title: Zig, commands: [zig]}
`

// feed is a catalog server whose content and availability the test controls.
type feed struct {
	body, etag string
	sig        string // served at /catalog.yaml.sig when non-empty
	down       atomic.Bool
	requests   atomic.Int32
	conditions atomic.Int32 // requests that sent If-None-Match
	srv        *httptest.Server
}

func newFeed(t *testing.T, body string) *feed {
	t.Helper()
	f := &feed{body: body, etag: `"v1"`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/catalog.yaml":
			if r.Header.Get("If-None-Match") != "" {
				f.conditions.Add(1)
			}
			if r.Header.Get("If-None-Match") == f.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", f.etag)
			w.Write([]byte(f.body))
		case "/catalog.yaml.sig":
			if f.sig == "" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(f.sig))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *feed) source(t *testing.T) (*Source, *time.Time) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	return &Source{
		URL:           f.srv.URL + "/catalog.yaml",
		CacheDir:      filepath.Join(t.TempDir(), "catalog"),
		AllowInsecure: true,
		Now:           func() time.Time { return now },
	}, &now
}

func hasZig(c *Catalog) bool { _, ok := c.Command("zig"); return ok }

func TestSourcePullCacheAndRefresh(t *testing.T) {
	f := newFeed(t, zigCatalog)
	src, now := f.source(t)
	ctx := context.Background()

	c, st := src.Load(ctx, Default(), false)
	if st.Origin != FromNetwork || st.Err != nil || !hasZig(c) {
		t.Fatalf("first load: %+v zig=%v", st, hasZig(c))
	}
	if _, ok := c.Command("npm"); !ok {
		t.Error("the pulled catalog should overlay the built-in one, not replace it")
	}

	// Young cache: no network.
	before := f.requests.Load()
	*now = now.Add(time.Hour)
	if c, st = src.Load(ctx, Default(), false); st.Origin != FromCache || !hasZig(c) || f.requests.Load() != before {
		t.Errorf("young cache: %+v, %d requests", st, f.requests.Load()-before)
	}

	// Old cache: conditional request, the server says nothing changed.
	*now = now.Add(48 * time.Hour)
	if c, st = src.Load(ctx, Default(), false); st.Origin != NotModified || !hasZig(c) || f.conditions.Load() != 1 {
		t.Errorf("stale cache: %+v, %d conditional requests", st, f.conditions.Load())
	}
	// ... and that counts as a refresh: it is young again.
	before = f.requests.Load()
	*now = now.Add(time.Hour)
	if _, st = src.Load(ctx, Default(), false); st.Origin != FromCache || f.requests.Load() != before {
		t.Errorf("after 304: %+v", st)
	}

	// The feed changes; force picks it up.
	f.body, f.etag = zigCatalog+"tools:\n  just: {image: just}\n", `"v2"`
	if c, st = src.Load(ctx, Default(), true); st.Origin != FromNetwork {
		t.Fatalf("forced: %+v", st)
	}
	if _, ok := c.Command("just"); !ok {
		t.Error("update not applied")
	}
}

func TestSourceNeverGetsInTheWay(t *testing.T) {
	f := newFeed(t, zigCatalog)
	src, now := f.source(t)
	ctx := context.Background()

	// Offline on the very first run: the built-in catalog, with the reason.
	f.down.Store(true)
	c, st := src.Load(ctx, Default(), false)
	if st.Origin != BuiltIn || st.Err == nil || hasZig(c) {
		t.Errorf("offline, no cache: %+v", st)
	}
	if _, ok := c.Command("npm"); !ok {
		t.Error("built-in catalog lost")
	}

	// Online once, then offline: the cached copy keeps working.
	f.down.Store(false)
	src.Load(ctx, Default(), false)
	f.down.Store(true)
	*now = now.Add(72 * time.Hour)
	c, st = src.Load(ctx, Default(), false)
	if st.Origin != FromCache || st.Err == nil || !hasZig(c) {
		t.Errorf("offline with a stale cache: %+v zig=%v", st, hasZig(c))
	}

	// A broken update does not replace the good cache.
	f.down.Store(false)
	f.body, f.etag = "toolchains:\n  zig: {titel: typo}\n", `"bad"`
	c, st = src.Load(ctx, Default(), true)
	if st.Origin != FromCache || st.Err == nil || !hasZig(c) {
		t.Errorf("bad update: %+v zig=%v", st, hasZig(c))
	}
	f.body, f.etag = zigCatalog, `"v1"`
	if c, st = src.Load(ctx, Default(), false); !hasZig(c) {
		t.Errorf("after the bad update the cache must still be the good one: %+v", st)
	}

	// A catalog from the future is refused by name, not half-read.
	f.body, f.etag = "schema: 99\nbrand-new-section: {}\n", `"future"`
	if _, st = src.Load(ctx, Default(), true); !errors.Is(st.Err, ErrNewerSchema) {
		t.Errorf("future schema: %+v", st)
	}

	// Poison in the cache directory is ignored, not trusted.
	os.WriteFile(filepath.Join(src.CacheDir, bodyFile), []byte("packages:\n  \"-oEvil\": {commands: [x]}\n"), 0o600)
	f.down.Store(true)
	if c, st = src.Load(ctx, Default(), false); hasZig(c) || st.Origin != BuiltIn {
		t.Errorf("tampered cache: %+v zig=%v", st, hasZig(c))
	}
}

func TestSourceSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)
	sign := func(body string) string {
		return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(body))) + "\n"
	}
	ctx := context.Background()

	f := newFeed(t, zigCatalog)
	f.sig = sign(zigCatalog)
	src, _ := f.source(t)
	src.PublicKey = pub
	if c, st := src.Load(ctx, Default(), false); st.Origin != FromNetwork || !hasZig(c) {
		t.Fatalf("valid signature: %+v", st)
	}
	// The cache is verified again on every read, against the current key.
	src2 := *src
	src2.PublicKey = otherPub
	f.down.Store(true)
	if c, st := src2.Load(ctx, Default(), false); hasZig(c) || st.Origin != BuiltIn {
		t.Errorf("cache must not be trusted under another key: %+v", st)
	}
	f.down.Store(false)

	for name, badSig := range map[string]string{
		"wrong signature":   sign("something else"),
		"missing signature": "",
		"garbage signature": "not base64!",
	} {
		bad := newFeed(t, zigCatalog)
		bad.sig = badSig
		s, _ := bad.source(t)
		s.PublicKey = pub
		if c, st := s.Load(ctx, Default(), false); hasZig(c) || st.Err == nil {
			t.Errorf("%s accepted: %+v", name, st)
		}
	}

	if _, err := ParsePublicKey(base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Error(err)
	}
	if _, err := ParsePublicKey("short"); err == nil {
		t.Error("a short key must be rejected")
	}
}

func TestSourceRequiresHTTPS(t *testing.T) {
	for _, u := range []string{"http://example.com/c.yaml", "ftp://example.com/c.yaml", "example.com/c.yaml", ""} {
		src := &Source{URL: u, CacheDir: t.TempDir()}
		c, st := src.Load(context.Background(), Default(), true)
		if st.Err == nil || st.Origin != BuiltIn || c == nil {
			t.Errorf("%q: %+v", u, st)
		}
	}
	if st := (&Source{URL: "https://example.com/c.yaml"}).check(); st == nil || !strings.Contains(st.Error(), "cache directory") {
		t.Errorf("missing cache dir: %v", st)
	}
}

func TestSourceRefusesDowngradeRedirect(t *testing.T) {
	plain := newFeed(t, zigCatalog)
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.srv.URL+"/catalog.yaml", http.StatusFound)
	}))
	defer tls.Close()
	src := &Source{URL: tls.URL + "/catalog.yaml", CacheDir: t.TempDir()}
	src.Client = tls.Client() // trusts the test certificate
	src.Client.CheckRedirect = src.checkRedirect
	c, st := src.Load(context.Background(), Default(), true)
	if hasZig(c) || st.Err == nil || !strings.Contains(st.Err.Error(), "refusing a redirect") {
		t.Errorf("downgrade followed: %+v", st)
	}
}
