package cache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Raccoon-CI/raccoonci/internal/azblob"
	"github.com/Raccoon-CI/raccoonci/internal/twirp"
	"github.com/Raccoon-CI/raccoonci/pkg/blob"
)

// The v2 protocol: the Twirp service github.actions.results.api.v1.CacheService
// at ACTIONS_RESULTS_URL, and Azure Blob for the data. CreateCacheEntry hands
// out a signed upload URL, the client sends the archive there with
// @azure/storage-blob (one Put Blob up to 128 MiB, blocks above that), and
// FinalizeCacheEntryUpload turns the upload into an entry. Entries are the
// same ones v1 serves: the version string is computed the same way by both.
const (
	twirpPrefix = "/twirp/github.actions.results.api.v1.CacheService/"
	blobPath    = "/_cache_blob/data/"
)

func (s *Service) twirp(w http.ResponseWriter, r *http.Request) {
	if _, err := s.signer.FromRequest(r); err != nil {
		twirp.WriteError(w, twirp.Errorf("unauthenticated", "%v", err))
		return
	}
	req, terr := twirp.ReadRequest(r)
	if terr != nil {
		twirp.WriteError(w, terr)
		return
	}
	var resp any
	var err error
	switch m := r.PathValue("method"); m {
	case "CreateCacheEntry":
		resp, err = s.createEntry(r.Context(), req)
	case "FinalizeCacheEntryUpload":
		resp, err = s.finalizeEntry(r.Context(), req)
	case "GetCacheEntryDownloadURL":
		resp, err = s.downloadURL(r.Context(), req)
	default:
		err = twirp.Errorf("bad_route", "no method %q", m)
	}
	if err != nil {
		var te *twirp.Error
		if !errors.As(err, &te) {
			te = twirp.Errorf("internal", "%v", err)
		}
		twirp.WriteError(w, te)
		return
	}
	twirp.WriteJSON(w, 200, resp)
}

// The client reads "ok" and "message" of a response to tell a refusal (the
// entry exists, another job is saving it) from a failure, so refusals are
// ordinary replies and only malformed requests and server faults are errors.

func (s *Service) createEntry(ctx context.Context, req twirp.Request) (any, error) {
	key, version := req.Str("key"), req.Str("version")
	if key == "" || len(key) > maxKeyLen || version == "" {
		return nil, twirp.Errorf("invalid_argument", "a key of 1 to %d characters and a version are required", maxKeyLen)
	}
	p, err := s.claim(key, version, true)
	switch {
	case errors.Is(err, errExists), errors.Is(err, errBusy):
		return map[string]any{"ok": false, "signed_upload_url": "", "message": err.Error()}, nil
	case err != nil:
		return nil, err
	}
	id := strconv.FormatInt(p.id, 10)
	exp := s.now().Add(uploadTTL).Unix()
	return map[string]any{
		"ok": true, "message": "",
		"signed_upload_url": fmt.Sprintf("%s%s%s?exp=%d&sig=%s", blob.BaseURLFrom(ctx, s.baseURL), blobPath, id, exp,
			s.signer.SignUpload(uploadRealm, id, exp)),
	}, nil
}

func (s *Service) finalizeEntry(ctx context.Context, req twirp.Request) (any, error) {
	key, version := req.Str("key"), req.Str("version")
	size, ok := req.Int("size_bytes", "sizeBytes")
	if key == "" || version == "" || !ok || size <= 0 {
		return nil, twirp.Errorf("invalid_argument", "key, version and a positive size_bytes are required")
	}
	refuse := func(format string, a ...any) (any, error) {
		return map[string]any{"ok": false, "entry_id": "0", "message": fmt.Sprintf(format, a...)}, nil
	}

	var rec *record
	var p *pending
	s.mu.Lock()
	for _, r := range s.done {
		if r.Key == key && r.Version == version {
			rec = r
			break
		}
	}
	if rec == nil {
		for _, q := range s.uploads {
			if q.key == key && q.version == version && q.upload != nil {
				p = q
				break
			}
		}
	}
	s.mu.Unlock()

	if rec != nil {
		if rec.Size != size { // not the upload this client made
			return refuse("an entry with this key and version already exists")
		}
		return map[string]any{"ok": true, "entry_id": strconv.FormatInt(rec.ID, 10), "message": ""}, nil // a retried finalize
	}
	if p == nil {
		return refuse("no upload was started for this key and version")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return refuse("the upload is closed")
	}
	if !p.upload.Committed() {
		return refuse("the upload has no committed content")
	}
	info, err := s.store.Stat(ctx, objectKey(p.id))
	if err != nil {
		return nil, twirp.Errorf("internal", "checking the stored entry: %v", err)
	}
	if info.Size != size {
		return refuse("the entry holds %d bytes, the client says %d", info.Size, size)
	}
	if s.maxBytes > 0 && size > s.maxBytes {
		s.store.Delete(context.WithoutCancel(ctx), objectKey(p.id))
		return refuse("the entry is larger than this cache's %d byte limit", s.maxBytes)
	}
	if err := s.promote(p, size); err != nil {
		return nil, twirp.Errorf("internal", "saving the index: %v", err)
	}
	p.done = true
	// The data is stored already (a single Put Blob), so evict afterwards: the
	// least recently used entries go until the cap holds again.
	if err := s.makeRoom(ctx, 0, p.id); err != nil {
		return nil, twirp.Errorf("internal", "evicting old entries: %v", err)
	}
	return map[string]any{"ok": true, "entry_id": strconv.FormatInt(p.id, 10), "message": ""}, nil
}

func (s *Service) downloadURL(ctx context.Context, req twirp.Request) (any, error) {
	key, version := req.Str("key"), req.Str("version")
	if key == "" || version == "" {
		return nil, twirp.Errorf("invalid_argument", "a key and a version are required")
	}
	keys := []string{key}
	for _, k := range req.Strings("restore_keys", "restoreKeys") {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) > maxKeys {
		return nil, twirp.Errorf("invalid_argument", "at most %d keys", maxKeys)
	}
	for _, k := range keys {
		if len(k) > maxKeyLen {
			return nil, twirp.Errorf("invalid_argument", "keys may be at most %d characters", maxKeyLen)
		}
	}
	rec, u, err := s.resolve(ctx, keys, version)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return map[string]any{"ok": false, "signed_download_url": "", "matched_key": ""}, nil
	}
	return map[string]any{"ok": true, "signed_download_url": u, "matched_key": rec.Key}, nil
}

// putBlob receives the data of a v2 upload, at the URL CreateCacheEntry handed out.
func (s *Service) putBlob(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || !s.signer.VerifyUpload(uploadRealm, idStr, exp, q.Get("sig")) {
		azblob.Fail(w, 403, "AuthenticationFailed", "invalid or expired upload URL")
		return
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)
	s.mu.Lock()
	p := s.uploads[id]
	s.mu.Unlock()
	if p == nil || p.upload == nil {
		azblob.Fail(w, 404, "BlobNotFound", "no such upload")
		return
	}
	p.upload.ServeHTTP(w, r)
}
