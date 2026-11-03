// Package cache serves the v1 cache protocol of actions/cache (and the
// cache: option of the setup-* actions), so they work unchanged.
//
// The client reaches it through ACTIONS_CACHE_URL and sends the same runtime
// token as for artifacts. The protocol is plain REST under
// /_apis/artifactcache/:
//
//	GET   cache?keys=a,b&version=v   find an entry (204 on a miss)
//	POST  caches {key,version}       reserve an id (409 if the entry exists)
//	PATCH caches/{id}                one chunk, placed by Content-Range
//	POST  caches/{id} {size}         commit
//
// Entries are immutable once committed, as on GitHub. Chunks wait on disk
// until the commit, because the client chooses their size (upload-chunk-size)
// and the store needs parts of at least 5 MiB; the commit cuts the entry into
// parts of its own. Committed entries are listed in an index file, so a cache
// outlives the process.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Raccoon-CI/lib/internal/azblob"
	"github.com/Raccoon-CI/lib/pkg/artifact"
	"github.com/Raccoon-CI/lib/pkg/blob"
)

const (
	apiPrefix    = "/_apis/artifactcache/"
	maxEntrySize = 10 << 30  // GitHub's limit per entry
	maxChunkSize = 256 << 20 // the toolkit caps its chunks at 128 MiB
	maxKeys      = 10
	maxKeyLen    = 512
	reserveTTL   = time.Hour // a reservation nobody finished can be replaced
	downloadTTL  = time.Hour
	uploadTTL    = 6 * time.Hour
	// uploadRealm labels the signatures on v2 upload URLs. Run ids cannot
	// contain "~", so no artifact upload URL can pass for one of ours.
	uploadRealm = "~cache"
)

