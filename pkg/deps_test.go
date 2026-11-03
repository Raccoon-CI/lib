package pkg

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/Raccoon-CI/lib/"

// deps collects the packages of this module that the non-test files of the
// package in dir (relative to the repository root) import, directly or not, by
// reading the files, so no build is needed.
func deps(t *testing.T, dir string, seen map[string]bool) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", filepath.FromSlash(dir), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			rest, ok := strings.CutPrefix(path, module)
			if !ok || seen[rest] {
				continue
			}
			seen[rest] = true
			deps(t, rest, seen)
		}
	}
}

// pkg/ is the shared library that the backend, the runner and the command line
// build on as separate repositories (TODO D.3). This repository has no
// application side left to guard against, so the rule is the other way round:
// what a package under pkg/ rests on, within this module, is other packages of
// pkg/ and the library's own private helpers internal/azblob and
// internal/twirp. Anything else added to this module (a cmd/, another internal/
// package) must not be pulled into pkg/ by accident.
func TestPkgRestsOnlyOnPkgAndItsHelpers(t *testing.T) {
	helpers := []string{"internal/azblob", "internal/twirp"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		checked++
		seen := map[string]bool{}
		deps(t, "pkg/"+e.Name(), seen)
		for p := range seen {
			if strings.HasPrefix(p, "pkg/") || slices.Contains(helpers, p) {
				continue
			}
			t.Errorf("pkg/%s depends on %s; within this module pkg/ may rest only on pkg/* and %v", e.Name(), p, helpers)
		}
	}
	if checked == 0 {
		t.Fatal("no packages found under pkg/")
	}
}
