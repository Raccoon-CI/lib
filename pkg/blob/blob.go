// Package blob is where artifact bytes live: an object store reached through a
// small interface, with an S3-compatible implementation.
//
// The interface is shaped by what the artifact service needs. Uploads are
// multipart so a block can be forwarded as it arrives, without landing on
// disk, and downloads are presigned URLs so the bytes never pass through the
// Raccoon CI server.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrNotFound is returned (wrapped in an *Error) when an object or upload does
// not exist.
var ErrNotFound = errors.New("blob: not found")

// Limits of S3 multipart uploads. Other S3-compatible stores follow them.
const (
	MaxParts    = 10000
	MinPartSize = 5 << 20 // every part but the last
)

// Info describes a stored object.
type Info struct {
	Size        int64
	ETag        string
	ContentType string
}

// Part is one finished part of a multipart upload. Numbers start at 1.
type Part struct {
	Number int
	ETag   string
}

// Store keeps objects by key.
type Store interface {
	// BeginUpload starts a multipart upload. The object is not visible
	// until Upload.Complete succeeds.
	BeginUpload(ctx context.Context, key, contentType string) (Upload, error)
	// PresignGet returns a URL that downloads the object without further
	// credentials until ttl runs out. A store served by Raccoon CI itself
	// (Dir) builds it on BaseURLFrom(ctx).
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// Stat reports an object's size; ErrNotFound if it does not exist.
	Stat(ctx context.Context, key string) (Info, error)
	// Delete removes an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
}

// Upload is one multipart upload in progress. Parts may be sent concurrently
// and in any order.
type Upload interface {
	// ID identifies the upload at the store, so it can be resumed or
	// aborted from another process.
	ID() string
	// PutPart sends exactly size bytes from r as part n and returns the
	// part's ETag. Sending the same number again replaces the part.
	PutPart(ctx context.Context, n int, r io.Reader, size int64) (Part, error)
	// Complete assembles the parts, in number order, into the object.
	// Parts that were sent but are not listed are discarded.
	Complete(ctx context.Context, parts []Part) error
	// Abort throws the upload and its parts away.
	Abort(ctx context.Context) error
}

// Error is a failure reported by the store.
type Error struct {
	Op      string
	Key     string
	Status  int    // HTTP status
	Code    string // store's error code, e.g. NoSuchKey
	Message string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("blob: %s %s: HTTP %d", e.Op, e.Key, e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is makes errors.Is(err, ErrNotFound) work.
func (e *Error) Is(target error) bool {
	return target == ErrNotFound && (e.Status == 404 || e.Code == "NoSuchKey" || e.Code == "NoSuchUpload")
}

type baseURLKey struct{}

// WithBaseURL records in ctx the address (scheme://host:port) a client used
// to reach Raccoon CI's server, so the upload and download links handed to it
// point back there. One server may listen on several addresses, for example
// on loopback for host jobs and on a docker network for a job container, and
// a link to one of them is useless from the other side. Set it from the
// listener (http.Server.BaseContext), never from the request's Host header.
func WithBaseURL(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, baseURLKey{}, strings.TrimRight(base, "/"))
}

// BaseURLFrom is the address WithBaseURL recorded in ctx, or fallback.
func BaseURLFrom(ctx context.Context, fallback string) string {
	if b, ok := ctx.Value(baseURLKey{}).(string); ok && b != "" {
		return b
	}
	return strings.TrimRight(fallback, "/")
}