type record struct {
	ID      int64     `json:"id"`
	Key     string    `json:"key"`
	Version string    `json:"version"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
	// Used is when a lookup last hit the entry; the oldest are evicted first.
	// It is saved with the index at the next commit, not on every hit.
	Used time.Time `json:"used,omitempty"`
}

func (r *record) lastUsed() time.Time {
	if r.Used.After(r.Created) {
		return r.Used
	}
	return r.Created
}

// pending is a reserved entry that is still receiving chunks.
type pending struct {
	id       int64
	key      string
	version  string
	reserved time.Time

	mu     sync.Mutex
	done   bool            // committed or abandoned
	spool  string          // v1: directory holding the chunks
	chunks map[int64]int64 // v1: start offset -> length
	upload *azblob.Upload  // v2: receives the data through a signed URL
}

type Service struct {
	store    blob.Store
	signer   *artifact.Signer
	baseURL  string
	spoolDir string
	ownSpool bool
	index    string
	now      func() time.Time
	partSize int64 // of the parts an entry is cut into; tests use tiny ones
	maxBytes int64
	free     func() (int64, error)

	// advertiseV2 tells clients to speak the v2 protocol. Both protocols are
	// always served, since they share one set of entries.
	advertiseV2 bool

	mu       sync.Mutex
	next     int64
	lastSave time.Time // of the index
	dirty    bool      // a last-use time is not on disk yet
	done     map[int64]*record
	uploads  map[int64]*pending
}

type Config struct {
	Store  blob.Store
	Signer *artifact.Signer
	// BaseURL is where jobs reach this service, e.g. http://127.0.0.1:4000.
	BaseURL string
	// IndexPath is the file that remembers committed entries. Empty keeps
	// them in memory only.
	IndexPath string
	// SpoolDir is where the service makes a directory of its own for chunks
	// until commit (see azblob.NewSpoolDir). Entries can be gigabytes, so put
	// it on the disk that holds the store. Empty means the system temporary
	// directory.
	SpoolDir string
	// AdvertiseV2 makes Env point clients at the v2 (Twirp) protocol instead
	// of v1.
	AdvertiseV2 bool
	// MaxBytes caps the total size of committed entries. When a new entry
	// would exceed it, the least recently used ones are evicted, as on GitHub.
	// An entry larger than the cap is refused. 0 means no cap.
	MaxBytes int64
	// Free reports the bytes free on the disk the store writes to; nil means
	// it is not checked. Before an entry is stored, the least recently used
	// entries are evicted until it fits with freeMargin to spare, and it is
	// refused if even that is not enough.
	Free func() (int64, error)
}

func New(c Config) (*Service, error) {
	if c.Store == nil || c.Signer == nil || c.BaseURL == "" {
		return nil, errors.New("cache: Store, Signer and BaseURL are required")
	}
	s := &Service{store: c.Store, signer: c.Signer, baseURL: strings.TrimRight(c.BaseURL, "/"),
		index: c.IndexPath, now: time.Now, next: 1,
		partSize: azblob.DefaultPartSize, advertiseV2: c.AdvertiseV2,
		maxBytes: c.MaxBytes, free: c.Free,
		done: map[int64]*record{}, uploads: map[int64]*pending{}}
	if s.index != "" {
		if err := os.MkdirAll(filepath.Dir(s.index), 0o700); err != nil {
			return nil, err
		}
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	d, err := azblob.NewSpoolDir(c.SpoolDir, "raccoonci-cache-spool-")
	if err != nil {
		return nil, err
	}
	s.spoolDir, s.ownSpool = d, true
	return s, nil
}

type indexFile struct {
	Next    int64     `json:"next"`
	Entries []*record `json:"entries"`
}

func (s *Service) load() error {
	b, err := os.ReadFile(s.index)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f indexFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("cache index %s: %w", s.index, err)
	}
	for _, r := range f.Entries {
		if r == nil || r.ID < 1 || r.Key == "" || r.Version == "" {
			return fmt.Errorf("cache index %s: bad entry", s.index)
		}
		s.done[r.ID] = r
		s.next = max(s.next, r.ID+1)
	}
	s.next = max(s.next, f.Next)
	return nil
}

// save rewrites the index. Callers hold s.mu.
func (s *Service) save() error {
	if s.index == "" {
		return nil
	}
	f := indexFile{Next: s.next}
	for _, r := range s.done {
		f.Entries = append(f.Entries, r)
	}
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].ID < f.Entries[j].ID })
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := s.index + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.index); err != nil {
		return err
	}
	s.lastSave, s.dirty = s.now(), false
	return nil
}

// Close drops reservations that were never committed, and saves last-use
// times that are not on disk yet.
func (s *Service) Close() {
	s.mu.Lock()
	ps := make([]*pending, 0, len(s.uploads))
	for id, p := range s.uploads {
		ps = append(ps, p)
		delete(s.uploads, id)
	}
	if s.dirty {
		s.save()
	}
	s.mu.Unlock()
	for _, p := range ps {
		p.abandon()
	}
	if s.ownSpool {
		os.RemoveAll(s.spoolDir)
	}
}

func (p *pending) abandon() {
	p.mu.Lock()
	p.done = true
	spool, up := p.spool, p.upload
	p.mu.Unlock()
	if spool != "" {
		os.RemoveAll(spool)
	}
	if up != nil {
		up.Discard(context.Background())
	}
}

// Env is what a job's steps need to find the cache. Unless v2 is advertised,
// ACTIONS_CACHE_SERVICE_V2 is cleared so a value inherited from the host
// cannot switch the client to a protocol we were not asked to use.
func (s *Service) Env(sc artifact.Scope, ttl time.Duration) (map[string]string, error) {
	tok, err := s.signer.Token(sc, ttl)
	if err != nil {
		return nil, err
	}
	env := map[string]string{
		"ACTIONS_CACHE_URL":        s.baseURL + "/",
		"ACTIONS_RUNTIME_TOKEN":    tok,
		"ACTIONS_CACHE_SERVICE_V2": "",
	}
	if s.advertiseV2 {
		// v2 is reached through ACTIONS_RESULTS_URL, the same address as the artifacts.
		env["ACTIONS_CACHE_SERVICE_V2"] = "true"
		env["ACTIONS_RESULTS_URL"] = s.baseURL + "/"
	}
	return env, nil
}

// Routes registers the v1 and v2 endpoints on mux.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiPrefix+"cache", s.auth(s.lookup))
	mux.HandleFunc("GET "+apiPrefix+"caches", s.auth(s.list))
	mux.HandleFunc("POST "+apiPrefix+"caches", s.auth(s.reserve))
	mux.HandleFunc("PATCH "+apiPrefix+"caches/{id}", s.auth(s.chunk))
	mux.HandleFunc("POST "+apiPrefix+"caches/{id}", s.auth(s.commit))
	mux.HandleFunc("POST "+twirpPrefix+"{method}", s.twirp)
	mux.HandleFunc("PUT "+blobPath+"{id}", s.putBlob)
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Routes(mux)
	return mux
}

func reply(w http.ResponseWriter, status int, v any) {
	if v == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// fail answers the way the toolkit reads errors: a JSON body with a message.
func fail(w http.ResponseWriter, status int, format string, a ...any) {
	reply(w, status, map[string]string{"message": fmt.Sprintf(format, a...)})
}

func (s *Service) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			fail(w, 401, "missing bearer token")
			return
		}
		if _, err := s.signer.Verify(tok); err != nil {
			fail(w, 401, "%v", err)
			return
		}
		h(w, r)
	}
}

func newer(a, b *record) bool {
	return a.Created.After(b.Created) || (a.Created.Equal(b.Created) && a.ID > b.ID)
}

// find looks for an entry the way actions/cache documents: for each key in
// order, an exact match wins, otherwise the newest entry whose key starts
// with it. Callers hold s.mu.
func (s *Service) find(keys []string, version string) *record {
	for _, k := range keys {
		var best *record
		for _, r := range s.done {
			if r.Version != version {
				continue
			}
			if r.Key == k {
				return r
			}
			if strings.HasPrefix(r.Key, k) && (best == nil || newer(r, best)) {
				best = r
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

func (s *Service) lookup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	version := q.Get("version")
	var keys []string
	for _, k := range strings.Split(q.Get("keys"), ",") {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if version == "" || len(keys) == 0 || len(keys) > maxKeys {
		fail(w, 400, "need a version and 1 to %d keys", maxKeys)
		return
	}
	for _, k := range keys {
		if len(k) > maxKeyLen {
			fail(w, 400, "keys may be at most %d characters", maxKeyLen)
			return
		}
	}
	rec, u, err := s.resolve(r.Context(), keys, version)
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	if rec == nil {
		reply(w, http.StatusNoContent, nil)
		return
	}
	reply(w, 200, map[string]string{
		"cacheKey": rec.Key, "cacheVersion": rec.Version,
		"creationTime": rec.Created.UTC().Format(time.RFC3339Nano), "archiveLocation": u,
	})
}

// resolve finds the entry for keys (tried in order) and version, and presigns
// its download. It returns a nil record if there is none.
func (s *Service) resolve(ctx context.Context, keys []string, version string) (*record, string, error) {
	for {
		s.mu.Lock()
		rec := s.find(keys, version)
		s.mu.Unlock()
		if rec == nil {
			return nil, "", nil
		}
		if _, err := s.store.Stat(ctx, objectKey(rec.ID)); err != nil {
			if !errors.Is(err, blob.ErrNotFound) {
				return nil, "", fmt.Errorf("checking the stored entry: %w", err)
			}
			// The object is gone (someone cleaned the store): forget the entry.
			s.mu.Lock()
			delete(s.done, rec.ID)
			err := s.save()
			s.mu.Unlock()
			if err != nil {
				return nil, "", fmt.Errorf("saving the index: %w", err)
			}
			continue
		}
		u, err := s.store.PresignGet(ctx, objectKey(rec.ID), downloadTTL)
		if err != nil {
			return nil, "", err
		}
		s.touch(rec.ID)
		return rec, u, nil
	}
}

// list answers the diagnostic query the toolkit makes after a miss when step
// debugging is on.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	out := []map[string]string{}
	s.mu.Lock()
	for _, rec := range s.done {
		if strings.HasPrefix(rec.Key, key) {
			out = append(out, map[string]string{"cacheKey": rec.Key, "cacheVersion": rec.Version,
				"creationTime": rec.Created.UTC().Format(time.RFC3339Nano)})
		}
	}
	s.mu.Unlock()
	reply(w, 200, map[string]any{"totalCount": len(out), "artifactCaches": out})
}

func objectKey(id int64) string { return "cache/" + strconv.FormatInt(id, 10) }

func (s *Service) reserve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key       string `json:"key"`
		Version   string `json:"version"`
		CacheSize int64  `json:"cacheSize"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, 400, "malformed JSON: %v", err)
		return
	}
	if req.Key == "" || len(req.Key) > maxKeyLen || req.Version == "" {
		fail(w, 400, "a key of 1 to %d characters and a version are required", maxKeyLen)
		return
	}
	if req.CacheSize > maxEntrySize {
		fail(w, 400, "the entry is over the %d byte limit", int64(maxEntrySize))
		return
	}
	if s.maxBytes > 0 && req.CacheSize > s.maxBytes {
		fail(w, 400, "the entry is larger than this cache's %d byte limit", s.maxBytes)
		return
	}

	p, err := s.claim(req.Key, req.Version, false)
	switch {
	case errors.Is(err, errExists):
		fail(w, 409, "%v", err)
		return
	case errors.Is(err, errBusy):
		fail(w, 409, "%v", err)
		return
	case err != nil:
		fail(w, 500, "%v", err)
		return
	}
	reply(w, 201, map[string]int64{"cacheId": p.id})
}

