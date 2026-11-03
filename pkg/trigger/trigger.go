// Package trigger decides whether an event starts a workflow, from the
// workflow's `on:`: which events, and for push and pull requests the branch,
// tag and path filters and the activity types, with GitHub's rules
// (glob.go has the pattern syntax). It also resolves the inputs of
// workflow_dispatch and checks schedule entries.
//
// It knows nothing about how the event arrived. The caller describes it in an
// Event; where the changed files come from (a push's compare, a pull
// request's diff) is the caller's business.
package trigger

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Raccoon-CI/lib/pkg/expr"
	"github.com/Raccoon-CI/lib/pkg/value"
)

// Event is what happened.
type Event struct {
	Name string // push, pull_request, workflow_dispatch, schedule, ...
	// Action is the activity type (opened, synchronize, labeled, ...). Empty
	// means the event's default.
	Action string
	// Ref is the full ref a push is about (refs/heads/main, refs/tags/v1).
	Ref string
	// BaseRef is the branch a pull request targets; its branch filters apply
	// to that, not to the head branch.
	BaseRef string
	// Files are the changed files, slash-separated, relative to the
	// repository root. nil means unknown: path filters are then not applied.
	Files []string
	// Cron is the schedule entry that fired, for the schedule event.
	Cron string
}

// Result says whether the event starts the workflow, and why not if it doesn't.
type Result struct {
	Triggered bool
	Reason    string
}

// defaultTypes are the activity types some events trigger on when the
// workflow gives no `types:`. Events missing here trigger on every type.
var defaultTypes = map[string][]string{
	"pull_request":        {"opened", "synchronize", "reopened"},
	"pull_request_target": {"opened", "synchronize", "reopened"},
}

// events parses `on:` into one config per event; a config is nil for an
// event listed without options.
func events(on any) (map[string]value.Object, error) {
	out := map[string]value.Object{}
	switch on := on.(type) {
	case nil:
		return nil, fmt.Errorf("the workflow has no 'on:'")
	case string:
		out[on] = nil
	case []any:
		for _, e := range on {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("on: event names must be strings, got %v", e)
			}
			out[s] = nil
		}
	case value.Object:
		for _, f := range on {
			switch c := f.Value.(type) {
			case nil:
				out[f.Key] = nil
			case value.Object:
				out[f.Key] = c
			case []any:
				// schedule: is a list of {cron: ...}; keep it under a key.
				out[f.Key] = value.Object{{Key: "", Value: c}}
			default:
				return nil, fmt.Errorf("on.%s: expected a mapping, got %v", f.Key, f.Value)
			}
		}
	default:
		return nil, fmt.Errorf("on: expected an event name, a list or a mapping")
	}
	return out, nil
}

func strList(cfg value.Object, key string) (list []string, present bool, err error) {
	v, ok := cfg.Get(key)
	if !ok {
		return nil, false, nil
	}
	switch v := v.(type) {
	case string:
		return []string{v}, true, nil
	case []any:
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, true, fmt.Errorf("%s: entries must be strings, got %v", key, e)
			}
			list = append(list, s)
		}
		return list, true, nil
	case nil:
		return nil, true, nil
	}
	return nil, true, fmt.Errorf("%s: expected a list of patterns", key)
}

// Match reports whether ev starts a workflow with this `on:` (Workflow.On).
func Match(on any, ev Event) (Result, error) {
	evs, err := events(on)
	if err != nil {
		return Result{}, err
	}
	cfg, listed := evs[ev.Name]
	if !listed {
		return Result{Reason: fmt.Sprintf("the workflow does not listen to %s", ev.Name)}, nil
	}

	switch ev.Name {
	case "schedule":
		return matchSchedule(cfg, ev)
	case "workflow_dispatch":
		return Result{Triggered: true}, nil
	}

	types, hasTypes, err := strList(cfg, "types")
	if err != nil {
		return Result{}, fmt.Errorf("on.%s: %w", ev.Name, err)
	}
	if !hasTypes {
		types = defaultTypes[ev.Name]
		hasTypes = types != nil
	}
	if hasTypes && ev.Action != "" && !slices.Contains(types, ev.Action) {
		return Result{Reason: fmt.Sprintf("%s: the activity type %q is not in types %v", ev.Name, ev.Action, types)}, nil
	}

	switch ev.Name {
	case "push", "pull_request", "pull_request_target":
		return matchFilters(cfg, ev)
	}
	return Result{Triggered: true}, nil
}

