package affected

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Raccoon-CI/raccoonci/pkg/expr"
	"github.com/Raccoon-CI/raccoonci/pkg/trigger"
	"github.com/Raccoon-CI/raccoonci/pkg/workflow"
)

// ConfigPath is where a repository can say what its jobs depend on.
const ConfigPath = ".raccoonci/affected.yml"

// Config is the optional, explicit side. It lives outside the workflow files
// because GitHub rejects keys it does not know there.
//
//	ignore:            # changes that never concern jobs tied to a directory
//	  - 'docs/**'
//	global:            # changes that concern every job
//	  - 'go.work'
//	jobs:              # what a job depends on, replacing what is inferred
//	  deploy: ['backend/**', 'infra/**']
type Config struct {
	Ignore []string            `yaml:"ignore"`
	Global []string            `yaml:"global"`
	Jobs   map[string][]string `yaml:"jobs"`
}

// LoadConfig reads ConfigPath from the repository; a missing file is an empty
// configuration.
func LoadConfig(repo fs.FS) (*Config, error) {
	b, ok := read(repo, ConfigPath)
	if !ok {
		return &Config{}, nil
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", ConfigPath, err)
	}
	lists := [][]string{c.Ignore, c.Global}
	for _, l := range c.Jobs {
		lists = append(lists, l)
	}
	for _, list := range lists {
		if _, err := trigger.MatchList(list, "x"); err != nil {
			return nil, fmt.Errorf("%s: %w", ConfigPath, err)
		}
	}
	return &c, nil
}

// defaultIgnore are changes that cannot alter a job tied to a directory:
// prose and licence files outside it.
var defaultIgnore = []string{"**.md", "LICENSE*", "COPYING*", ".gitignore", ".editorconfig"}

// Decision is whether a job has to run, and the reason in a sentence.
type Decision struct {
	Affected bool
	Reason   string
}

// Input is what Decide looks at.
type Input struct {
	Workflow     *workflow.Workflow
	WorkflowPath string // of this workflow in the repository
	// Files are the changed files. nil means unknown: everything is affected.
	Files  []string
	Repo   fs.FS
	Config *Config
}

// Decide says, per job id, whether the changes concern it.
func Decide(in Input) map[string]Decision {
	out := map[string]Decision{}
	all := func(why string) map[string]Decision {
		for _, j := range in.Workflow.Jobs {
			out[j.ID] = Decision{true, why}
		}
		return out
	}
	if in.Files == nil {
		return all("the changed files are not known")
	}
	cfg := in.Config
	if cfg == nil {
		cfg = &Config{}
	}
	pkgs := Packages(in.Repo)

	// What a change to each file means for all jobs at once.
	for _, f := range in.Files {
		switch {
		case f == in.WorkflowPath:
			return all(f + " (the workflow itself) changed")
		case strings.HasPrefix(f, ".github/actions/"), f == ConfigPath:
			return all(f + " changed")
		case matches(cfg.Global, f):
			return all(f + " is listed as global in " + ConfigPath)
		}
	}

	type footprint struct {
		whole    bool // not tied to anything: any change concerns it
		dirs     []string
		patterns []string // explicit
	}
	prints := map[string]*footprint{}
	areas := map[string]bool{}
	for _, p := range pkgs {
		areas[p.Dir] = true
	}
	for _, j := range in.Workflow.Jobs {
		fp := &footprint{}
		prints[j.ID] = fp
		switch {
		case len(cfg.Jobs[j.ID]) > 0:
			fp.patterns = cfg.Jobs[j.ID]
		default:
			dirs, ok := jobDirs(j)
			if !ok {
				fp.whole = true
				break
			}
			fp.dirs = closure(dirs, pkgs)
			for _, d := range dirs {
				areas[d] = true
			}
		}
	}

	for _, j := range in.Workflow.Jobs {
		fp := prints[j.ID]
		d := Decision{Reason: "nothing it depends on changed"}
		for _, f := range in.Files {
			var why string
			switch {
			case fp.whole:
				why = "it is not tied to a directory, so " + f + " concerns it"
			case len(fp.patterns) > 0:
				if matches(fp.patterns, f) {
					why = f + " matches its entry in " + ConfigPath
				}
			case within(fp.dirs, f):
				why = f + " is in what it depends on"
			case !inAny(areas, f) && !matches(cfg.Ignore, f) && !matches(defaultIgnore, f):
				why = f + " belongs to no package or job directory (list it under ignore: in " + ConfigPath + " if it does not matter)"
			}
			if why != "" {
				d = Decision{true, why}
				break
			}
		}
		out[j.ID] = d
	}
	return out
}