var (
	errExists = errors.New("an entry with this key and version already exists")
	errBusy   = errors.New("another job is saving this entry")
)

// claim reserves key and version for a new upload. With signedURL the data
// arrives through a signed blob URL (v2) instead of as chunks (v1). An
// unfinished reservation older than reserveTTL is replaced.
func (s *Service) claim(key, version string, signedURL bool) (*pending, error) {
	now := s.now()
	var stale []*pending
	s.mu.Lock()
	for _, rec := range s.done {
		if rec.Key == key && rec.Version == version {
			s.mu.Unlock()
			return nil, errExists
		}
	}
	for id, p := range s.uploads {
		if p.key != key || p.version != version {
			continue
		}
		if now.Sub(p.reserved) < reserveTTL {
			s.mu.Unlock()
			return nil, errBusy
		}
		delete(s.uploads, id)
		stale = append(stale, p)
	}
	id := s.next
	s.next++
	p := &pending{id: id, key: key, version: version, reserved: now, chunks: map[int64]int64{}}
	if signedURL {
		p.upload = azblob.New(s.store, objectKey(id), "application/octet-stream",
			filepath.Join(s.spoolDir, "b"+strconv.FormatInt(id, 10)), s.partSize)
	}
	s.uploads[id] = p
	s.mu.Unlock()

	for _, q := range stale {
		q.abandon()
	}
	return p, nil
}

