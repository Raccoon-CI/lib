// Package workflow parses GitHub Actions workflow files into Raccoon CI's job
// model. Fields that GitHub lets users write as either literals or ${{ }}
// expressions are kept as raw values (see package value) and evaluated later,
// once their contexts are known.
package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Raccoon-CI/lib/pkg/value"
)

type Workflow struct {
	Name        string
	RunName     string
	On          any
	Permissions any
	Env         any // value.Object, or an expression string
	Defaults    Defaults
	Concurrency any
	Jobs        []*Job // in file order
}

type Defaults struct {
	Shell            string
	WorkingDirectory string
}

type Job struct {
	ID              string
	Name            string
	Needs           []string
	If              string
	RunsOn          any
	Permissions     any
	Environment     any
	Concurrency     any
	Outputs         value.Object
	Env             any
	Defaults        Defaults
	TimeoutMinutes  any
	ContinueOnError any
	Strategy        *Strategy
	Container       any
	Services        any
	Steps           []*Step

	// Uses, With and Secrets are set when the job calls a reusable workflow.
	Uses    string
	With    value.Object
	Secrets any
}

type Strategy struct {
	// Matrix is a value.Object or an expression string. It may contain ${{ }}
	// expressions, which must be evaluated before calling ExpandMatrix.
	Matrix      any
	FailFast    any // GitHub's default is true
	MaxParallel any
}

type Step struct {
	ID               string
	Name             string
	If               string
	Uses             string
	Run              string
	Shell            string
	WorkingDirectory string
	With             value.Object
	Env              any
	ContinueOnError  any
	TimeoutMinutes   any
}

func (w *Workflow) Job(id string) *Job {
	for _, j := range w.Jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// Job and step IDs: a letter or underscore, then letters, digits, - or _.
var idPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// Parse parses and validates a workflow file. Like GitHub, it rejects unknown
// keys, so typos such as `run-on` fail loudly instead of being ignored.
func Parse(src []byte) (*Workflow, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("workflow file is empty")
	}
	p := &parser{dec: value.NewDecoder(value.DefaultNodeBudget)}
	w, err := p.workflow(doc.Content[0])
	if err != nil {
		return nil, err
	}
	if err := checkNeeds(w); err != nil {
		return nil, err
	}
	return w, nil
}

type parser struct {
	dec *value.Decoder
}

