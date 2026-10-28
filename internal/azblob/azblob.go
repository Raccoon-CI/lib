// Package azblob receives one blob from a client that thinks it is talking to
// Azure Blob Storage, and writes it into a blob.Store.
//
// @actions/artifact and @actions/cache send their data with @azure/storage-blob
// to a "signed URL" the service hands out. An Upload sits behind one such URL
// and implements the calls the SDK makes: Put Block and Put Block List (for
// large files, sent in blocks of the SDK's choosing) and Put Blob (for files
// small enough to go in one request).
//
// If the first block's id has the SDK's shape, base64(36-char prefix + 12
// digits of block index), block i is forwarded as part i+1 of a multipart
// upload without touching the disk, and the block list must be exactly
// 0..n-1 in order. Any other id shape takes a spool path: blocks wait on disk
// and are cut into parts of their own at commit.
package azblob

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"raccoonci/internal/blob"
)

const (
	// DefaultPartSize is the size of the parts the spool path cuts a blob into.
	DefaultPartSize = 8 << 20
	// MaxRequestSize bounds one Put Block or Put Blob body. Azure allows more;
	// the SDKs send blocks of 8 to 64 MiB and single shots of up to 128 MiB.
	MaxRequestSize = 256 << 20
)

// Mode says how the blocks of an upload reach the store. The first request fixes it.
type Mode int

const (
	ModeNone   Mode = iota // nothing received yet
	ModeDirect             // block ids follow the SDK's pattern: forwarded as parts
	ModeSpool              // any other ids: staged on disk until the block list
	ModeSingle             // one Put Blob
)

type state int

const (
	pending state = iota // data may arrive
	committed
	gone
)

// Error is a failure in Azure's vocabulary: an HTTP status and a code such as
// InvalidBlockList. The SDK retries 5xx and network errors and gives up on the rest.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func errf(status int, code, msg string) *Error { return &Error{status, code, msg} }

// Fail answers a request with an Azure-style error.
func Fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-ms-error-code", code)
	w.WriteHeader(status)
	xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string
		Message string
	}{Code: code, Message: msg})
}

// Upload is one blob being received. Methods are safe for concurrent use.
type Upload struct {
	store       blob.Store
	key         string
	contentType string
	spoolDir    string
	partSize    int64

	mu     sync.Mutex
	st     state
	mode   Mode
	prefix string            // direct: the 36 characters every block id starts with
	up     blob.Upload       // direct, or the finished spool upload
	parts  map[int]blob.Part // direct: part number -> part
	blocks map[string]int64  // spool: block id -> size
	spool  string            // spool: the directory in use
}

// New prepares an upload that will end up under key in store. spoolDir is
// created only if the spool path is taken. A partSize of 0 means DefaultPartSize.
func New(store blob.Store, key, contentType, spoolDir string, partSize int64) *Upload {
	if partSize <= 0 {
		partSize = DefaultPartSize
	}
	return &Upload{store: store, key: key, contentType: contentType, spoolDir: spoolDir, partSize: partSize}
}

func (u *Upload) Mode() Mode {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.mode
}

// Committed reports whether the blob exists in the store.
func (u *Upload) Committed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.st == committed
}

// SpoolDir is the directory the spool path used, or "" if it was never taken.
func (u *Upload) SpoolDir() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.spool
}

// parseBlockID reads an id the way @azure/storage-blob writes it:
// base64(<36-char prefix><12 digits of block index>).
func parseBlockID(id string) (prefix string, idx int, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil || len(raw) != 48 {
		return "", 0, false
	}
	for _, c := range raw[36:] {
		if c < '0' || c > '9' {
			return "", 0, false
		}
	}
	n, err := strconv.Atoi(string(raw[36:]))
	if err != nil || n >= blob.MaxParts {
		return "", 0, false
	}
	return string(raw[:36]), n, true
}

