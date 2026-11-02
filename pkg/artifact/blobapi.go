package artifact

import (
	"net/http"
	"strconv"

	"github.com/Raccoon-CI/raccoonci/internal/azblob"
)

// putBlob handles the calls @azure/storage-blob makes against the signed
// upload URL. The URL's signature says which artifact it may write to.
func (s *Service) putBlob(w http.ResponseWriter, r *http.Request) {
	run, idStr := r.PathValue("run"), r.PathValue("id")
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || !s.signer.VerifyUpload(run, idStr, exp, q.Get("sig")) {
		azblob.Fail(w, 403, "AuthenticationFailed", "invalid or expired upload URL")
		return
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)
	s.mu.Lock()
	a := s.byID[id]
	s.mu.Unlock()
	if a == nil || a.run != run {
		azblob.Fail(w, 404, "BlobNotFound", "no such upload")
		return
	}
	if a.data == nil { // loaded from the index: finished long ago
		azblob.Fail(w, 409, "BlobAlreadyExists", "the artifact is already uploaded")
		return
	}
	a.data.ServeHTTP(w, r)
}