func (p *parser) workflow(n *yaml.Node) (*Workflow, error) {
	w := &Workflow{}
	var jobsNode *yaml.Node
	err := fields(n, func(k, v *yaml.Node) (err error) {
		switch k.Value {
		case "name":
			w.Name, err = str(v)
		case "run-name":
			w.RunName, err = str(v)
		case "on":
			w.On, err = p.dec.Decode(v)
		case "permissions":
			w.Permissions, err = p.dec.Decode(v)
		case "env":
			w.Env, err = p.dec.Decode(v)
		case "defaults":
			w.Defaults, err = defaults(v)
		case "concurrency":
			w.Concurrency, err = p.dec.Decode(v)
		case "jobs":
			jobsNode = v
		default:
			err = unknownKey(k, "workflow")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if jobsNode == nil {
		return nil, errors.New(`workflow has no "jobs"`)
	}
	err = fields(jobsNode, func(k, v *yaml.Node) error {
		if !idPattern.MatchString(k.Value) {
			return errAt(k, "invalid job id %q: must start with a letter or _ and contain only letters, digits, - and _", k.Value)
		}
		j, err := p.job(k.Value, v)
		if err != nil {
			return err
		}
		w.Jobs = append(w.Jobs, j)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(w.Jobs) == 0 {
		return nil, errAt(jobsNode, `"jobs" must define at least one job`)
	}
	return w, nil
}

func (p *parser) job(id string, n *yaml.Node) (*Job, error) {
	j := &Job{ID: id}
	var steps, runsOn *yaml.Node
	err := fields(n, func(k, v *yaml.Node) (err error) {
		switch k.Value {
		case "name":
			j.Name, err = str(v)
		case "needs":
			j.Needs, err = strList(v)
		case "if":
			j.If, err = str(v)
		case "runs-on":
			runsOn = v
			j.RunsOn, err = p.dec.Decode(v)
		case "permissions":
			j.Permissions, err = p.dec.Decode(v)
		case "environment":
			j.Environment, err = p.dec.Decode(v)
		case "concurrency":
			j.Concurrency, err = p.dec.Decode(v)
		case "outputs":
			j.Outputs, err = p.object(v)
		case "env":
			j.Env, err = p.dec.Decode(v)
		case "defaults":
			j.Defaults, err = defaults(v)
		case "timeout-minutes":
			j.TimeoutMinutes, err = p.dec.Decode(v)
		case "continue-on-error":
			j.ContinueOnError, err = p.dec.Decode(v)
		case "strategy":
			j.Strategy, err = p.strategy(v)
		case "container":
			j.Container, err = p.dec.Decode(v)
		case "services":
			j.Services, err = p.dec.Decode(v)
		case "steps":
			steps = v
		case "uses":
			j.Uses, err = str(v)
		case "with":
			j.With, err = p.object(v)
		case "secrets":
			j.Secrets, err = p.dec.Decode(v)
		default:
			err = unknownKey(k, fmt.Sprintf("job %q", id))
		}
		return err
	})
	if err != nil {
		return nil, err
	}

	if j.Uses != "" {
		if steps != nil || runsOn != nil {
			return nil, errAt(n, `job %q calls a reusable workflow ("uses"), so it cannot have "steps" or "runs-on"`, id)
		}
		return j, nil
	}
	if runsOn == nil {
		return nil, errAt(n, `job %q has no "runs-on"`, id)
	}
	if steps == nil {
		return nil, errAt(n, `job %q has no "steps"`, id)
	}
	j.Steps, err = p.steps(fmt.Sprintf("job %q", id), steps)
	if err != nil {
		return nil, err
	}
	return j, nil
}

// DecodeSteps parses a list of steps outside a workflow file, such as the
// steps of a composite action. where names the list in error messages.
func DecodeSteps(dec *value.Decoder, n *yaml.Node, where string) ([]*Step, error) {
	p := &parser{dec: dec}
	return p.steps(where, deref(n))
}

func (p *parser) steps(where string, n *yaml.Node) ([]*Step, error) {
	var out []*Step
	stepIDs := map[string]bool{}
	err := items(n, func(i int, sn *yaml.Node) error {
		s, err := p.step(fmt.Sprintf("%s step %d", where, i+1), sn)
		if err != nil {
			return err
		}
		if s.ID != "" {
			if stepIDs[s.ID] {
				return errAt(sn, "%s: step id %q is used more than once", where, s.ID)
			}
			stepIDs[s.ID] = true
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

func (p *parser) step(where string, n *yaml.Node) (*Step, error) {
	s := &Step{}
	hasUses, hasRun := false, false
	err := fields(n, func(k, v *yaml.Node) (err error) {
		switch k.Value {
		case "id":
			s.ID, err = str(v)
			if err == nil && !idPattern.MatchString(s.ID) {
				err = errAt(v, "%s: invalid step id %q: must start with a letter or _ and contain only letters, digits, - and _", where, s.ID)
			}
		case "name":
			s.Name, err = str(v)
		case "if":
			s.If, err = str(v)
		case "uses":
			hasUses = true
			s.Uses, err = str(v)
		case "run":
			hasRun = true
			s.Run, err = str(v)
		case "shell":
			s.Shell, err = str(v)
		case "working-directory":
			s.WorkingDirectory, err = str(v)
		case "with":
			s.With, err = p.object(v)
		case "env":
			s.Env, err = p.dec.Decode(v)
		case "continue-on-error":
			s.ContinueOnError, err = p.dec.Decode(v)
		case "timeout-minutes":
			s.TimeoutMinutes, err = p.dec.Decode(v)
		default:
			err = unknownKey(k, where)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if hasUses == hasRun {
		return nil, errAt(n, `%s must have exactly one of "uses" or "run"`, where)
	}
	return s, nil
}

func (p *parser) strategy(n *yaml.Node) (*Strategy, error) {
	s := &Strategy{}
	err := fields(n, func(k, v *yaml.Node) (err error) {
		switch k.Value {
		case "matrix":
			s.Matrix, err = p.dec.Decode(v)
		case "fail-fast":
			s.FailFast, err = p.dec.Decode(v)
		case "max-parallel":
			s.MaxParallel, err = p.dec.Decode(v)
		default:
			err = unknownKey(k, "strategy")
		}
		return err
	})
	return s, err
}

func (p *parser) object(n *yaml.Node) (value.Object, error) {
	v, err := p.dec.Decode(n)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(value.Object)
	if !ok {
		return nil, errAt(n, "expected a mapping")
	}
	return obj, nil
}

func defaults(n *yaml.Node) (Defaults, error) {
	var d Defaults
	err := fields(n, func(k, v *yaml.Node) error {
		if k.Value != "run" {
			return unknownKey(k, "defaults")
		}
		return fields(v, func(k, v *yaml.Node) (err error) {
			switch k.Value {
			case "shell":
				d.Shell, err = str(v)
			case "working-directory":
				d.WorkingDirectory, err = str(v)
			default:
				err = unknownKey(k, "defaults.run")
			}
			return err
		})
	})
	return d, err
}

// checkNeeds rejects needs that name unknown jobs and dependency cycles,
// both of which GitHub refuses before running anything.
func checkNeeds(w *Workflow) error {
	for _, j := range w.Jobs {
		for _, need := range j.Needs {
			if w.Job(need) == nil {
				return fmt.Errorf("job %q needs %q, which does not exist", j.ID, need)
			}
		}
	}
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var path []string
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			cycle := append(path[slices.Index(path, id):], id)
			return fmt.Errorf("jobs form a dependency cycle: %s", strings.Join(cycle, " -> "))
		case done:
			return nil
		}
		state[id] = visiting
		path = append(path, id)
		for _, need := range w.Job(id).Needs {
			if err := visit(need); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[id] = done
		return nil
	}
	for _, j := range w.Jobs {
		if err := visit(j.ID); err != nil {
			return err
		}
	}
	return nil
}

// fields calls fn for each key/value of a mapping, rejecting duplicate keys.
func fields(n *yaml.Node, fn func(k, v *yaml.Node) error) error {
	n = deref(n)
	if n.Kind != yaml.MappingNode {
		return errAt(n, "expected a mapping")
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if seen[k.Value] {
			return errAt(k, "duplicate key %q", k.Value)
		}
		seen[k.Value] = true
		if err := fn(k, deref(n.Content[i+1])); err != nil {
			return err
		}
	}
	return nil
}

func items(n *yaml.Node, fn func(i int, v *yaml.Node) error) error {
	n = deref(n)
	if n.Kind != yaml.SequenceNode {
		return errAt(n, "expected a list")
	}
	for i, c := range n.Content {
		if err := fn(i, deref(c)); err != nil {
			return err
		}
	}
	return nil
}

func str(n *yaml.Node) (string, error) {
	if n.Kind != yaml.ScalarNode {
		return "", errAt(n, "expected a string")
	}
	return n.Value, nil
}

func strList(n *yaml.Node) ([]string, error) {
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}, nil
	}
	var out []string
	err := items(n, func(_ int, v *yaml.Node) error {
		s, err := str(v)
		out = append(out, s)
		return err
	})
	return out, err
}

func deref(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func unknownKey(k *yaml.Node, where string) error {
	return errAt(k, "unknown key %q in %s", k.Value, where)
}

func errAt(n *yaml.Node, format string, args ...any) error {
	return fmt.Errorf("line %d: %s", n.Line, fmt.Sprintf(format, args...))
}
