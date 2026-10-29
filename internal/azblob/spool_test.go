package azblob

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewSpoolDir(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "spool") // does not exist yet
	old := time.Now().Add(-2 * staleSpool)
	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(parent, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(p, "chunk"), []byte("x"), 0o600)
		os.Chtimes(p, mtime, mtime)
		return p
	}

	first, err := NewSpoolDir(parent, "rc-")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(first) != parent {
		t.Fatalf("spool dir %s is not under %s", first, parent)
	}

	// Left by a killed process, by a running one, and not ours at all.
	stale := mk("rc-stale", old)
	fresh := mk("rc-fresh", time.Now())
	other := mk("other-stale", old)

	second, err := NewSpoolDir(parent, "rc-")
	if err != nil {
		t.Fatal(err)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if exists(stale) {
		t.Error("a day-old spool directory was kept")
	}
	if !exists(fresh) || !exists(first) {
		t.Error("a recent spool directory, possibly another process's, was removed")
	}
	if !exists(other) {
		t.Error("a directory without the prefix was removed")
	}
	if second == first {
		t.Error("two calls returned the same directory")
	}

	// No parent: the system temporary directory.
	d, err := NewSpoolDir("", "rc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d)
	if filepath.Dir(d) != filepath.Clean(os.TempDir()) {
		t.Errorf("%s is not under %s", d, os.TempDir())
	}
}