func matches(patterns []string, f string) bool {
	ok, err := trigger.MatchList(patterns, f)
	return err == nil && ok
}

func within(dirs []string, f string) bool {
	return slices.ContainsFunc(dirs, func(d string) bool { return contains(d, f) })
}

func inAny(areas map[string]bool, f string) bool {
	for d := range areas {
		if contains(d, f) {
			return true
		}
	}
	return false
}

// contains reports whether file f is dir or below it.
func contains(dir, f string) bool {
	return dir == "." || f == dir || strings.HasPrefix(f, dir+"/")
}

// closure returns dirs plus the directories of the workspace packages that lie
// in them or contain them, and everything those depend on.
func closure(dirs []string, pkgs []Package) []string {
	byDir := map[string]Package{}
	for _, p := range pkgs {
		byDir[p.Dir] = p
	}
	set := map[string]bool{}
	var visit func(dir string)
	visit = func(dir string) {
		if set[dir] {
			return
		}
		set[dir] = true
		for _, dep := range byDir[dir].Deps {
			visit(dep)
		}
	}
	for _, d := range dirs {
		visit(d)
		for _, p := range pkgs {
			if contains(d, p.Dir) || contains(p.Dir, d) {
				visit(p.Dir)
			}
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// jobDirs infers the directories a job works in: the working directory of
// its run steps (or the job's default) and the local actions it calls. ok is
// false when it cannot tell, for example a run step in the repository root,
// or a directory only known at run time.
func jobDirs(j *workflow.Job) (dirs []string, ok bool) {
	if j.Uses != "" {
		return nil, false // a reusable workflow: its steps are elsewhere
	}
	add := func(raw string) bool {
		d := expr.InterpolateLoose(raw, &expr.Context{}, "\x00")
		if strings.Contains(d, "\x00") || path.IsAbs(d) {
			return false
		}
		d = path.Clean(strings.TrimPrefix(d, "./"))
		if d == "." || d == ".." || strings.HasPrefix(d, "../") {
			return false
		}
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
		return true
	}
	for _, s := range j.Steps {
		switch {
		case s.Uses != "":
			if rest, ok := strings.CutPrefix(s.Uses, "./"); ok {
				if !add(rest) {
					return nil, false
				}
			}
		case s.Run != "":
			wd := s.WorkingDirectory
			if wd == "" {
				wd = j.Defaults.WorkingDirectory
			}
			if wd == "" || !add(wd) {
				return nil, false
			}
		}
	}
	return dirs, len(dirs) > 0
}

// Skippable says whether leaving the job out when it is not affected is
// safe: it must not hand anything to other jobs (outputs, uploaded
// artifacts), because skipping it would leave them without. Dependents see a
// skipped job as having succeeded.
func Skippable(j *workflow.Job) (bool, string) {
	if len(j.Outputs) > 0 {
		return false, "it has outputs other jobs may use"
	}
	for _, s := range j.Steps {
		if strings.HasPrefix(strings.ToLower(s.Uses), "actions/upload-artifact") {
			return false, "it uploads an artifact other jobs may download"
		}
	}
	return true, ""
}
