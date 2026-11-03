package affected

import (
	"slices"
	"testing"

	"github.com/Raccoon-CI/lib/pkg/catalog"
)

// Which directories package discovery does not enter is the catalog's say
// (`skip-dirs` and `build-output-dirs`), not a list in this package.
func TestSkippedDirectoriesComeFromTheCatalog(t *testing.T) {
	// An npm workspace whose pattern matches two directories that are not
	// packages of the project: a Python virtual environment and a build output.
	repo := files(map[string]string{
		"package.json":               `{"name":"root","workspaces":["packages/*"]}`,
		"packages/a/package.json":    `{"name":"a"}`,
		"packages/venv/package.json": `{"name":"in-a-venv"}`,
		"packages/dist/package.json": `{"name":"built"}`,
	})
	names := func(pkgs []Package) []string {
		var out []string
		for _, p := range pkgs {
			out = append(out, p.Name)
		}
		slices.Sort(out)
		return out
	}

	// Built in: venv and dist are both skipped (the venv one was missed by the
	// list this package used to carry).
	if got := names(Packages(repo)); !slices.Equal(got, []string{"a"}) {
		t.Errorf("built-in skip lists: packages = %v, want [a]", got)
	}
	// A catalog that skips nothing finds all three.
	if got := names(PackagesSkipping(repo, nil)); !slices.Equal(got, []string{"a", "built", "in-a-venv"}) {
		t.Errorf("nothing skipped: packages = %v", got)
	}
	// A catalog that skips only dist: the venv directory is a package.
	cat, err := catalog.Default().Overlay([]byte("skip-dirs: [.git]\nbuild-output-dirs: [dist]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(PackagesSkipping(repo, SkipDirs(cat))); !slices.Equal(got, []string{"a", "in-a-venv"}) {
		t.Errorf("catalog skipping dist only: packages = %v", got)
	}
}

// Decide takes the same list through Input, so a catalog the user overlaid
// applies to the jobs it decides about too.
func TestDecideUsesTheGivenSkipDirs(t *testing.T) {
	// packages/a depends on the package in packages/venv; packages/b is unrelated.
	repo := files(map[string]string{
		"package.json":               `{"name":"root","workspaces":["packages/*"]}`,
		"packages/a/package.json":    `{"name":"a","dependencies":{"in-a-venv":"*"}}`,
		"packages/b/package.json":    `{"name":"b"}`,
		"packages/venv/package.json": `{"name":"in-a-venv"}`,
	})
	w := wf(t, `  ja:
    runs-on: ubuntu-latest
    steps:
      - run: echo
        working-directory: packages/a
  jb:
    runs-on: ubuntu-latest
    steps:
      - run: echo
        working-directory: packages/b
`)
	in := Input{Workflow: w, WorkflowPath: ".github/workflows/ci.yml", Files: []string{"packages/venv/x.js"}, Repo: repo}

	// venv is skipped (built in): packages/venv is no package, the changed file
	// belongs to nothing known, so every job is affected, the cautious answer.
	if got := Decide(in); !got["ja"].Affected || !got["jb"].Affected {
		t.Errorf("venv skipped: %+v, want both jobs affected", got)
	}
	// Not skipped: packages/venv is a package that only packages/a depends on,
	// so the unrelated job can be left out.
	in.SkipDirs = []string{".git"}
	if got := Decide(in); !got["ja"].Affected || got["jb"].Affected {
		t.Errorf("venv not skipped: %+v, want only ja affected", got)
	}
}

// A toolchain's `workspace-kind` in the catalog names a kind of package this
// package finds; a kind it did not know would silently never match.
func TestCatalogWorkspaceKindsAreKindsWeFind(t *testing.T) {
	for name, tc := range catalog.Default().Toolchains {
		if tc.WorkspaceKind != "" && !slices.Contains(Kinds, tc.WorkspaceKind) {
			t.Errorf("toolchain %s: workspace-kind %q is not one of %v", name, tc.WorkspaceKind, Kinds)
		}
	}
}
