// Package archive unpacks downloaded tarballs (action repositories, tool
// releases) without letting their contents escape the destination.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ExtractTarGz unpacks a .tar.gz into dir, dropping the top-level directory
// that GitHub tarballs (owner-repo-sha/) and release archives
// (node-v20.1.0-linux-x64/) wrap everything in. Writes go through os.Root,
// so entries can't escape dir, not even through symlinks the archive itself
// creates. maxSize caps the total unpacked size.
func ExtractTarGz(r io.Reader, dir string, maxSize int64) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.Contains("/"+h.Name+"/", "/../") || path.IsAbs(h.Name) {
			return fmt.Errorf("invalid path %q", h.Name)
		}
		_, name, ok := strings.Cut(path.Clean(h.Name), "/")
		if !ok || name == "" {
			continue // the top-level directory itself, or a global header
		}
		if !fs.ValidPath(name) {
			return fmt.Errorf("invalid path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			total += h.Size
			if total > maxSize {
				return fmt.Errorf("larger than %d MiB unpacked", maxSize>>20)
			}
			err = writeFile(root, name, tr, h.FileInfo().Mode().Perm()|0o600)
		case tar.TypeSymlink:
			if err = root.MkdirAll(path.Dir(name), 0o755); err == nil {
				err = root.Symlink(h.Linkname, name)
			}
		case tar.TypeLink:
			// Hard links must point inside the archive, which they do by
			// construction once both names went through the same checks.
			target := h.Linkname
			if _, t, ok := strings.Cut(path.Clean(target), "/"); ok && fs.ValidPath(t) && !strings.Contains("/"+target+"/", "/../") {
				err = root.Link(t, name)
			} else {
				err = fmt.Errorf("invalid link target %q", target)
			}
		}
		if err != nil {
			return err
		}
	}
}

// ExtractTarGzVerified unpacks a .tar.gz stream (see ExtractTarGz) into
// dest, and only keeps the result if the stream's SHA-256 is wantSHA256.
// The unpacking happens next to dest and is renamed into place at the end,
// so dest is either complete and verified or absent (any previous dest is
// replaced).
func ExtractTarGzVerified(r io.Reader, dest, wantSHA256 string, maxSize int64) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	h := sha256.New()
	tee := io.TeeReader(io.LimitReader(r, maxSize), h)
	if err := ExtractTarGz(tee, tmp, maxSize); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, tee); err != nil { // the rest of the gzip stream
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA256) {
		return fmt.Errorf("SHA-256 is %s, expected %s", got, wantSHA256)
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func writeFile(root *os.Root, name string, r io.Reader, perm fs.FileMode) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
