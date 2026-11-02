package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarGz(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		body := []byte(h.Linkname)
		if h.Typeflag == tar.TypeReg {
			h.Size, h.Linkname = int64(len(body)), ""
		}
		if h.Mode == 0 && h.Typeflag != tar.TypeXGlobalHeader {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(body)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// reg is a regular file; its content travels in Linkname for brevity.
func reg(name, body string, mode int64) *tar.Header {
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Linkname: body, Mode: mode}
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	data := tarGz(t,
		&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "sha"}},
		&tar.Header{Typeflag: tar.TypeDir, Name: "node-v20/", Mode: 0o755},
		reg("node-v20/bin/node", "#!/bin/sh\n", 0o755),
		reg("node-v20/lib/cli.js", "js", 0o644),
		&tar.Header{Typeflag: tar.TypeSymlink, Name: "node-v20/bin/npm", Linkname: "../lib/cli.js"},
		&tar.Header{Typeflag: tar.TypeLink, Name: "node-v20/bin/node2", Linkname: "node-v20/bin/node"},
	)
	if err := ExtractTarGz(bytes.NewReader(data), dir, 1<<20); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "bin/node"))
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("bin/node: %v %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "bin/npm")); err != nil || string(b) != "js" {
		t.Errorf("symlink: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/node2")); err != nil {
		t.Errorf("hard link: %v", err)
	}
	if err := ExtractTarGz(bytes.NewReader(data), t.TempDir(), 5); err == nil {
		t.Error("size limit not enforced")
	}
}

func TestExtractVerified(t *testing.T) {
	data := tarGz(t, reg("top/bin/tool", "x", 0o755))
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	parent := t.TempDir()
	dest := filepath.Join(parent, "tools", "1.0", "x64")

	if err := ExtractTarGzVerified(bytes.NewReader(data), dest, strings.Repeat("0", 64), 1<<20); err == nil {
		t.Fatal("a wrong checksum must fail")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("nothing may be left at dest after a failure")
	}
	if err := ExtractTarGzVerified(bytes.NewReader(data), dest, strings.ToUpper(want), 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "tool")); err != nil {
		t.Error(err)
	}
	// A failed retry leaves the good copy alone, and no temp dirs behind.
	if err := ExtractTarGzVerified(bytes.NewReader(data), dest, strings.Repeat("0", 64), 1<<20); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "tool")); err != nil {
		t.Errorf("good copy lost: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dest)); len(entries) != 1 {
		t.Errorf("leftovers: %v", entries)
	}
}

func TestExtractStaysInside(t *testing.T) {
	outside := t.TempDir()
	for name, headers := range map[string][]*tar.Header{
		"dotdot":   {reg("top/../../escape", "x", 0)},
		"absolute": {reg("/etc/escape", "x", 0)},
		"symlink then write through it": {
			{Typeflag: tar.TypeSymlink, Name: "top/evil", Linkname: outside},
			reg("top/evil/pwned", "x", 0),
		},
		"hard link out": {{Typeflag: tar.TypeLink, Name: "top/l", Linkname: "../../etc/passwd"}},
	} {
		if err := ExtractTarGz(bytes.NewReader(tarGz(t, headers...)), t.TempDir(), 1<<20); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
			t.Fatalf("%s: wrote outside the destination", name)
		}
	}
}
