package catalog

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxCatalogSize bounds a downloaded catalog; the embedded one is ~12 KB.
const maxCatalogSize = 1 << 20

// DefaultMaxAge is how long a pulled catalog is used before asking again.
const DefaultMaxAge = 24 * time.Hour

// Source pulls a catalog from a URL and keeps the last good copy on disk.
//
// Updating never gets in the way of running jobs: whatever goes wrong
// (offline, a bad file, a catalog from the future), Load falls back to the
// cached copy and then to the catalog it was given, and says why in
// Status.Err. A pulled file only replaces the cache after it has been
// verified and validated as a whole.
//
// The URL must be https, and redirects may not downgrade it. If PublicKey is
// set, the catalog must also carry a valid ed25519 signature (fetched from
// URL + ".sig", base64), which is checked again each time the cache is read.
// A catalog can only name things to look up and install through Raccoon CI's
// own installers (see the package comment), but a signature is what makes a
// shared feed trustworthy.
type Source struct {
	URL      string
	CacheDir string
	// MaxAge: a cached copy younger than this is used without asking the
	// network. Zero means DefaultMaxAge.
	MaxAge    time.Duration
	PublicKey ed25519.PublicKey
	Client    *http.Client
	// AllowInsecure permits http:// URLs, for tests.
	AllowInsecure bool
	// Now is the clock, for tests.
	Now func() time.Time
}

// Origin says where a loaded catalog came from.
type Origin string

const (
	FromNetwork Origin = "network"      // pulled just now
	NotModified Origin = "not-modified" // the server confirmed the cached copy is current
	FromCache   Origin = "cache"        // cached copy, young enough not to ask
	BuiltIn     Origin = "built-in"     // nothing usable was pulled
)

type Status struct {
	Origin  Origin
	Fetched time.Time // when the pulled copy in use was fetched; zero for BuiltIn
	// Err is why the network refresh didn't happen or didn't apply, when the
	// catalog in use is older than a refresh would have given.
	Err error
}

type cacheMeta struct {
	ETag    string    `json:"etag"`
	Fetched time.Time `json:"fetched"`
}

const (
	bodyFile = "catalog.yaml"
	sigFile  = "catalog.yaml.sig"
	metaFile = "meta.json"
)

// ParsePublicKey reads a base64 ed25519 public key (32 bytes).
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("a catalog key is %d bytes of base64", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: s.checkRedirect}
}

// checkRedirect keeps a pull from being downgraded to plain http on the way.
func (s *Source) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" && !s.AllowInsecure {
		return fmt.Errorf("refusing a redirect to %s://", req.URL.Scheme)
	}
	return nil
}

func (s *Source) check() error {
	u, err := url.Parse(s.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("catalog URL %q is not a URL", s.URL)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && s.AllowInsecure) {
		return fmt.Errorf("catalog URL %q must be https", s.URL)
	}
	if s.CacheDir == "" {
		return errors.New("catalog source needs a cache directory")
	}
	return nil
}

// Load returns base with the pulled catalog applied on top. With force it
// asks the server even if the cache is young.
func (s *Source) Load(ctx context.Context, base *Catalog, force bool) (*Catalog, Status) {
	if err := s.check(); err != nil {
		return base, Status{Origin: BuiltIn, Err: err}
	}
	cached, meta := s.readCache(base)
	maxAge := s.MaxAge
	if maxAge == 0 {
		maxAge = DefaultMaxAge
	}
	if cached != nil && !force && s.now().Sub(meta.Fetched) < maxAge {
		return cached, Status{Origin: FromCache, Fetched: meta.Fetched}
	}

	// fallback is what to use when the refresh doesn't work out.
	fallback := func(err error) (*Catalog, Status) {
		if cached != nil {
			return cached, Status{Origin: FromCache, Fetched: meta.Fetched, Err: err}
		}
		return base, Status{Origin: BuiltIn, Err: err}
	}

	etag := ""
	if cached != nil {
		etag = meta.ETag
	}
	body, sig, newETag, err := s.fetch(ctx, etag)
	if errors.Is(err, errNotModified) && cached != nil {
		meta.Fetched = s.now()
		_ = s.writeMeta(meta)
		return cached, Status{Origin: NotModified, Fetched: meta.Fetched}
	}
	if err != nil {
		return fallback(err)
	}
	c, err := s.verify(base, body, sig)
	if err != nil {
		return fallback(fmt.Errorf("pulled catalog rejected: %w", err))
	}
	st := Status{Origin: FromNetwork, Fetched: s.now()}
	if err := s.writeCache(body, sig, cacheMeta{ETag: newETag, Fetched: st.Fetched}); err != nil {
		st.Err = fmt.Errorf("catalog not cached: %w", err)
	}
	return c, st
}

