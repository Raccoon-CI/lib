package blob

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is just enough of S3, path-style, for the calls S3 makes.
type fakeS3 struct {
	t       *testing.T
	mu      sync.Mutex
	uploads map[string]map[int][]byte // upload id -> part number -> data
	keys    map[string]string         // upload id -> key
	objects map[string][]byte
	types   map[string]string
	// completeError makes Complete answer 200 with an <Error> body.
	completeError bool
	// failParts makes that many part requests answer 503 before they succeed.
	failParts int
	// gotUnsigned records the payload hash header of each part request.
	gotUnsigned []string
	nextID      int
}

func newFake(t *testing.T) (*fakeS3, *S3) {
	f := &fakeS3{t: t, uploads: map[string]map[int][]byte{}, keys: map[string]string{},
		objects: map[string][]byte{}, types: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s, err := NewS3(S3Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "AK", SecretKey: "SK", PathStyle: true, Prefix: "art/"})
	if err != nil {
		t.Fatal(err)
	}
	return f, s
}

func etag(b []byte) string { h := md5.Sum(b); return `"` + hex.EncodeToString(h[:]) + `"` }

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	// Presigned requests carry the signature in the query, others in a header.
	if q.Get("X-Amz-Signature") == "" && !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		f.t.Errorf("unsigned request: %s %s", r.Method, r.URL)
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/bkt/")
	if !ok {
		http.Error(w, "bad path", 400)
		return
	}
	switch {
	case r.Method == "POST" && q.Has("uploads"):
		f.nextID++
		id := fmt.Sprintf("up%d", f.nextID)
		f.uploads[id], f.keys[id] = map[int][]byte{}, key
		f.types[id] = r.Header.Get("Content-Type")
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == "PUT" && q.Has("partNumber"):
		parts, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			noSuchUpload(w)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		body, _ := io.ReadAll(r.Body)
		if f.failParts > 0 {
			f.failParts--
			http.Error(w, "<Error><Code>SlowDown</Code><Message>try again</Message></Error>", 503)
			return
		}
		if int64(len(body)) != r.ContentLength {
			f.t.Errorf("part body %d bytes, Content-Length %d", len(body), r.ContentLength)
		}
		f.gotUnsigned = append(f.gotUnsigned, r.Header.Get("X-Amz-Content-Sha256"))
		parts[n] = body
		w.Header().Set("ETag", etag(body))
	case r.Method == "POST" && q.Has("uploadId"):
		id := q.Get("uploadId")
		parts, ok := f.uploads[id]
		if !ok {
			noSuchUpload(w)
			return
		}
		if f.completeError {
			fmt.Fprint(w, `<Error><Code>EntityTooSmall</Code><Message>part too small</Message></Error>`)
			return
		}
		var req struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var all []byte
		last := 0
		for _, p := range req.Parts {
			if p.PartNumber <= last {
				f.t.Errorf("parts not ascending: %d after %d", p.PartNumber, last)
			}
			last = p.PartNumber
			data, ok := parts[p.PartNumber]
			if !ok || etag(data) != p.ETag {
				w.WriteHeader(400)
				fmt.Fprint(w, `<Error><Code>InvalidPart</Code><Message>bad part</Message></Error>`)
				return
			}
			all = append(all, data...)
		}
		f.objects[f.keys[id]] = all
		f.types[f.keys[id]] = f.types[id]
		delete(f.uploads, id)
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Key>%s</Key></CompleteMultipartUploadResult>`, key)
	case r.Method == "DELETE" && q.Has("uploadId"):
		if _, ok := f.uploads[q.Get("uploadId")]; !ok {
			noSuchUpload(w)
			return
		}
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(204)
	case r.Method == "DELETE":
		delete(f.objects, key)
		w.WriteHeader(204)
	case r.Method == "HEAD" || r.Method == "GET":
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			if r.Method == "GET" {
				fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
			}
			return
		}
		w.Header().Set("ETag", etag(data))
		w.Header().Set("Content-Type", f.types[key])
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == "GET" {
			w.Write(data)
		}
	default:
		http.Error(w, "unsupported", 400)
	}
}

func noSuchUpload(w http.ResponseWriter) {
	w.WriteHeader(404)
	fmt.Fprint(w, `<Error><Code>NoSuchUpload</Code><Message>gone</Message></Error>`)
}

func TestMultipartRoundTrip(t *testing.T) {
	f, s := newFake(t)
	ctx := context.Background()
	// A key that needs escaping in both the path and the signature.
	key := "run 1/job#2/名字 (v4).zip"

	up, err := s.BeginUpload(ctx, key, "application/zip")
	if err != nil {
		t.Fatal(err)
	}
	if up.ID() == "" {
		t.Fatal("empty upload id")
	}
	chunks := [][]byte{bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("b"), 50), []byte("tail")}
	// Parts arrive concurrently and out of order.
	var wg sync.WaitGroup
	parts := make([]Part, len(chunks))
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
	if t.Failed() {
		return
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("object visible before Complete: %v", err)
	}
	// Listing the parts in the wrong order must still assemble in order.
	if err := up.Complete(ctx, []Part{parts[2], parts[0], parts[1]}); err != nil {
		t.Fatal(err)
	}
	want := bytes.Join(chunks, nil)
	if got := f.objects["art/"+key]; !bytes.Equal(got, want) {
		t.Fatalf("stored %q, want %q", got, want)
	}
	info, err := s.Stat(ctx, key)
	if err != nil || info.Size != int64(len(want)) || info.ContentType != "application/zip" {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	for _, h := range f.gotUnsigned {
		if h != "UNSIGNED-PAYLOAD" {
			t.Errorf("part sent with payload hash %q", h)
		}
	}
}

// The body of a part is a stream (a limited reader over spool files), which
// the SDK cannot rewind; a store that answers 503 once must still get the
// part on the retry.
func TestStreamedPartSurvivesATransientError(t *testing.T) {
	f, s := newFake(t)
	f.failParts = 1
	up, err := s.BeginUpload(context.Background(), "k", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Repeat("x", 1000)
	// io.LimitReader hides any Seek the underlying reader has.
	p, err := up.PutPart(context.Background(), 1, io.LimitReader(strings.NewReader(data), int64(len(data))), int64(len(data)))
	if err != nil {
		t.Fatalf("a retryable error became fatal: %v", err)
	}
	if err := up.Complete(context.Background(), []Part{p}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.objects["art/k"]); got != data {
		t.Errorf("stored %d bytes, want %d", len(got), len(data))
	}
}

func TestSkippedPartsAreDiscarded(t *testing.T) {
	f, s := newFake(t)
	ctx := context.Background()
	up, _ := s.BeginUpload(ctx, "k", "")
	p1, _ := up.PutPart(ctx, 1, strings.NewReader("keep"), 4)
	up.PutPart(ctx, 2, strings.NewReader("uncommitted"), 11)
	if err := up.Complete(ctx, []Part{p1}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.objects["art/k"]); got != "keep" {
		t.Fatalf("stored %q", got)
	}
}

func TestRetriedPartReplaces(t *testing.T) {
	f, s := newFake(t)
	ctx := context.Background()
	up, _ := s.BeginUpload(ctx, "k", "")
	up.PutPart(ctx, 1, strings.NewReader("first"), 5)
	p, _ := up.PutPart(ctx, 1, strings.NewReader("again"), 5)
	if err := up.Complete(ctx, []Part{p}); err != nil {
		t.Fatal(err)
	}
	if got := string(f.objects["art/k"]); got != "again" {
		t.Fatalf("stored %q", got)
	}
}

func TestCompleteRejects(t *testing.T) {
	_, s := newFake(t)
	ctx := context.Background()
	up, _ := s.BeginUpload(ctx, "k", "")
	if err := up.Complete(ctx, nil); err == nil {
		t.Error("empty part list accepted")
	}
	p := Part{Number: 1, ETag: `"x"`}
	if err := up.Complete(ctx, []Part{p, p}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate part: %v", err)
	}
	if _, err := up.PutPart(ctx, 0, strings.NewReader("x"), 1); err == nil {
		t.Error("part 0 accepted")
	}
	if _, err := up.PutPart(ctx, MaxParts+1, strings.NewReader("x"), 1); err == nil {
		t.Error("part 10001 accepted")
	}
	// A part the store does not know about is the store's error.
	err := up.Complete(ctx, []Part{{Number: 7, ETag: `"nope"`}})
	var be *Error
	if !errors.As(err, &be) || be.Code != "InvalidPart" {
		t.Errorf("unknown part: %v", err)
	}
}

// S3 can answer 200 to CompleteMultipartUpload and still have failed.
func TestCompleteErrorInOKResponse(t *testing.T) {
	f, s := newFake(t)
	ctx := context.Background()
	up, _ := s.BeginUpload(ctx, "k", "")
	p, _ := up.PutPart(ctx, 1, strings.NewReader("data"), 4)
	f.completeError = true
	err := up.Complete(ctx, []Part{p})
	var be *Error
	if !errors.As(err, &be) || be.Code != "EntityTooSmall" {
		t.Fatalf("got %v, want EntityTooSmall", err)
	}
	if _, ok := f.objects["art/k"]; ok {
		t.Error("object stored despite the error")
	}
}

func TestAbort(t *testing.T) {
	f, s := newFake(t)
	ctx := context.Background()
	up, _ := s.BeginUpload(ctx, "k", "")
	up.PutPart(ctx, 1, strings.NewReader("x"), 1)
	if err := up.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.uploads) != 0 {
		t.Error("upload still open")
	}
	if err := up.Abort(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Abort = %v, want ErrNotFound", err)
	}
	// Another process can pick the upload up by id.
	up2, _ := s.BeginUpload(ctx, "k2", "")
	p, _ := s.ResumeUpload("k2", up2.ID()).PutPart(ctx, 1, strings.NewReader("hi"), 2)
	if err := up2.Complete(ctx, []Part{p}); err != nil {
		t.Fatal(err)
	}
}

func TestPresignGet(t *testing.T) {
	f, s := newFake(t)
	ctx := context.Background()
	key := "a b/c.zip"
	up, _ := s.BeginUpload(ctx, key, "application/zip")
	p, _ := up.PutPart(ctx, 1, strings.NewReader("payload"), 7)
	if err := up.Complete(ctx, []Part{p}); err != nil {
		t.Fatal(err)
	}
	_ = f

	u, err := s.PresignGet(ctx, key, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"X-Amz-Signature=", "X-Amz-Expires=900", "X-Amz-Algorithm=AWS4-HMAC-SHA256", "/bkt/art/a%20b/c.zip"} {
		if !strings.Contains(u, want) {
			t.Errorf("URL lacks %q: %s", want, u)
		}
	}
	// No credentials beyond the URL.
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "payload" {
		t.Fatalf("GET = %d %q", resp.StatusCode, body)
	}
}

func TestStatAndDeleteMissing(t *testing.T) {
	_, s := newFake(t)
	ctx := context.Background()
	_, err := s.Stat(ctx, "nope")
	var be *Error
	if !errors.Is(err, ErrNotFound) || !errors.As(err, &be) || be.Status != 404 {
		t.Fatalf("Stat = %v", err)
	}
	if err := s.Delete(ctx, "nope"); err != nil {
		t.Errorf("Delete of a missing key: %v", err)
	}
}

func TestDelete(t *testing.T) {
	f, s := newFake(t)
	ctx := context.Background()
	up, _ := s.BeginUpload(ctx, "k", "")
	p, _ := up.PutPart(ctx, 1, strings.NewReader("x"), 1)
	up.Complete(ctx, []Part{p})
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0)
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 0 {
		t.Errorf("objects left: %v", keys)
	}
}

func TestNewS3Validates(t *testing.T) {
	if _, err := NewS3(S3Config{AccessKey: "a", SecretKey: "b"}); err == nil {
		t.Error("missing bucket accepted")
	}
	if _, err := NewS3(S3Config{Bucket: "b"}); err == nil {
		t.Error("missing credentials accepted")
	}
}