// freeMargin is the free space that must remain after an entry is stored.
const freeMargin = 256 << 20

var errNoRoom = errors.New("not enough room in the cache for this entry")

// saveEvery is how often the index is rewritten for the sake of last-use
// times alone (a commit or an eviction rewrites it anyway).
const saveEvery = 5 * time.Minute

// touch records that the entry was just used, so it is evicted last. The time
// reaches the disk with the next save, and at the latest saveEvery later, so
// that a restart does not make a busy entry look unused.
func (s *Service) touch(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.done[id]; r != nil {
		now := s.now()
		r.Used = now
		s.dirty = true
		if s.index != "" && now.Sub(s.lastSave) >= saveEvery {
			s.save() // best effort: the in-memory time is right either way
		}
	}
}

// EntryInfo describes a committed entry.
type EntryInfo struct {
	ID      int64
	Key     string
	Version string
	Size    int64
	// Used is when the entry was last hit (or made).
	Used time.Time
}

func infoOf(r *record) EntryInfo {
	return EntryInfo{ID: r.ID, Key: r.Key, Version: r.Version, Size: r.Size, Used: r.lastUsed()}
}

// Entries lists the committed entries.
func (s *Service) Entries() []EntryInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EntryInfo, 0, len(s.done))
	for _, r := range s.done {
		out = append(out, infoOf(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ReadIndex lists the entries in an index file without starting a service on
// it; a missing file has none.
func ReadIndex(path string) ([]EntryInfo, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f indexFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("cache index %s: %w", path, err)
	}
	out := make([]EntryInfo, 0, len(f.Entries))
	for _, r := range f.Entries {
		if r != nil {
			out = append(out, infoOf(r))
		}
	}
	return out, nil
}

// Evict removes one committed entry and its object (nothing if it is gone).
func (s *Service) Evict(ctx context.Context, id int64) error { return s.evict(ctx, id) }

// Expire evicts the entries not used since before, as GitHub does after seven
// days, and returns how many and how many bytes that freed.
func (s *Service) Expire(ctx context.Context, before time.Time) (n int, bytes int64, err error) {
	s.mu.Lock()
	var due []EntryInfo
	for _, r := range s.done {
		if r.lastUsed().Before(before) {
			due = append(due, infoOf(r))
		}
	}
	s.mu.Unlock()
	for _, e := range due {
		if err := s.evict(ctx, e.ID); err != nil {
			return n, bytes, err
		}
		n++
		bytes += e.Size
	}
	return n, bytes, nil
}

// Idle reports whether no upload is in progress.
func (s *Service) Idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads) == 0
}

