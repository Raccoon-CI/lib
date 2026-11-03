// Package artifact serves the artifact protocol that actions/upload-artifact
// and download-artifact (v4 and later) speak, so they work unchanged against
// Raccoon CI instead of GitHub.
//
// There are two halves, as there are on GitHub. The metadata half is the
// Twirp service github.actions.results.api.v1.ArtifactService, reached
// through ACTIONS_RESULTS_URL and authenticated with ACTIONS_RUNTIME_TOKEN.
// The data half is Azure Blob Storage: CreateArtifact hands the client a
// "signed upload URL" and the client sends the zip there with the Azure SDK.
// We point that URL at ourselves and implement the two calls the SDK makes
// (Put Block, Put Block List), writing into a blob.Store. Downloads need no
// imitation: GetSignedArtifactURL returns whatever URL the store presigns.
package artifact

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Raccoon-CI/lib/internal/azblob"
	"github.com/Raccoon-CI/lib/internal/twirp"
	"github.com/Raccoon-CI/lib/pkg/blob"
)

const (
	twirpPrefix = "/twirp/github.actions.results.api.v1.ArtifactService/"
	uploadTTL   = 6 * time.Hour
	downloadTTL = time.Hour
	maxNameLen  = 255
	defaultKeep = 90 * 24 * time.Hour
)

// Record is what is remembered of a finished artifact (see Index).
type Record struct {
	ID      int64
	Run     string
	Job     string
	Name    string
	Key     string // where the data is in the store
	Mime    string
	Size    int64
	Digest  string
	Created time.Time
	Expires time.Time
}

// Index remembers finished artifacts, so that a restart does not forget them.
// An Index serves one Service (one store): the server gives each bucket its
// own. Uploads in progress are not remembered; they are lost with the process.
type Index interface {
	// Load returns every record, at start.
	Load(ctx context.Context) ([]Record, error)
	// Save stores a record when its artifact is finalized.
	Save(ctx context.Context, r Record) error
	// Delete forgets a record.
	Delete(ctx context.Context, id int64) error
}

// Service keeps artifact metadata in memory, and in an Index when there is one.
// Without an Index a restart forgets it, which is fine for a single
// `raccoonci run`; the server keeps it in its database.
type Service struct {
	store    blob.Store
	signer   *Signer
	index    Index
	keep     time.Duration
	maxKeep  time.Duration
	baseURL  string
	spoolDir string
	ownSpool bool
	partSize int64 // for the spool path; 0 means azblob's default
	free     func() (int64, error)
	minFree  int64

	mu     sync.Mutex
	nextID int64
	byID   map[int64]*artifact
	byName map[string]*artifact // run + "\x00" + name
}

// Config for New.
type Config struct {
	Store  blob.Store
	Signer *Signer
	// BaseURL is where jobs reach this service, e.g. http://127.0.0.1:4000.
	BaseURL string
	// SpoolDir is where the service makes a directory of its own for the blocks
	// of clients that don't follow the toolkit's block-id pattern (see
	// azblob.NewSpoolDir). Empty means the system temporary directory.
	SpoolDir string
	// Index, if set, remembers finished artifacts: they are loaded here and
	// saved when finalized.
	Index Index
	// Keep is how long an artifact is kept when the client does not say
	// (default 90 days); MaxKeep, if set, caps what a client may ask for.
	Keep    time.Duration
	MaxKeep time.Duration
	// Free, with MinFree, refuses to start a new artifact while the disk the
	// service writes to (its store and its spool) has less than MinFree bytes
	// available. Nil means no check (a bucket has no disk to fill, but the
	// spool does, so a server with a bucket may still pass the spool's disk).
	Free    func() (int64, error)
	MinFree int64
}

func New(c Config) (*Service, error) {
	if c.Store == nil || c.Signer == nil || c.BaseURL == "" {
		return nil, errors.New("artifact: Store, Signer and BaseURL are required")
	}
	d, err := azblob.NewSpoolDir(c.SpoolDir, "raccoonci-spool-")
	if err != nil {
		return nil, err
	}
	s := &Service{store: c.Store, signer: c.Signer, index: c.Index, maxKeep: c.MaxKeep, keep: c.Keep, free: c.Free, minFree: c.MinFree,
		baseURL: strings.TrimRight(c.BaseURL, "/"), spoolDir: d, ownSpool: true,
		byID: map[int64]*artifact{}, byName: map[string]*artifact{}}
	if s.keep <= 0 {
		s.keep = defaultKeep
	}
	if s.index != nil {
		recs, err := s.index.Load(context.Background())
		if err != nil {
			os.RemoveAll(d)
			return nil, fmt.Errorf("artifact: loading the index: %w", err)
		}
		for _, r := range recs {
			a := &artifact{id: r.ID, run: r.Run, job: r.Job, name: r.Name, key: r.Key, mime: r.Mime, expires: r.Expires,
				created: r.Created, finalized: true, size: r.Size, digest: r.Digest}
			s.byID[a.id] = a
			s.byName[key(a.run, a.name)] = a
			s.nextID = max(s.nextID, a.id)
		}
	}
	return s, nil
}

