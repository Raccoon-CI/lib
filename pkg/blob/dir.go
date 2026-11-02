package blob

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dir is a Store on a local directory, for runs on one machine where there is
// no bucket. It follows the same rules as S3 (an object appears when the
// upload completes, parts can be replaced), except that it does not insist on
// MinPartSize.
//
// Downloads go through the server that mounts Handler, since there is no
// separate store to hand out URLs for. They are signed the same way: a URL
// works until its ttl runs out and needs nothing else.
type Dir struct {
	// Prefix is the path Handler must be mounted at; it starts and ends
	// with "/". NewDir sets DownloadPrefix. Change it before use when two
	// Dirs share one server.
	Prefix string

	root    *os.Root
	baseURL string
	secret  []byte
	now     func() time.Time
}

var _ Store = (*Dir)(nil)

// DownloadPrefix is where Handler expects to be mounted.
const DownloadPrefix = "/_dl/"

// NewDir opens (and creates) dir. baseURL is where Handler is reachable by the
// clients that get presigned URLs, e.g. "http://127.0.0.1:4000". secret signs
// those URLs.
func NewDir(dir, baseURL string, secret []byte) (*Dir, error) {
	if len(secret) < 16 {
		return nil, errors.New("blob: secret must be at least 16 bytes")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"objects", "uploads"} {
		if err := root.MkdirAll(d, 0o700); err != nil {
			root.Close()
			return nil, err
		}
	}
	return &Dir{Prefix: DownloadPrefix, root: root, baseURL: strings.TrimRight(baseURL, "/"), secret: secret, now: time.Now}, nil
}

func (d *Dir) Close() error { return d.root.Close() }

func checkKey(key string) error {
	if key == "" || len(key) > 1024 || strings.ContainsRune(key, 0) || strings.HasPrefix(key, "/") {
		return fmt.Errorf("blob: bad key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("blob: bad key %q", key)
		}
	}
	return nil
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func notFound(op, key string) error {
	return &Error{Op: op, Key: key, Status: 404, Code: "NoSuchKey"}
}

func (d *Dir) BeginUpload(_ context.Context, key, _ string) (Upload, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	id := randomID()
	if err := d.root.MkdirAll(path.Join("uploads", id), 0o700); err != nil {
		return nil, err
	}
	return &dirUpload{d: d, key: key, id: id, etags: map[int]string{}}, nil
}

func (d *Dir) Stat(_ context.Context, key string) (Info, error) {
	if err := checkKey(key); err != nil {
		return Info{}, err
	}
	fi, err := d.root.Stat(path.Join("objects", key))
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, notFound("stat", key)
	}
	if err != nil {
		return Info{}, err
	}
	return Info{Size: fi.Size()}, nil
}

func (d *Dir) Delete(_ context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	err := d.root.Remove(path.Join("objects", key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (d *Dir) sign(key string, exp int64) string {
	m := hmac.New(sha256.New, d.secret)
	fmt.Fprintf(m, "download\n%s\n%d", key, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// PresignGet signs the path only, so the link works on every address the
// server listens on; it is built on the one in ctx (see WithBaseURL).
func (d *Dir) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	exp := d.now().Add(ttl).Unix()
	u := url.URL{Path: d.Prefix + key}
	return fmt.Sprintf("%s%s?exp=%d&sig=%s", BaseURLFrom(ctx, d.baseURL), u.EscapedPath(), exp, d.sign(key, exp)), nil
}

// Handler serves the URLs PresignGet returns. Mount it at d.Prefix.
func (d *Dir) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, d.Prefix)
		exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
		if err != nil || checkKey(key) != nil || d.now().Unix() > exp ||
			!hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(d.sign(key, exp))) {
			http.Error(w, "invalid or expired link", http.StatusForbidden)
			return
		}
		f, err := d.root.Open(path.Join("objects", key))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", fi.ModTime(), f)
	})
}

type dirUpload struct {
	d   *Dir
	key string
	id  string

	mu    sync.Mutex
	etags map[int]string
}

func (u *dirUpload) ID() string { return u.id }

func (u *dirUpload) dir() string { return path.Join("uploads", u.id) }

func (u *dirUpload) PutPart(_ context.Context, n int, r io.Reader, size int64) (Part, error) {
	if n < 1 || n > MaxParts {
		return Part{}, fmt.Errorf("blob: part number %d out of range 1..%d", n, MaxParts)
	}
	name := path.Join(u.dir(), strconv.Itoa(n))
	tmp := name + "." + randomID() + ".tmp"
	f, err := u.d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Part{}, wrapFS("put part", u.key, err)
	}
	h := md5.New()
	got, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && got != size {
		err = fmt.Errorf("blob: part %d has %d bytes, want %d", n, got, size)
	}
	if err == nil {
		err = u.d.root.Rename(tmp, name)
	}
	if err != nil {
		u.d.root.Remove(tmp)
		return Part{}, wrapFS("put part", u.key, err)
	}
	etag := `"` + hex.EncodeToString(h.Sum(nil)) + `"`
	u.mu.Lock()
	u.etags[n] = etag
	u.mu.Unlock()
	return Part{Number: n, ETag: etag}, nil
}

func (u *dirUpload) Complete(_ context.Context, parts []Part) error {
	if len(parts) == 0 {
		return errors.New("blob: no parts to complete")
	}
	ps := append([]Part(nil), parts...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Number < ps[j].Number })
	u.mu.Lock()
	for i, p := range ps {
		if i > 0 && p.Number == ps[i-1].Number {
			u.mu.Unlock()
			return fmt.Errorf("blob: part %d listed twice", p.Number)
		}
		if u.etags[p.Number] == "" || u.etags[p.Number] != p.ETag {
			u.mu.Unlock()
			return &Error{Op: "complete upload", Key: u.key, Status: 400, Code: "InvalidPart", Message: fmt.Sprintf("part %d is missing or has changed", p.Number)}
		}
	}
	u.mu.Unlock()

	final := path.Join(u.dir(), "final")
	out, err := u.d.root.OpenFile(final, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return wrapFS("complete upload", u.key, err)
	}
	for _, p := range ps {
		in, err := u.d.root.Open(path.Join(u.dir(), strconv.Itoa(p.Number)))
		if err == nil {
			_, err = io.Copy(out, in)
			in.Close()
		}
		if err != nil {
			out.Close()
			return wrapFS("complete upload", u.key, err)
		}
	}
	if err := out.Close(); err != nil {
		return wrapFS("complete upload", u.key, err)
	}
	dest := path.Join("objects", u.key)
	if err := u.d.root.MkdirAll(path.Dir(dest), 0o700); err != nil {
		return wrapFS("complete upload", u.key, err)
	}
	if err := u.d.root.Rename(final, dest); err != nil {
		return wrapFS("complete upload", u.key, err)
	}
	u.d.root.RemoveAll(u.dir())
	return nil
}

func (u *dirUpload) Abort(_ context.Context) error {
	if _, err := u.d.root.Stat(u.dir()); errors.Is(err, fs.ErrNotExist) {
		return &Error{Op: "abort upload", Key: u.key, Status: 404, Code: "NoSuchUpload"}
	}
	return u.d.root.RemoveAll(u.dir())
}

func wrapFS(op, key string, err error) error {
	return &Error{Op: op, Key: key, Status: 500, Code: "IOError", Message: err.Error()}
}
