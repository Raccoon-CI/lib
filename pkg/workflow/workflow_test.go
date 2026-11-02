package workflow

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Raccoon-CI/raccoonci/pkg/value"
)

const ciWorkflow = `
name: CI
on:
  push:
    branches: [main]
  pull_request:
env:
  CI: true
defaults:
  run:
    shell: bash
jobs:
  test:
    runs-on: ${{ matrix.os }}
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, macos-latest]
        python: [3.9, 3.10]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with:
          python-version: ${{ matrix.python }}
      - id: tests
        run: |
          pip install -r requirements.txt
          pytest
  deploy:
    needs: test
    if: github.ref == 'refs/heads/main'
    uses: ./.github/workflows/deploy.yml
    secrets: inherit
`

func TestParseWorkflow(t *testing.T) {
	w, err := Parse([]byte(ciWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "CI" || w.Defaults.Shell != "bash" || len(w.Jobs) != 2 {
		t.Fatalf("unexpected workflow: %+v", w)
	}
	if on, ok := w.On.(value.Object); !ok || on[0].Key != "push" {
		t.Errorf(`"on" decoded as %#v; it must stay a mapping keyed by event name`, w.On)
	}

	test := w.Jobs[0]
	if test.ID != "test" || test.RunsOn != "${{ matrix.os }}" || len(test.Steps) != 3 {
		t.Fatalf("unexpected test job: %+v", test)
	}
	if test.Strategy.FailFast != false {
		t.Errorf("fail-fast = %#v, want false", test.Strategy.FailFast)
	}
	if s := test.Steps[2]; s.ID != "tests" || !strings.Contains(s.Run, "pytest") {
		t.Errorf("unexpected run step: %+v", s)
	}
	if v, _ := test.Steps[1].With.Get("python-version"); v != "${{ matrix.python }}" {
		t.Errorf("with.python-version = %#v", v)
	}

	// Numbers are floats, as on GitHub: 3.10 really is 3.1 there too.
	combos, err := ExpandMatrix(test.Strategy.Matrix)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := combos[1].Get("python"); v != 3.1 {
		t.Errorf("python = %#v, want 3.1", v)
	}

	deploy := w.Job("deploy")
	if deploy.Uses != "./.github/workflows/deploy.yml" || deploy.Secrets != "inherit" || deploy.Needs[0] != "test" {
		t.Errorf("unexpected deploy job: %+v", deploy)
	}
}

func TestParseKeepsDynamicMatrixRaw(t *testing.T) {
	w, err := Parse([]byte(`
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix: ${{ fromJSON(needs.setup.outputs.matrix) }}
    steps:
      - run: make
`))
	if err != nil {
		t.Fatal(err)
	}
	if m := w.Jobs[0].Strategy.Matrix; !value.ContainsExpr(m) {
		t.Errorf("matrix = %#v, want the raw expression", m)
	}
}

func TestParseAnchors(t *testing.T) {
	w, err := Parse([]byte(`
on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps: &steps
      - uses: actions/checkout@v4
      - run: make test
  b:
    runs-on: windows-latest
    steps: *steps
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Job("b").Steps; len(got) != 2 || got[1].Run != "make test" {
		t.Errorf("aliased steps = %+v", got)
	}
}

func TestParseErrors(t *testing.T) {
	const head = "on: push\njobs:\n"
	tests := []struct {
		name, src, wantErr string
	}{
		{"empty", "", "empty"},
		{"no jobs", "on: push", `no "jobs"`},
		{"typo in job key", head + "  a:\n    run-on: x\n    steps: [{run: x}]", `line 4: unknown key "run-on" in job "a"`},
		{"missing runs-on", head + "  a:\n    steps: [{run: x}]", `job "a" has no "runs-on"`},
		{"missing steps", head + "  a:\n    runs-on: x", `job "a" has no "steps"`},
		{"uses and steps", head + "  a:\n    uses: ./w.yml\n    steps: [{run: x}]", "cannot have"},
		{"run and uses", head + "  a:\n    runs-on: x\n    steps:\n      - run: x\n        uses: y", "exactly one of"},
		{"neither run nor uses", head + "  a:\n    runs-on: x\n    steps:\n      - name: nothing", "exactly one of"},
		{"bad job id", head + "  1st:\n    runs-on: x\n    steps: [{run: x}]", `invalid job id "1st"`},
		{"duplicate step id", head + "  a:\n    runs-on: x\n    steps:\n      - {id: s, run: x}\n      - {id: s, run: y}", `step id "s" is used more than once`},
		{"duplicate job", head + "  a:\n    runs-on: x\n    steps: [{run: x}]\n  a:\n    runs-on: x\n    steps: [{run: x}]", `duplicate key "a"`},
		{"unknown need", head + "  a:\n    needs: b\n    runs-on: x\n    steps: [{run: x}]", `needs "b", which does not exist`},
		{"cycle", head + "  a:\n    needs: c\n    runs-on: x\n    steps: [{run: x}]\n  b:\n    needs: a\n    runs-on: x\n    steps: [{run: x}]\n  c:\n    needs: b\n    runs-on: x\n    steps: [{run: x}]", "cycle: a -> c -> b -> a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// Each level of anchors repeats the previous one 10 times, so expanding E9
// would produce 10^10 nodes.
func TestParseRejectsAliasBomb(t *testing.T) {
	var b strings.Builder
	b.WriteString("on: push\nenv:\n  E0: &e0 [lol, lol, lol, lol, lol, lol, lol, lol, lol, lol]\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&b, "  E%d: &e%d [%s]\n", i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*e%d, ", i-1), 10), ", "))
	}
	b.WriteString("jobs:\n  a:\n    runs-on: x\n    steps: [{run: x}]\n")
	_, err := Parse([]byte(b.String()))
	if err == nil || !strings.Contains(err.Error(), "alias expansion limit") {
		t.Fatalf("got error %v, want the alias expansion limit", err)
	}
}
