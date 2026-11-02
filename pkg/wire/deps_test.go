package wire

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
			rest, ok := strings.CutPrefix(path, "github.com/Raccoon-CI/raccoonci/")
			if !ok || seen[path] {
				continue
			}
			seen[path] = true
			internalDeps(t, filepath.Join("..", "..", filepath.FromSlash(rest)), seen)
		}
	}
}

// The protocol between a controller and a machine is shared by the backend, the
// runner and the command line, which will be separate repositories: it may rest
// on the data packages and on nothing that runs steps (docs: TODO D.3, S2).
func TestWireRestsOnlyOnDataPackages(t *testing.T) {
	seen := map[string]bool{}
	internalDeps(t, ".", seen)
	var got []string
	for p := range seen {
		got = append(got, p)
	}
	slices.Sort(got)
	want := []string{"github.com/Raccoon-CI/raccoonci/pkg/runevent", "github.com/Raccoon-CI/raccoonci/pkg/value"}
	if !slices.Equal(got, want) {
		t.Fatalf("package wire depends on %v; it may depend on %v only (package runner, which runs steps, must not be among them)", got, want)
	}
}