// Close abandons uploads that were never committed, so the store doesn't keep
// their parts.
func (s *Service) Close() {
	s.mu.Lock()
	all := make([]*artifact, 0, len(s.byID))
	for _, a := range s.byID {
		all = append(all, a)
	}
	s.mu.Unlock()
	for _, a := range all {
		a.mu.Lock()
		unfinished := !a.finalized && !a.gone
		a.mu.Unlock()
		if unfinished {
			a.abort(context.Background(), s.store)
		}
	}
	if s.ownSpool {
		os.RemoveAll(s.spoolDir)
	}
}

// Env is what a job's steps need to find the service.
func (s *Service) Env(sc Scope, ttl time.Duration) (map[string]string, error) {
	tok, err := s.signer.Token(sc, ttl)
	if err != nil {
		return nil, err
	}
	return map[string]string{"ACTIONS_RESULTS_URL": s.baseURL + "/", "ACTIONS_RUNTIME_TOKEN": tok}, nil
}

// Handler serves the Twirp API and the blob upload endpoint. Mount blob
// store download handlers (blob.Dir.Handler) next to it yourself.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+twirpPrefix+"{method}", s.twirp)
	mux.HandleFunc("PUT /_blob/{run}/{id}", s.putBlob)
	return mux
}

// artifact is one named upload of a run. The bytes are received by data.
type artifact struct {
	id      int64
	run     string
	job     string
	name    string
	key     string
	mime    string
	expires time.Time
	created time.Time
	data    *azblob.Upload

	mu        sync.Mutex // guards what follows; taken before data's own lock, never after
	finalized bool       // FinalizeArtifact seen: visible to listing and download
	gone      bool
	size      int64
	digest    string
}

// abort discards the artifact and whatever it left in the store. An artifact
// loaded from the Index has no upload (data is nil) and its object is deleted
// from st.
func (a *artifact) abort(ctx context.Context, st blob.Store) {
	a.mu.Lock()
	a.gone = true
	a.mu.Unlock()
	if a.data == nil {
		st.Delete(ctx, a.key)
		return
	}
	a.data.Discard(ctx)
}

// ---- Twirp ----