// verify checks the signature (if a key is configured) and applies the
// catalog on top of base.
func (s *Source) verify(base *Catalog, body, sig []byte) (*Catalog, error) {
	if s.PublicKey != nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
		if err != nil || len(raw) != ed25519.SignatureSize {
			return nil, errors.New("missing or malformed signature")
		}
		if !ed25519.Verify(s.PublicKey, body, raw) {
			return nil, errors.New("signature doesn't match")
		}
	}
	return base.Overlay(body)
}

var errNotModified = errors.New("not modified")

func (s *Source) fetch(ctx context.Context, etag string) (body, sig []byte, newETag string, err error) {
	get := func(u, etag string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		return s.client().Do(req)
	}
	read := func(resp *http.Response) ([]byte, error) {
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogSize+1))
		if err == nil && len(b) > maxCatalogSize {
			err = fmt.Errorf("larger than %d KiB", maxCatalogSize>>10)
		}
		return b, err
	}

	resp, err := get(s.URL, etag)
	if err != nil {
		return nil, nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return nil, nil, "", errNotModified
	default:
		return nil, nil, "", fmt.Errorf("GET %s: %s", s.URL, resp.Status)
	}
	if body, err = read(resp); err != nil {
		return nil, nil, "", fmt.Errorf("GET %s: %w", s.URL, err)
	}
	newETag = resp.Header.Get("ETag")

	if s.PublicKey != nil {
		sresp, err := get(s.URL+".sig", "")
		if err != nil {
			return nil, nil, "", err
		}
		defer sresp.Body.Close()
		if sresp.StatusCode != http.StatusOK {
			return nil, nil, "", fmt.Errorf("GET %s.sig: %s", s.URL, sresp.Status)
		}
		if sig, err = read(sresp); err != nil {
			return nil, nil, "", fmt.Errorf("GET %s.sig: %w", s.URL, err)
		}
	}
	return body, sig, newETag, nil
}

// readCache returns the cached catalog if it is present and still verifies
// and validates under the current code and key.
func (s *Source) readCache(base *Catalog) (*Catalog, cacheMeta) {
	var meta cacheMeta
	mb, err := readCapped(filepath.Join(s.CacheDir, metaFile))
	if err != nil || json.Unmarshal(mb, &meta) != nil {
		return nil, meta
	}
	body, err := readCapped(filepath.Join(s.CacheDir, bodyFile))
	if err != nil {
		return nil, meta
	}
	var sig []byte
	if s.PublicKey != nil {
		if sig, err = readCapped(filepath.Join(s.CacheDir, sigFile)); err != nil {
			return nil, meta
		}
	}
	c, err := s.verify(base, body, sig)
	if err != nil {
		return nil, meta
	}
	return c, meta
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxCatalogSize))
}

func (s *Source) writeCache(body, sig []byte, meta cacheMeta) error {
	if err := os.MkdirAll(s.CacheDir, 0o700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.CacheDir, bodyFile), body); err != nil {
		return err
	}
	if sig != nil {
		if err := writeAtomic(filepath.Join(s.CacheDir, sigFile), sig); err != nil {
			return err
		}
	}
	return s.writeMeta(meta)
}

func (s *Source) writeMeta(meta cacheMeta) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.CacheDir, metaFile), b)
}

// writeAtomic replaces path with data in one rename, so a crash or a second
// raccoonci process never sees half a file.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
