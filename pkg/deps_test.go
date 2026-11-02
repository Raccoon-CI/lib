package pkg

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/Raccoon-CI/raccoonci/"

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
// will build on as separate repositories (TODO D.3, S4). Whatever runs steps,
// scans, provisions, downloads, schedules or serves must stay outside it: no
// package under pkg/ may depend on these, directly or not. internal/azblob and
// internal/twirp are the library's own private helpers and are allowed.
func TestPkgDoesNotDependOnTheApplicationSide(t *testing.T) {
	forbidden := []string{"runner", "engine", "actions", "scan", "provision", "nodejs", "docker", "missing", "mirror", "local", "runnercmd", "semver", "affected", "envsnap", "plan", "sched", "ghrunner", "server"}
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
			for _, bad := range forbidden {
				if p == "internal/"+bad || strings.HasPrefix(p, "internal/"+bad+"/") {
					t.Errorf("pkg/%s depends on %s; pkg/ must not rest on internal/%s", e.Name(), p, bad)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no packages found under pkg/")
	}
}