// makeRoom evicts least recently used entries (never keep) until need more
// bytes fit under the cap and, if the disk is checked, until need bytes plus
// freeMargin are free. It fails if that cannot be done.
func (s *Service) makeRoom(ctx context.Context, need, keep int64) error {
	for {
		s.mu.Lock()
		var total int64
		var oldest *record
		for _, r := range s.done {
			total += r.Size
			if r.ID != keep && (oldest == nil || r.lastUsed().Before(oldest.lastUsed())) {
				oldest = r
			}
		}
		s.mu.Unlock()

		over := s.maxBytes > 0 && total+need > s.maxBytes
		short := false
		if s.free != nil {
			if f, err := s.free(); err == nil && f < need+freeMargin {
				short = true
			}
		}
		if !over && !short {
			return nil
		}
		if oldest == nil {
			return fmt.Errorf("%w (%d bytes needed, nothing left to evict)", errNoRoom, need)
		}
		if err := s.evict(ctx, oldest.ID); err != nil {
			return err
		}
	}
}

// evict removes a committed entry and its object.
func (s *Service) evict(ctx context.Context, id int64) error {
	s.mu.Lock()
	rec := s.done[id]
	if rec == nil {
		s.mu.Unlock()
		return nil
	}
	delete(s.done, id)
	err := s.save()
	if err != nil {
		s.done[id] = rec
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("saving the index: %w", err)
	}
	return s.store.Delete(context.WithoutCancel(ctx), objectKey(id))
}

// promote makes a finished upload visible as an entry. If the index cannot be
// saved it changes nothing and says so.
func (s *Service) promote(p *pending, size int64) error {
	rec := &record{ID: p.id, Key: p.key, Version: p.version, Size: size, Created: s.now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done[p.id] = rec
	delete(s.uploads, p.id)
	if err := s.save(); err != nil {
		delete(s.done, p.id)
		s.uploads[p.id] = p
		return err
	}
	return nil
}

// parseRange reads "bytes start-end/*".
func parseRange(h string) (start, end int64, ok bool) {
	rest, found := strings.CutPrefix(h, "bytes ")
	if !found {
		return 0, 0, false
	}
	span, _, _ := strings.Cut(rest, "/")
	a, b, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(a, 10, 64)
	end, err2 := strconv.ParseInt(b, 10, 64)
	return start, end, err1 == nil && err2 == nil && start >= 0 && end >= start
}

func (s *Service) pendingByID(w http.ResponseWriter, r *http.Request) *pending {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, "bad cache id")
		return nil
	}
	s.mu.Lock()
	p := s.uploads[id]
	s.mu.Unlock()
	if p == nil {
		fail(w, 404, "no such reservation")
	}
	return p
}