func (s *Service) twirp(w http.ResponseWriter, r *http.Request) {
	fail := func(e *twirp.Error) { twirp.WriteError(w, e) }

	sc, err := s.signer.FromRequest(r)
	if err != nil {
		fail(twirp.Errorf("unauthenticated", "%v", err))
		return
	}
	req, terr := twirp.ReadRequest(r)
	if terr != nil {
		fail(terr)
		return
	}

	// A token only opens the run it was issued for.
	if req.Str("workflow_run_backend_id", "workflowRunBackendId") != sc.Run {
		fail(twirp.Errorf("permission_denied", "the token does not cover this run"))
		return
	}
	job := req.Str("workflow_job_run_backend_id", "workflowJobRunBackendId")
	method := r.PathValue("method")
	var resp any
	switch method {
	case "CreateArtifact":
		if job != sc.Job {
			fail(twirp.Errorf("permission_denied", "the token does not cover this job"))
			return
		}
		resp, err = s.create(r.Context(), sc, req)
	case "FinalizeArtifact":
		resp, err = s.finalize(r.Context(), sc, req)
	case "ListArtifacts":
		resp, err = s.list(sc, req)
	case "GetSignedArtifactURL":
		resp, err = s.signedURL(r.Context(), sc, req)
	case "DeleteArtifact":
		resp, err = s.delete(r.Context(), sc, req)
	default:
		err = twirp.Errorf("bad_route", "no method %q", method)
	}
	if err != nil {
		var te *twirp.Error
		if !errors.As(err, &te) {
			te = twirp.Errorf("internal", "%v", err)
		}
		fail(te)
		return
	}
	twirp.WriteJSON(w, 200, resp)
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func key(run, name string) string { return run + "\x00" + name }

func (s *Service) create(ctx context.Context, sc Scope, req twirp.Request) (any, error) {
	name := req.Str("name")
	if name == "" || len(name) > maxNameLen {
		return nil, twirp.Errorf("invalid_argument", "the artifact name must be 1 to %d characters", maxNameLen)
	}
	if s.free != nil && s.minFree > 0 {
		// A failure to measure never blocks an upload.
		if free, err := s.free(); err == nil && free < s.minFree {
			return nil, twirp.Errorf("resource_exhausted", "the server's disk is nearly full (%d MiB free); no new artifacts are accepted until space is freed", free>>20)
		}
	}
	now := time.Now()
	keep := now.Add(s.keep)
	if e := req.Str("expires_at", "expiresAt"); e != "" {
		t, err := time.Parse(time.RFC3339Nano, e)
		if err != nil {
			return nil, twirp.Errorf("invalid_argument", "bad expires_at: %v", err)
		}
		keep = t
	}
	if s.maxKeep > 0 && keep.After(now.Add(s.maxKeep)) {
		keep = now.Add(s.maxKeep)
	}

	s.mu.Lock()
	old := s.byName[key(sc.Run, name)]
	if old != nil {
		old.mu.Lock()
		fin := old.finalized
		old.mu.Unlock()
		if fin {
			s.mu.Unlock()
			return nil, twirp.Errorf("already_exists", "an artifact named %q already exists in this run", name)
		}
	}
	s.nextID++
	a := &artifact{id: s.nextID, run: sc.Run, job: sc.Job, name: name, mime: req.Str("mime_type", "mimeType"),
		expires: keep, created: time.Now()}
	a.key = sc.Run + "/" + strconv.FormatInt(a.id, 10)
	a.data = azblob.New(s.store, a.key, a.mime, filepath.Join(s.spoolDir, strconv.FormatInt(a.id, 10)), s.partSize)
	s.byID[a.id] = a
	s.byName[key(sc.Run, name)] = a
	s.mu.Unlock()

	// An earlier attempt that never finished (the client retried) is replaced.
	if old != nil {
		old.abort(ctx, s.store)
		s.mu.Lock()
		delete(s.byID, old.id)
		s.mu.Unlock()
	}

	exp := time.Now().Add(uploadTTL).Unix()
	return map[string]any{
		"ok": true,
		"signed_upload_url": fmt.Sprintf("%s/_blob/%s/%d?exp=%d&sig=%s", blob.BaseURLFrom(ctx, s.baseURL), sc.Run, a.id, exp,
			s.signer.SignUpload(sc.Run, strconv.FormatInt(a.id, 10), exp)),
	}, nil
}

// find returns the live artifact of run with this name.
func (s *Service) find(run, name string) *artifact {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byName[key(run, name)]
}

func (s *Service) finalize(ctx context.Context, sc Scope, req twirp.Request) (any, error) {
	a := s.find(sc.Run, req.Str("name"))
	if a == nil {
		return nil, twirp.Errorf("not_found", "no artifact named %q", req.Str("name"))
	}
	size, _ := req.Int("size")
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.gone:
		return nil, twirp.Errorf("not_found", "no artifact named %q", req.Str("name"))
	case a.finalized:
		return map[string]any{"ok": true, "artifact_id": strconv.FormatInt(a.id, 10)}, nil
	case !a.data.Committed():
		return nil, twirp.Errorf("failed_precondition", "the artifact has no committed content")
	}
	info, err := s.store.Stat(ctx, a.key)
	if err != nil {
		return nil, twirp.Errorf("internal", "checking the stored artifact: %v", err)
	}
	if info.Size != size {
		return nil, twirp.Errorf("failed_precondition", "the artifact holds %d bytes, the client says %d", info.Size, size)
	}
	digest := req.Str("hash")
	if s.index != nil {
		rec := Record{ID: a.id, Run: a.run, Job: a.job, Name: a.name, Key: a.key, Mime: a.mime, Size: info.Size,
			Digest: digest, Created: a.created, Expires: a.expires}
		if err := s.index.Save(ctx, rec); err != nil {
			return nil, twirp.Errorf("internal", "remembering the artifact: %v", err)
		}
	}
	a.finalized, a.size, a.digest = true, info.Size, digest
	return map[string]any{"ok": true, "artifact_id": strconv.FormatInt(a.id, 10)}, nil
}

