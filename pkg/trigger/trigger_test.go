package trigger

import (
	"strings"
	"testing"

	"github.com/Raccoon-CI/lib/pkg/workflow"
)

func on(t *testing.T, yaml string) any {
	t.Helper()
	w, err := workflow.Parse([]byte(yaml + "\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps: [{run: 'true'}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	return w.On
}

func match(t *testing.T, onYAML string, ev Event) Result {
	t.Helper()
	r, err := Match(on(t, onYAML), ev)
	if err != nil {
		t.Fatalf("%s: %v", onYAML, err)
	}
	return r
}

// The examples of GitHub's "Workflow syntax / filter pattern cheat sheet".
func TestFilterPatterns(t *testing.T) {
	tests := []struct {
		pattern string
		yes, no []string
	}{
		{"feature/*", []string{"feature/my-branch", "feature/your-branch"}, []string{"feature", "feature/a/b"}},
		{"feature/**", []string{"feature/beta-a/my-branch", "feature/your-branch", "feature/mona/the/octocat"}, []string{"feature"}},
		{"main", []string{"main"}, []string{"main2", "x/main"}},
		{"releases/**-alpha", []string{"releases/10-alpha", "releases/beta/3-alpha"}, []string{"releases/10-beta"}},
		{"*", []string{"main", "releases"}, []string{"all/the/branches"}},
		{"**", []string{"all/the/branches", "every/tag"}, nil},
		{"*feature", []string{"mona-feature", "feature", "ver-10-feature"}, []string{"10/feature", "feature/beta"}},
		{"v2*", []string{"v2", "v2.0", "v2.9"}, []string{"v1"}},
		{"v[12].[0-9]+.[0-9]+", []string{"v1.10.1", "v2.0.0"}, []string{"v3.0.0", "v1..1"}},
		{"**/README.md", []string{"README.md", "js/README.md"}, []string{"README.markdown"}},
		{"docs/**", []string{"docs/readme.md", "docs/mona/octocat.txt"}, []string{"docs"}},
		{"**.js", []string{"index.js", "js/index.js", "src/js/app.js"}, []string{"index.jsx"}},
		{"colou?r", []string{"color", "colour"}, []string{"colouur"}},
		{`a\*b`, []string{"a*b"}, []string{"axb"}},
	}
	for _, tt := range tests {
		re, err := compile(tt.pattern)
		if err != nil {
			t.Fatalf("%s: %v", tt.pattern, err)
		}
		for _, s := range tt.yes {
			if !re.MatchString(s) {
				t.Errorf("%q should match %q", tt.pattern, s)
			}
		}
		for _, s := range tt.no {
			if re.MatchString(s) {
				t.Errorf("%q should not match %q", tt.pattern, s)
			}
		}
	}
	for _, bad := range []string{"[abc", `x\`, "?x"} {
		if _, err := compile(bad); err == nil {
			t.Errorf("%q compiled", bad)
		}
	}
}

func TestNegationOrderMatters(t *testing.T) {
	list := []string{"sub-project/**", "!sub-project/docs/**"}
	for file, want := range map[string]bool{
		"sub-project/src/a.go":  true,
		"sub-project/docs/a.md": false,
		"other/a.go":            false,
	} {
		got, err := matchList(list, file)
		if err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", file, got, err, want)
		}
	}
	// A positive pattern after the negative one selects the file again.
	got, _ := matchList([]string{"docs/**", "!docs/**/*.md", "docs/README.md"}, "docs/README.md")
	if !got {
		t.Error("the later positive pattern should win")
	}
	if _, err := matchList([]string{"!docs/**"}, "x"); err == nil {
		t.Error("negative patterns alone were accepted")
	}
}

func TestEventForms(t *testing.T) {
	push := Event{Name: "push", Ref: "refs/heads/main"}
	for _, y := range []string{"on: push", "on: [push, pull_request]", "on:\n  push:\n  workflow_dispatch:", "on:\n  push:\n    branches: [main]"} {
		if r := match(t, y, push); !r.Triggered {
			t.Errorf("%q: %s", y, r.Reason)
		}
	}
	if r := match(t, "on: [pull_request]", push); r.Triggered || !strings.Contains(r.Reason, "does not listen") {
		t.Errorf("%+v", r)
	}
}

func TestBranchAndTagFilters(t *testing.T) {
	branch := func(b string) Event { return Event{Name: "push", Ref: "refs/heads/" + b} }
	tag := func(v string) Event { return Event{Name: "push", Ref: "refs/tags/" + v} }
	tests := []struct {
		on   string
		ev   Event
		want bool
	}{
		{"on:\n  push:\n    branches: [main, 'releases/**']", branch("main"), true},
		{"on:\n  push:\n    branches: [main, 'releases/**']", branch("releases/10"), true},
		{"on:\n  push:\n    branches: [main, 'releases/**']", branch("feature"), false},
		{"on:\n  push:\n    branches-ignore: ['docs/**']", branch("docs/a"), false},
		{"on:\n  push:\n    branches-ignore: ['docs/**']", branch("main"), true},
		// Only branches listed: tag pushes do not trigger, and the reverse.
		{"on:\n  push:\n    branches: [main]", tag("v1"), false},
		{"on:\n  push:\n    tags: ['v*']", branch("main"), false},
		{"on:\n  push:\n    tags: ['v*']", tag("v1.2"), true},
		{"on:\n  push:\n    tags: ['v*']", tag("x1"), false},
		{"on:\n  push:\n    tags-ignore: ['v*-rc']", tag("v1-rc"), false},
		{"on:\n  push:\n    tags-ignore: ['v*-rc']", tag("v1"), true},
		// Both given: each applies to its own kind of ref.
		{"on:\n  push:\n    branches: [main]\n    tags: ['v*']", tag("v1"), true},
		{"on:\n  push:\n    branches: [main]\n    tags: ['v*']", branch("main"), true},
		{"on:\n  push:\n    branches: [main]\n    tags: ['v*']", branch("dev"), false},
		{"on:\n  push:\n    branches: ['**', '!wip/**']", branch("wip/x"), false},
	}
	for _, tt := range tests {
		r := match(t, tt.on, tt.ev)
		if r.Triggered != tt.want {
			t.Errorf("%q on %s: triggered=%v (%s)", tt.on, tt.ev.Ref, r.Triggered, r.Reason)
		}
	}
}

func TestPullRequestFiltersUseTheBaseBranch(t *testing.T) {
	y := "on:\n  pull_request:\n    branches: [main]"
	if r := match(t, y, Event{Name: "pull_request", BaseRef: "main"}); !r.Triggered {
		t.Error(r.Reason)
	}
	if r := match(t, y, Event{Name: "pull_request", BaseRef: "refs/heads/release"}); r.Triggered {
		t.Error("a pull request to release matched branches: [main]")
	}
}

func TestActivityTypes(t *testing.T) {
	pr := func(action string) Event { return Event{Name: "pull_request", BaseRef: "main", Action: action} }
	for action, want := range map[string]bool{"opened": true, "synchronize": true, "reopened": true, "": true, "labeled": false, "closed": false} {
		if r := match(t, "on: pull_request", pr(action)); r.Triggered != want {
			t.Errorf("default types, %q: %v", action, r.Triggered)
		}
	}
	y := "on:\n  pull_request:\n    types: [labeled, closed]"
	if r := match(t, y, pr("labeled")); !r.Triggered {
		t.Error(r.Reason)
	}
	if r := match(t, y, pr("opened")); r.Triggered {
		t.Error("opened matched types: [labeled, closed]")
	}
	// An event without a default list triggers on any type.
	if r := match(t, "on: issues", Event{Name: "issues", Action: "edited"}); !r.Triggered {
		t.Error(r.Reason)
	}
	if r := match(t, "on:\n  release:\n    types: [published]", Event{Name: "release", Action: "created"}); r.Triggered {
		t.Error("created matched types: [published]")
	}
}

func TestPathFilters(t *testing.T) {
	push := func(files ...string) Event {
		if files == nil {
			files = []string{}
		}
		return Event{Name: "push", Ref: "refs/heads/main", Files: files}
	}
	paths := "on:\n  push:\n    paths:\n      - 'frontend/**'\n      - '!frontend/docs/**'"
	for _, tt := range []struct {
		files []string
		want  bool
	}{
		{[]string{"frontend/a.js"}, true},
		{[]string{"backend/a.go", "frontend/b.js"}, true},
		{[]string{"backend/a.go"}, false},
		{[]string{"frontend/docs/x.md"}, false},
		{[]string{"frontend/docs/x.md", "frontend/src/y.js"}, true},
		{nil, false},
	} {
		if r := match(t, paths, push(tt.files...)); r.Triggered != tt.want {
			t.Errorf("paths with %v: %v (%s)", tt.files, r.Triggered, r.Reason)
		}
	}
	ignore := "on:\n  push:\n    paths-ignore: ['docs/**', '**.md']"
	for _, tt := range []struct {
		files []string
		want  bool
	}{
		{[]string{"docs/a.txt", "README.md"}, false},
		{[]string{"docs/a.txt", "main.go"}, true},
		{[]string{"main.go"}, true},
	} {
		if r := match(t, ignore, push(tt.files...)); r.Triggered != tt.want {
			t.Errorf("paths-ignore with %v: %v (%s)", tt.files, r.Triggered, r.Reason)
		}
	}
	// Unknown files: the filter cannot be applied, so it does not block.
	r := match(t, paths, Event{Name: "push", Ref: "refs/heads/main"})
	if !r.Triggered || !strings.Contains(r.Reason, "not known") {
		t.Errorf("%+v", r)
	}
	// Tag pushes skip path filters.
	if r := match(t, paths, Event{Name: "push", Ref: "refs/tags/v1", Files: []string{"backend/a.go"}}); !r.Triggered {
		t.Errorf("a tag push was held back by paths: %s", r.Reason)
	}
}

func TestConflictingFiltersAreErrors(t *testing.T) {
	for _, y := range []string{
		"on:\n  push:\n    branches: [a]\n    branches-ignore: [b]",
		"on:\n  push:\n    tags: [a]\n    tags-ignore: [b]",
		"on:\n  push:\n    paths: [a]\n    paths-ignore: [b]",
		"on:\n  pull_request:\n    tags: [a]",
		"on:\n  push:\n    branches: ['[abc']",
	} {
		ev := Event{Name: "push", Ref: "refs/heads/main", Files: []string{"a"}}
		if strings.Contains(y, "pull_request") {
			ev = Event{Name: "pull_request", BaseRef: "main"}
		}
		if _, err := Match(on(t, y), ev); err == nil {
			t.Errorf("%q was accepted", y)
		}
	}
}

func TestSchedule(t *testing.T) {
	y := "on:\n  schedule:\n    - cron: '30 5 * * 1,3'\n    - cron: '0 0 * * *'"
	if r := match(t, y, Event{Name: "schedule", Cron: "0 0 * * *"}); !r.Triggered {
		t.Error(r.Reason)
	}
	if r := match(t, y, Event{Name: "schedule", Cron: "1 1 * * *"}); r.Triggered {
		t.Error("an unknown cron triggered")
	}
	crons, err := Schedules(on(t, y))
	if err != nil || len(crons) != 2 || crons[0] != "30 5 * * 1,3" {
		t.Errorf("%v %v", crons, err)
	}
	if _, err := Schedules(on(t, "on:\n  schedule:\n    - cron: '* * *'")); err == nil {
		t.Error("a cron with three fields was accepted")
	}
}

func TestDispatchInputs(t *testing.T) {
	y := `on:
  workflow_dispatch:
    inputs:
      env:
        type: choice
        options: [staging, prod]
        required: true
      dry:
        type: boolean
        default: true
      count:
        type: number
        default: 3
      note:
        description: free text`
	o := on(t, y)
	got, err := DispatchInputs(o, map[string]string{"env": "prod", "count": "5"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := got.Get("env"); v != "prod" {
		t.Errorf("env = %v", v)
	}
	if v, _ := got.Get("dry"); v != true {
		t.Errorf("dry = %#v", v)
	}
	if v, _ := got.Get("count"); v != float64(5) {
		t.Errorf("count = %#v", v)
	}
	if v, _ := got.Get("note"); v != "" {
		t.Errorf("note = %#v", v)
	}
	for name, in := range map[string]map[string]string{
		"missing required": {},
		"bad choice":       {"env": "dev"},
		"bad boolean":      {"env": "prod", "dry": "maybe"},
		"bad number":       {"env": "prod", "count": "12abc"},
		"unknown input":    {"env": "prod", "nope": "1"},
	} {
		if _, err := DispatchInputs(o, in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if r := match(t, y, Event{Name: "workflow_dispatch"}); !r.Triggered {
		t.Error(r.Reason)
	}
}
