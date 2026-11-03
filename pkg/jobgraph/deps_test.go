package jobgraph

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// internalDeps lists the packages of this module that the non-test files of
// the package in dir import, directly or not (by reading the files, so no
// build is needed).
func internalDeps(t *testing.T, dir string, seen map[string]bool) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
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
			rest, ok := strings.CutPrefix(path, "github.com/Raccoon-CI/lib/")
			if !ok || seen[path] {
				continue
			}
			seen[path] = true
			internalDeps(t, filepath.Join("..", "..", filepath.FromSlash(rest)), seen)
		}
	}
}

// The job graph will live in the shared library, which backend and runner both
// build on: it may rest on the data packages and on nothing that runs steps,
// scans, provisions or downloads (docs: TODO D.3, S1).
func TestJobgraphRestsOnlyOnDataPackages(t *testing.T) {
	seen := map[string]bool{}
	internalDeps(t, ".", seen)
	var got []string
	for p := range seen {
		got = append(got, strings.TrimPrefix(p, "github.com/Raccoon-CI/lib/"))
	}
	slices.Sort(got)
	t.Log(got)
	allowed := []string{"pkg/catalog", "pkg/expr", "pkg/platform", "pkg/runevent", "pkg/value", "pkg/workflow"}
	for _, p := range got {
		if !slices.Contains(allowed, p) {
			t.Errorf("package jobgraph depends on %s; it may depend on %v only", p, allowed)
		}
	}
}