// PutBlock stages one block of n bytes.
func (u *Upload) PutBlock(ctx context.Context, blockID string, body io.Reader, n int64) *Error {
	u.mu.Lock()
	if u.st != pending {
		u.mu.Unlock()
		return errf(409, "BlobNotWritable", "the blob is already committed or discarded")
	}
	if u.mode == ModeNone {
		if p, _, ok := parseBlockID(blockID); ok {
			u.mode, u.prefix = ModeDirect, p
		} else {
			u.mode = ModeSpool
		}
	}

	if u.mode == ModeSpool {
		if u.spool == "" {
			if err := os.MkdirAll(u.spoolDir, 0o700); err != nil {
				u.mu.Unlock()
				return errf(500, "InternalError", err.Error())
			}
			u.spool = u.spoolDir
		}
		dir := u.spool
		u.mu.Unlock()
		name := filepath.Join(dir, blockFile(blockID))
		tmp := name + "." + strconv.FormatInt(time.Now().UnixNano(), 36) + ".tmp"
		if err := copyFile(tmp, body, n); err != nil {
			os.Remove(tmp)
			return errf(500, "InternalError", err.Error())
		}
		if err := os.Rename(tmp, name); err != nil {
			os.Remove(tmp)
			return errf(500, "InternalError", err.Error())
		}
		u.mu.Lock()
		if u.blocks == nil {
			u.blocks = map[string]int64{}
		}
		u.blocks[blockID] = n
		u.mu.Unlock()
		return nil
	}

	p, idx, ok := parseBlockID(blockID)
	if !ok || p != u.prefix {
		u.mu.Unlock()
		return errf(400, "InvalidBlockId", "block ids must all follow the pattern of the first one")
	}
	if u.up == nil {
		up, err := u.store.BeginUpload(ctx, u.key, u.contentType)
		if err != nil {
			u.mu.Unlock()
			return errf(500, "InternalError", err.Error())
		}
		u.up = up
	}
	up := u.up
	u.mu.Unlock()

	part, err := up.PutPart(ctx, idx+1, body, n)
	if err != nil {
		return errf(500, "InternalError", err.Error())
	}
	u.mu.Lock()
	if u.st == pending && u.up == up {
		if u.parts == nil {
			u.parts = map[int]blob.Part{}
		}
		u.parts[idx+1] = part
	}
	u.mu.Unlock()
	return nil
}

func blockFile(blockID string) string {
	sum := sha256.Sum256([]byte(blockID))
	return hex.EncodeToString(sum[:])
}

func copyFile(name string, r io.Reader, n int64) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	got, err := io.Copy(f, io.LimitReader(r, n+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && got != n {
		err = fmt.Errorf("block has %d bytes, Content-Length said %d", got, n)
	}
	return err
}

// PutBlob stores the whole blob, n bytes, in one go. Repeating a Put Blob that
// already succeeded does nothing, since a client retries after a lost reply.
func (u *Upload) PutBlob(ctx context.Context, body io.Reader, n int64) *Error {
	if n <= 0 {
		return errf(400, "InvalidInput", "the blob is empty")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.st == committed && u.mode == ModeSingle:
		return nil
	case u.st != pending:
		return errf(409, "BlobNotWritable", "the blob is already committed or discarded")
	case u.mode != ModeNone:
		return errf(409, "BlobHasBlocks", "blocks were already staged for this blob")
	}
	up, err := u.store.BeginUpload(ctx, u.key, u.contentType)
	if err != nil {
		return errf(500, "InternalError", err.Error())
	}
	part, err := up.PutPart(ctx, 1, body, n)
	if err == nil {
		err = up.Complete(ctx, []blob.Part{part})
	}
	if err != nil {
		up.Abort(context.WithoutCancel(ctx))
		return errf(500, "InternalError", err.Error())
	}
	u.mode, u.st = ModeSingle, committed
	return nil
}

// Commit takes the block list and makes the blob exist. Blocks that were
// staged but are not listed are dropped, as Azure drops uncommitted blocks.
func (u *Upload) Commit(ctx context.Context, ids []string) *Error {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch u.st {
	case committed:
		if u.mode == ModeSingle {
			return errf(409, "BlobNotWritable", "the blob was written with Put Blob")
		}
		return nil // a retried request
	case pending:
	default:
		return errf(409, "BlobNotWritable", "the blob is already discarded")
	}
	if len(ids) == 0 || u.mode == ModeNone {
		return errf(400, "InvalidBlockList", "the block list is empty")
	}

	var err error
	if u.mode == ModeDirect {
		parts := make([]blob.Part, len(ids))
		for i, id := range ids {
			p, idx, ok := parseBlockID(id)
			if !ok || p != u.prefix || idx != i {
				return errf(400, "InvalidBlockList", "blocks must be numbered 0..n-1 and listed in order")
			}
			part, ok := u.parts[i+1]
			if !ok {
				return errf(400, "InvalidBlockList", fmt.Sprintf("block %d was never uploaded", i))
			}
			parts[i] = part
		}
		err = u.up.Complete(ctx, parts)
	} else {
		err = u.commitSpool(ctx, ids)
	}
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) {
			return ae
		}
		var be *blob.Error
		if errors.As(err, &be) && (be.Code == "EntityTooSmall" || be.Code == "InvalidPart") {
			return errf(400, "InvalidBlockList", "the store refused the parts: "+be.Message)
		}
		return errf(500, "InternalError", err.Error())
	}
	u.st = committed
	return nil
}