func matchFilters(cfg value.Object, ev Event) (Result, error) {
	no := func(format string, args ...any) (Result, error) {
		return Result{Reason: ev.Name + ": " + fmt.Sprintf(format, args...)}, nil
	}
	fail := func(err error) (Result, error) { return Result{}, fmt.Errorf("on.%s: %w", ev.Name, err) }

	var branches, branchesIgnore, tags, tagsIgnore, paths, pathsIgnore []string
	var hasB, hasBI, hasT, hasTI, hasP, hasPI bool
	for _, f := range []struct {
		key     string
		list    *[]string
		present *bool
	}{
		{"branches", &branches, &hasB}, {"branches-ignore", &branchesIgnore, &hasBI},
		{"tags", &tags, &hasT}, {"tags-ignore", &tagsIgnore, &hasTI},
		{"paths", &paths, &hasP}, {"paths-ignore", &pathsIgnore, &hasPI},
	} {
		var err error
		if *f.list, *f.present, err = strList(cfg, f.key); err != nil {
			return fail(err)
		}
	}
	if hasB && hasBI {
		return fail(fmt.Errorf("'branches' and 'branches-ignore' cannot be used together"))
	}
	if hasT && hasTI {
		return fail(fmt.Errorf("'tags' and 'tags-ignore' cannot be used together"))
	}
	if hasP && hasPI {
		return fail(fmt.Errorf("'paths' and 'paths-ignore' cannot be used together"))
	}
	if ev.Name != "push" && (hasT || hasTI) {
		return fail(fmt.Errorf("'tags' filters only exist for push"))
	}
	hasBranchFilter, hasTagFilter := hasB || hasBI, hasT || hasTI

	// Which ref the filters look at.
	isTag := false
	name := ""
	switch ev.Name {
	case "push":
		switch {
		case strings.HasPrefix(ev.Ref, "refs/tags/"):
			isTag, name = true, strings.TrimPrefix(ev.Ref, "refs/tags/")
		case strings.HasPrefix(ev.Ref, "refs/heads/"):
			name = strings.TrimPrefix(ev.Ref, "refs/heads/")
		default:
			name = ev.Ref
		}
	default:
		name = strings.TrimPrefix(ev.BaseRef, "refs/heads/")
	}

	switch {
	case ev.Name == "push" && isTag:
		if hasTagFilter {
			ok, err := refFilter(tags, hasT, tagsIgnore, name)
			if err != nil {
				return fail(err)
			}
			if !ok {
				return no("the tag %q does not match the tags filter", name)
			}
		} else if hasBranchFilter {
			return no("a tag was pushed, and the workflow only lists branches")
		}
	default:
		if hasBranchFilter {
			ok, err := refFilter(branches, hasB, branchesIgnore, name)
			if err != nil {
				return fail(err)
			}
			if !ok {
				return no("the branch %q does not match the branches filter", name)
			}
		} else if ev.Name == "push" && hasTagFilter {
			return no("a branch was pushed, and the workflow only lists tags")
		}
	}

	// Path filters are not evaluated for tag pushes.
	if (hasP || hasPI) && !(ev.Name == "push" && isTag) {
		if ev.Files == nil {
			return Result{Triggered: true, Reason: "the path filter was not applied: the changed files are not known"}, nil
		}
		ok := false
		for _, file := range ev.Files {
			if hasP {
				sel, err := matchList(paths, file)
				if err != nil {
					return fail(err)
				}
				ok = ok || sel
			} else {
				ign := false
				for _, p := range pathsIgnore {
					re, err := compile(p)
					if err != nil {
						return fail(err)
					}
					if re.MatchString(file) {
						ign = true
						break
					}
				}
				ok = ok || !ign
			}
		}
		if !ok {
			if hasP {
				return no("none of the %d changed files matches the paths filter", len(ev.Files))
			}
			return no("all %d changed files are matched by paths-ignore", len(ev.Files))
		}
	}
	return Result{Triggered: true}, nil
}

// refFilter applies a branches/tags filter (list) or its -ignore sibling.
func refFilter(list []string, isList bool, ignore []string, name string) (bool, error) {
	if isList {
		return matchList(list, name)
	}
	for _, p := range ignore {
		re, err := compile(p)
		if err != nil {
			return false, err
		}
		if re.MatchString(name) {
			return false, nil
		}
	}
	return true, nil
}