func (s *Service) chunk(w http.ResponseWriter, r *http.Request) {
	p := s.pendingByID(w, r)
	if p == nil {
		return
	}
	start, end, ok := parseRange(r.Header.Get("Content-Range"))
	if !ok {
		fail(w, 400, "a Content-Range of the form bytes start-end/* is required")
		return
	}
	n := end - start + 1
	if n > maxChunkSize || end >= maxEntrySize {
		fail(w, 400, "chunk too large")
		return
	}

	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		fail(w, 409, "the reservation is closed")
		return
	}
	if p.spool == "" {
		dir := filepath.Join(s.spoolDir, "c"+strconv.FormatInt(p.id, 10))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			p.mu.Unlock()
			fail(w, 500, "%v", err)
			return
		}
		p.spool = dir
	}
	dir := p.spool
	p.mu.Unlock()

	// The body is streamed, so Content-Length is usually absent: the range says
	// how much to expect.
	f, err := os.CreateTemp(dir, "chunk-*.tmp")
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	got, err := io.Copy(f, io.LimitReader(r.Body, n+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && got != n {
		fail(w, 400, "the body has %d bytes, the range says %d", got, n)
		os.Remove(f.Name())
		return
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, strconv.FormatInt(start, 10)))
	}
	if err != nil {
		os.Remove(f.Name())
		fail(w, 500, "%v", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		fail(w, 409, "the reservation is closed")
		return
	}
	p.chunks[start] = n
	reply(w, http.StatusNoContent, nil)
}

func (s *Service) commit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, "bad cache id")
		return
	}
	var req struct {
		Size int64 `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, 400, "malformed JSON: %v", err)
		return
	}
	s.mu.Lock()
	_, already := s.done[id]
	p := s.uploads[id]
	s.mu.Unlock()
	if already { // a retried commit
		reply(w, http.StatusNoContent, nil)
		return
	}
	if p == nil {
		fail(w, 404, "no such reservation")
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		fail(w, 409, "the reservation is closed")
		return
	}
	starts := make([]int64, 0, len(p.chunks))
	for st := range p.chunks {
		starts = append(starts, st)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	var off int64
	for _, st := range starts {
		switch {
		case st < off:
			fail(w, 400, "chunks overlap at byte %d", st)
			return
		case st > off:
			fail(w, 400, "chunks leave a gap at byte %d", off)
			return
		}
		off += p.chunks[st]
	}
	if off != req.Size || off == 0 {
		fail(w, 400, "the chunks hold %d bytes, the commit says %d", off, req.Size)
		return
	}

	if s.maxBytes > 0 && off > s.maxBytes {
		fail(w, 400, "the entry is larger than this cache's %d byte limit", s.maxBytes)
		return
	}
	if err := s.makeRoom(r.Context(), off, p.id); err != nil {
		fail(w, 507, "%v", err)
		return
	}
	if err := s.assemble(r.Context(), p, starts, off); err != nil {
		fail(w, 500, "storing the entry: %v", err)
		return
	}
	if err := s.promote(p, off); err != nil {
		s.store.Delete(context.WithoutCancel(r.Context()), objectKey(p.id))
		fail(w, 500, "saving the index: %v", err)
		return
	}
	p.done = true
	os.RemoveAll(p.spool)
	// Entries committed at the same moment each saw room for themselves; settle
	// the total now that both are in.
	s.makeRoom(r.Context(), 0, p.id)
	reply(w, http.StatusNoContent, nil)
}

// assemble joins the chunks and stores them as one object.
func (s *Service) assemble(ctx context.Context, p *pending, starts []int64, total int64) error {
	if (total+s.partSize-1)/s.partSize > blob.MaxParts {
		return errors.New("the entry is too large for the store")
	}
	var readers []io.Reader
	for _, st := range starts {
		f, err := os.Open(filepath.Join(p.spool, strconv.FormatInt(st, 10)))
		if err != nil {
			return err
		}
		defer f.Close()
		readers = append(readers, f)
	}
	up, err := s.store.BeginUpload(ctx, objectKey(p.id), "application/octet-stream")
	if err != nil {
		return err
	}
	r := io.MultiReader(readers...)
	var parts []blob.Part
	for rest, num := total, 1; rest > 0; num++ {
		n := min(s.partSize, rest)
		part, err := up.PutPart(ctx, num, io.LimitReader(r, n), n)
		if err != nil {
			up.Abort(context.WithoutCancel(ctx))
			return err
		}
		parts = append(parts, part)
		rest -= n
	}
	if err := up.Complete(ctx, parts); err != nil {
		up.Abort(context.WithoutCancel(ctx))
		return err
	}
	return nil
}