func (u *Upload) commitSpool(ctx context.Context, ids []string) error {
	var total int64
	var readers []io.Reader
	for _, id := range ids {
		n, ok := u.blocks[id]
		if !ok {
			return errf(400, "InvalidBlockList", "a block in the list was never uploaded")
		}
		total += n
		f, err := os.Open(filepath.Join(u.spool, blockFile(id)))
		if err != nil {
			return err
		}
		defer f.Close()
		readers = append(readers, f)
	}
	if total == 0 {
		return errf(400, "InvalidBlockList", "the blob is empty")
	}
	if (total+u.partSize-1)/u.partSize > blob.MaxParts {
		return errf(400, "RequestBodyTooLarge", "the blob is too large")
	}
	up, err := u.store.BeginUpload(ctx, u.key, u.contentType)
	if err != nil {
		return err
	}
	r := io.MultiReader(readers...)
	var parts []blob.Part
	for rest, num := total, 1; rest > 0; num++ {
		n := min(u.partSize, rest)
		p, err := up.PutPart(ctx, num, io.LimitReader(r, n), n)
		if err != nil {
			up.Abort(context.WithoutCancel(ctx))
			return err
		}
		parts = append(parts, p)
		rest -= n
	}
	if err := up.Complete(ctx, parts); err != nil {
		up.Abort(context.WithoutCancel(ctx))
		return err
	}
	u.up = up
	os.RemoveAll(u.spool)
	return nil
}

// Discard throws the upload away: staged parts, the spool, and the blob itself
// if it was already committed. Later writes are refused.
func (u *Upload) Discard(ctx context.Context) {
	u.mu.Lock()
	defer u.mu.Unlock()
	prev := u.st
	u.st = gone
	switch prev {
	case pending:
		if u.up != nil {
			u.up.Abort(ctx)
		}
	case committed:
		u.store.Delete(ctx, u.key)
	}
	if u.spool != "" {
		os.RemoveAll(u.spool)
	}
}

// blockList is the body of Put Block List.
type blockList struct {
	// Committed, Uncommitted and Latest all name a block id. Only blocks
	// staged in this upload can be named, so the three mean the same here.
	IDs []struct {
		XMLName xml.Name
		ID      string `xml:",chardata"`
	} `xml:",any"`
}

// ServeHTTP handles a PUT on the upload's URL: Put Block (comp=block), Put
// Block List (comp=blocklist), or Put Blob (no comp). The caller has already
// checked that the URL is allowed to write here.
func (u *Upload) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("x-ms-version", "2021-08-06")
	h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	q := r.URL.Query()

	body := func() bool { // checks the declared length of a Put Block or Put Blob
		if r.ContentLength < 0 {
			Fail(w, 411, "MissingContentLengthHeader", "Content-Length is required")
			return false
		}
		if r.ContentLength > MaxRequestSize {
			Fail(w, 413, "RequestBodyTooLarge", "the request body is too large")
			return false
		}
		return true
	}
	switch q.Get("comp") {
	case "block":
		blockID := q.Get("blockid")
		if blockID == "" || len(blockID) > 64 {
			Fail(w, 400, "InvalidQueryParameterValue", "bad blockid")
			return
		}
		if !body() {
			return
		}
		if e := u.PutBlock(r.Context(), blockID, r.Body, r.ContentLength); e != nil {
			Fail(w, e.Status, e.Code, e.Msg)
			return
		}
	case "blocklist":
		b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		var bl blockList
		if err != nil || xml.Unmarshal(b, &bl) != nil {
			Fail(w, 400, "InvalidXmlDocument", "malformed block list")
			return
		}
		ids := make([]string, len(bl.IDs))
		for i, e := range bl.IDs {
			ids[i] = e.ID
		}
		if e := u.Commit(r.Context(), ids); e != nil {
			Fail(w, e.Status, e.Code, e.Msg)
			return
		}
		h.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		h.Set("ETag", `"0x8DC0000000000"`)
	case "":
		if t := r.Header.Get("x-ms-blob-type"); t != "BlockBlob" {
			Fail(w, 400, "InvalidHeaderValue", "x-ms-blob-type must be BlockBlob, got "+strconv.Quote(t))
			return
		}
		if !body() {
			return
		}
		if e := u.PutBlob(r.Context(), r.Body, r.ContentLength); e != nil {
			Fail(w, e.Status, e.Code, e.Msg)
			return
		}
		h.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		h.Set("ETag", `"0x8DC0000000000"`)
	default:
		Fail(w, 501, "UnsupportedHttpVerb", "only Put Block, Put Block List and Put Blob are supported")
		return
	}
	w.WriteHeader(http.StatusCreated)
}
