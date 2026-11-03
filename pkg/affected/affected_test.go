package affected

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Raccoon-CI/lib/pkg/workflow"
)

func wf(t *testing.T, jobs string) *workflow.Workflow {
	t.Helper()
	w, err := workflow.Parse([]byte("on: push\njobs:\n" + jobs))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func files(m map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for k, v := range m {
		fsys[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return fsys
}

func decide(t *testing.T, w *workflow.Workflow, repo fstest.MapFS, changed ...string) map[string]Decision {
	t.Helper()
	cfg, err := LoadConfig(repo)
	if err != nil {
		t.Fatal(err)
	}
	if changed == nil {
		changed = []string{}
	}
	return Decide(Input{Workflow: w, WorkflowPath: ".github/workflows/ci.yml", Files: changed, Repo: repo, Config: cfg})
}

const frontBack = `
  front:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: frontend
    steps:
      - run: npm test
  back:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
        working-directory: backend
  anywhere:
    runs-on: ubuntu-latest
    steps:
      - run: ./scripts/check.sh
`

func TestJobsFollowTheirWorkingDirectory(t *testing.T) {
	w := wf(t, frontBack)
	repo := files(map[string]string{"frontend/a.js": "", "backend/b.go": ""})

	d := decide(t, w, repo, "backend/b.go")
	if d["front"].Affected {
		t.Errorf("front for a backend change: %+v", d["front"])
	}
	if !d["back"].Affected || !d["anywhere"].Affected {
		t.Errorf("%+v", d)
	}
	d = decide(t, w, repo, "frontend/src/x.js")
	if !d["front"].Affected || d["back"].Affected {
		t.Errorf("%+v", d)
	}
	// Both: both run.
	d = decide(t, w, repo, "frontend/x.js", "backend/y.go")
	if !d["front"].Affected || !d["back"].Affected {
		t.Errorf("%+v", d)
	}
}

func TestUnknownOrSharedChangesAffectEverything(t *testing.T) {
	w := wf(t, frontBack)
	repo := files(map[string]string{"frontend/a.js": "", "backend/b.go": ""})
	for _, f := range []string{"package.json", "tsconfig.base.json", "tools/gen.sh", ".github/workflows/ci.yml", ".github/actions/setup/action.yml"} {
		d := decide(t, w, repo, f)
		if !d["front"].Affected || !d["back"].Affected {
			t.Errorf("%s: %+v", f, d)
		}
	}
	// Prose and licences never concern a directory-bound job...
	d := decide(t, w, repo, "README.md", "LICENSE")
	if d["front"].Affected || d["back"].Affected {
		t.Errorf("docs: %+v", d)
	}
	// ...but a job that could touch anything still runs.
	if !d["anywhere"].Affected {
		t.Error("a job tied to no directory was skipped for a docs change")
	}
	// Unknown files: everything runs.
	if d := Decide(Input{Workflow: w, Files: nil, Repo: repo}); !d["front"].Affected {
		t.Error("unknown changes skipped a job")
	}
	// Nothing changed at all: only directory-bound jobs can stand down.
	d = decide(t, w, repo)
	if d["front"].Affected || d["back"].Affected {
		t.Errorf("no changes: %+v", d)
	}
}

func TestConfigOverridesAndExtends(t *testing.T) {
	w := wf(t, frontBack+`
  deploy:
    runs-on: ubuntu-latest
    steps:
      - run: ./deploy.sh
`)
	repo := files(map[string]string{
		"frontend/a.js": "", "backend/b.go": "", "docs/guide.txt": "",
		ConfigPath: `
ignore: ['docs/**']
global: ['go.work']
jobs:
  deploy: ['backend/**', 'infra/**']
`,
	})
	d := decide(t, w, repo, "docs/guide.txt")
	if d["front"].Affected || d["back"].Affected || d["deploy"].Affected {
		t.Errorf("ignored docs: %+v", d)
	}
	d = decide(t, w, repo, "infra/main.tf")
	if !d["deploy"].Affected {
		t.Errorf("deploy's own entry matches infra/: %+v", d["deploy"])
	}
	// infra/ belongs to no package or job directory, so, not being listed
	// under ignore, it concerns the jobs tied to a directory too.
	if !d["front"].Affected || !strings.Contains(d["front"].Reason, "belongs to no package") {
		t.Errorf("%+v", d["front"])
	}
}

func TestGlobalFromConfig(t *testing.T) {
	w := wf(t, frontBack)
	repo := files(map[string]string{"frontend/a.js": "", ConfigPath: "global: ['go.work']\n"})
	d := decide(t, w, repo, "go.work")
	if !d["front"].Affected || !d["back"].Affected || !strings.Contains(d["front"].Reason, "global") {
		t.Errorf("%+v", d)
	}
}

func TestBadConfigIsAnError(t *testing.T) {
	for _, c := range []string{"nope: 1\n", "ignore: ['[abc']\n", "jobs:\n  a: ['!x']\n"} {
		if _, err := LoadConfig(files(map[string]string{ConfigPath: c})); err == nil {
			t.Errorf("%q accepted", c)
		}
	}
}

func TestOutputsAndArtifactsMakeAJobUnskippable(t *testing.T) {
	w := wf(t, `
  plain:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
  out:
    runs-on: ubuntu-latest
    outputs: {v: '${{ steps.s.outputs.v }}'}
    steps: [{id: s, run: 'true'}]
  art:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/upload-artifact@v4
        with: {name: x, path: y}
`)
	for id, want := range map[string]bool{"plain": true, "out": false, "art": false} {
		if got, _ := Skippable(w.Job(id)); got != want {
			t.Errorf("%s: skippable = %v", id, got)
		}
	}
}

func TestLocalActionsCountAsDirectories(t *testing.T) {
	w := wf(t, `
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: ./tools/lint
      - run: make
        working-directory: app
`)
	repo := files(map[string]string{"app/m.go": "", "tools/lint/action.yml": ""})
	if d := decide(t, w, repo, "tools/lint/run.sh"); !d["j"].Affected {
		t.Errorf("%+v", d)
	}
	if d := decide(t, w, repo, "other/x.go"); !d["j"].Affected {
		t.Errorf("a file outside every area concerns the job: %+v", d["j"])
	}
	if d := decide(t, w, repo, "README.md"); d["j"].Affected {
		t.Errorf("a README concerns a directory-bound job: %+v", d["j"])
	}
}

func TestWorkspacePackagesAndTheirDependencies(t *testing.T) {
	repo := files(map[string]string{
		"package.json":               `{"name":"root","workspaces":["packages/*"]}`,
		"packages/ui/package.json":   `{"name":"@acme/ui","dependencies":{"@acme/core":"*","react":"18"}}`,
		"packages/core/package.json": `{"name":"@acme/core"}`,
		"packages/web/package.json":  `{"name":"@acme/web","dependencies":{"@acme/ui":"workspace:*"}}`,
		"packages/docs/package.json": `{"name":"@acme/docs"}`,
	})
	w := wf(t, `
  web:
    runs-on: ubuntu-latest
    steps:
      - run: npm test
        working-directory: packages/web
  docs:
    runs-on: ubuntu-latest
    steps:
      - run: npm test
        working-directory: packages/docs
  core:
    runs-on: ubuntu-latest
    steps:
      - run: npm test
        working-directory: packages/core
`)
	// core is a dependency of ui, which web depends on: a change in core
	// concerns web and core, but not docs.
	d := decide(t, w, repo, "packages/core/index.js")
	if !d["web"].Affected || !d["core"].Affected || d["docs"].Affected {
		t.Errorf("core change: %+v", d)
	}
	// A change in web concerns only web: nothing depends on it.
	d = decide(t, w, repo, "packages/web/app.js")
	if !d["web"].Affected || d["core"].Affected || d["docs"].Affected {
		t.Errorf("web change: %+v", d)
	}
	pk := Packages(repo)
	var ui Package
	for _, p := range pk {
		if p.Name == "@acme/ui" {
			ui = p
		}
	}
	if !slices.Equal(ui.Deps, []string{"packages/core"}) {
		t.Errorf("ui deps = %v (react is not a workspace package)", ui.Deps)
	}
}

func TestGoWorkspaceAndCargoWorkspace(t *testing.T) {
	repo := files(map[string]string{
		"go.work":     "go 1.24\n\nuse (\n\t./svc\n\t./lib // shared\n)\nuse ./tool\n",
		"svc/go.mod":  "module acme/svc\n\nrequire (\n\tacme/lib v0.0.0\n\tgithub.com/x/y v1.0.0\n)\n",
		"lib/go.mod":  "module acme/lib\n",
		"tool/go.mod": "module acme/tool\n\nreplace acme/lib => ../lib\n",
	})
	got := map[string][]string{}
	for _, p := range Packages(repo) {
		got[p.Dir] = p.Deps
	}
	if !slices.Equal(got["svc"], []string{"lib"}) || !slices.Equal(got["tool"], []string{"lib"}) || len(got["lib"]) != 0 || len(got) != 3 {
		t.Errorf("go packages: %v", got)
	}

	cargo := files(map[string]string{
		"Cargo.toml":             "[workspace]\nmembers = [\n  \"crates/*\",\n]\nexclude = [\"crates/old\"]\n",
		"crates/app/Cargo.toml":  "[package]\nname = \"app\"\n\n[dependencies]\ncore = { path = \"../core\" }\nserde = \"1\"\n\n[dev-dependencies.util]\npath = \"../util\"\n",
		"crates/core/Cargo.toml": "[package]\nname = \"core\"\n",
		"crates/util/Cargo.toml": "[package]\nname = \"util\"\n[dependencies]\ncore = { path = \"../core\", version = \"1\" }\n",
		"crates/old/Cargo.toml":  "[package]\nname = \"old\"\n",
	})
	cg := map[string][]string{}
	for _, p := range Packages(cargo) {
		slices.Sort(p.Deps)
		cg[p.Dir] = p.Deps
	}
	if !slices.Equal(cg["crates/app"], []string{"crates/core", "crates/util"}) || !slices.Equal(cg["crates/util"], []string{"crates/core"}) || len(cg) != 3 {
		t.Errorf("cargo packages: %v", cg)
	}
}