func matchSchedule(cfg value.Object, ev Event) (Result, error) {
	crons, err := scheduleCrons(cfg)
	if err != nil {
		return Result{}, err
	}
	for _, c := range crons {
		if c == strings.TrimSpace(ev.Cron) {
			return Result{Triggered: true}, nil
		}
	}
	return Result{Reason: fmt.Sprintf("schedule: no entry has the cron %q", ev.Cron)}, nil
}

// scheduleCrons lists the cron expressions of on.schedule, checked for shape.
func scheduleCrons(cfg value.Object) ([]string, error) {
	raw, _ := cfg.Get("")
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("on.schedule: expected a list of {cron: ...}")
	}
	var out []string
	for _, e := range list {
		o, ok := e.(value.Object)
		if !ok {
			return nil, fmt.Errorf("on.schedule: entries need a 'cron' key")
		}
		v, _ := o.Get("cron")
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("on.schedule: entries need a 'cron' key")
		}
		s = strings.TrimSpace(s)
		if n := len(strings.Fields(s)); n != 5 {
			return nil, fmt.Errorf("on.schedule: cron %q has %d fields, want 5", s, n)
		}
		out = append(out, s)
	}
	return out, nil
}

// Schedules returns the cron expressions the workflow runs on, if any.
func Schedules(on any) ([]string, error) {
	evs, err := events(on)
	if err != nil {
		return nil, err
	}
	cfg, ok := evs["schedule"]
	if !ok {
		return nil, nil
	}
	return scheduleCrons(cfg)
}

// DispatchInputs resolves the inputs of a workflow_dispatch run: defaults
// fill what was not given, required inputs must be given, choices must be
// one of their options, and unknown inputs are refused, as GitHub does.
// Booleans and numbers get their types, since `inputs.x` is typed there.
func DispatchInputs(on any, given map[string]string) (value.Object, error) {
	evs, err := events(on)
	if err != nil {
		return nil, err
	}
	cfg, ok := evs["workflow_dispatch"]
	if !ok {
		return nil, fmt.Errorf("the workflow has no workflow_dispatch trigger")
	}
	decl, _ := cfg.Get("inputs")
	declared, _ := decl.(value.Object)
	for name := range given {
		if _, ok := declared.Get(name); !ok {
			return nil, fmt.Errorf("unexpected input %q: the workflow declares %s", name, inputNames(declared))
		}
	}
	var out value.Object
	for _, f := range declared {
		spec, _ := f.Value.(value.Object)
		typ := "string"
		if t, ok := spec.Get("type"); ok && t != nil {
			typ = expr.ToString(t)
		}
		raw, have := given[f.Key]
		if !have {
			if d, ok := spec.Get("default"); ok && d != nil {
				raw, have = expr.ToString(d), true
			}
		}
		if req, _ := spec.Get("required"); expr.Truthy(req) && !have {
			return nil, fmt.Errorf("input %q is required", f.Key)
		}
		if !have {
			if typ == "boolean" {
				out = append(out, value.Field{Key: f.Key, Value: false})
			} else {
				out = append(out, value.Field{Key: f.Key, Value: ""})
			}
			continue
		}
		switch typ {
		case "boolean":
			switch strings.ToLower(raw) {
			case "true":
				out = append(out, value.Field{Key: f.Key, Value: true})
			case "false":
				out = append(out, value.Field{Key: f.Key, Value: false})
			default:
				return nil, fmt.Errorf("input %q is a boolean, got %q", f.Key, raw)
			}
		case "number":
			n, ok := parseNumber(raw)
			if !ok {
				return nil, fmt.Errorf("input %q is a number, got %q", f.Key, raw)
			}
			out = append(out, value.Field{Key: f.Key, Value: n})
		case "choice":
			opts, _ := spec.Get("options")
			list, _ := opts.([]any)
			if !slices.ContainsFunc(list, func(o any) bool { return expr.ToString(o) == raw }) {
				return nil, fmt.Errorf("input %q must be one of %v, got %q", f.Key, list, raw)
			}
			out = append(out, value.Field{Key: f.Key, Value: raw})
		default:
			out = append(out, value.Field{Key: f.Key, Value: raw})
		}
	}
	return out, nil
}

func inputNames(o value.Object) string {
	if len(o) == 0 {
		return "no inputs"
	}
	var names []string
	for _, f := range o {
		names = append(names, f.Key)
	}
	return strings.Join(names, ", ")
}

func parseNumber(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}