func (s *Service) list(sc Scope, req twirp.Request) (any, error) {
	name := req.Str("name_filter", "nameFilter")
	id, byID := req.Int("id_filter", "idFilter")
	s.mu.Lock()
	all := make([]*artifact, 0, len(s.byID))
	for _, a := range s.byID {
		if a.run == sc.Run {
			all = append(all, a)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(all, func(x, y *artifact) int { return int(x.id - y.id) })

	out := []map[string]any{}
	for _, a := range all {
		a.mu.Lock()
		if a.finalized && !a.gone && (name == "" || a.name == name) && (!byID || a.id == id) {
			m := map[string]any{
				"workflow_run_backend_id":     a.run,
				"workflow_job_run_backend_id": a.job,
				"database_id":                 strconv.FormatInt(a.id, 10),
				"name":                        a.name,
				"size":                        strconv.FormatInt(a.size, 10),
				"created_at":                  ts(a.created),
			}
			if a.digest != "" {
				m["digest"] = a.digest
			}
			out = append(out, m)
		}
		a.mu.Unlock()
	}
	return map[string]any{"artifacts": out}, nil
}

func (s *Service) signedURL(ctx context.Context, sc Scope, req twirp.Request) (any, error) {
	a := s.find(sc.Run, req.Str("name"))
	if a == nil {
		return nil, twirp.Errorf("not_found", "no artifact named %q", req.Str("name"))
	}
	a.mu.Lock()
	fin := a.finalized && !a.gone
	a.mu.Unlock()
	if !fin {
		return nil, twirp.Errorf("not_found", "no artifact named %q", req.Str("name"))
	}
	u, err := s.store.PresignGet(ctx, a.key, downloadTTL)
	if err != nil {
		return nil, err
	}
	return map[string]any{"signed_url": u}, nil
}

func (s *Service) delete(ctx context.Context, sc Scope, req twirp.Request) (any, error) {
	a := s.find(sc.Run, req.Str("name"))
	if a == nil {
		return nil, twirp.Errorf("not_found", "no artifact named %q", req.Str("name"))
	}
	if err := s.remove(ctx, a); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "artifact_id": strconv.FormatInt(a.id, 10)}, nil
}

// remove forgets an artifact and deletes its data. The index record goes
// first: if that fails nothing has been lost.
func (s *Service) remove(ctx context.Context, a *artifact) error {
	if s.index != nil {
		a.mu.Lock()
		fin := a.finalized
		a.mu.Unlock()
		if fin {
			if err := s.index.Delete(ctx, a.id); err != nil {
				return twirp.Errorf("internal", "forgetting the artifact: %v", err)
			}
		}
	}
	a.abort(ctx, s.store)
	s.mu.Lock()
	delete(s.byID, a.id)
	if s.byName[key(a.run, a.name)] == a {
		delete(s.byName, key(a.run, a.name))
	}
	s.mu.Unlock()
	return nil
}

// Info describes a finished artifact.
type Info struct {
	ID      int64
	Run     string
	Job     string
	Name    string
	Size    int64
	Digest  string
	Created time.Time
	Expires time.Time
}

// Artifacts lists the finished artifacts of a run, oldest first.
func (s *Service) Artifacts(run string) []Info {
	s.mu.Lock()
	all := make([]*artifact, 0)
	for _, a := range s.byID {
		if a.run == run {
			all = append(all, a)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(all, func(x, y *artifact) int { return int(x.id - y.id) })
	out := []Info{}
	for _, a := range all {
		a.mu.Lock()
		if a.finalized && !a.gone {
			out = append(out, Info{ID: a.id, Run: a.run, Job: a.job, Name: a.name, Size: a.size, Digest: a.digest, Created: a.created, Expires: a.expires})
		}
		a.mu.Unlock()
	}
	return out
}

// ErrNoArtifact is returned for an artifact that does not exist (or is not
// finished).
var ErrNoArtifact = errors.New("artifact: no such artifact")

// DownloadURL presigns a link to a finished artifact of a run.
func (s *Service) DownloadURL(ctx context.Context, run string, id int64, ttl time.Duration) (string, error) {
	s.mu.Lock()
	a := s.byID[id]
	s.mu.Unlock()
	if a == nil || a.run != run {
		return "", ErrNoArtifact
	}
	a.mu.Lock()
	ok := a.finalized && !a.gone
	a.mu.Unlock()
	if !ok {
		return "", ErrNoArtifact
	}
	return s.store.PresignGet(ctx, a.key, ttl)
}

// Expire deletes the finished artifacts whose time to keep has passed and
// returns how many. An artifact whose record could not be forgotten stays.
func (s *Service) Expire(ctx context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	var due []*artifact
	for _, a := range s.byID {
		if !a.expires.IsZero() && !a.expires.After(now) {
			due = append(due, a)
		}
	}
	s.mu.Unlock()
	n := 0
	var firstErr error
	for _, a := range due {
		a.mu.Lock()
		fin := a.finalized && !a.gone
		a.mu.Unlock()
		if !fin { // an upload in progress is not expired by the clock
			continue
		}
		if err := s.remove(ctx, a); err != nil {
			firstErr = cmpErr(firstErr, err)
			continue
		}
		n++
	}
	return n, firstErr
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}
