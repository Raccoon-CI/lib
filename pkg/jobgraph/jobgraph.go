// Package jobgraph runs the graph of a workflow's jobs: it orders jobs by
// needs, evaluates job conditions, expands matrices (including ones built from
// earlier jobs' outputs), and hands each job instance to an executor. It does
// not know how an instance runs.
package jobgraph

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Raccoon-CI/lib/pkg/catalog"
	"github.com/Raccoon-CI/lib/pkg/expr"
	"github.com/Raccoon-CI/lib/pkg/platform"
	"github.com/Raccoon-CI/lib/pkg/runevent"
	"github.com/Raccoon-CI/lib/pkg/value"
	"github.com/Raccoon-CI/lib/pkg/workflow"
)

// Options is what the graph needs of a run. It rests on data packages only:
// running an instance is the Remote function's business, wherever it runs.
type Options struct {
	// Github is the run's github context, without the per-job keys.
	Github value.Object
	Vars   value.Object
	Inputs value.Object
	// Catalog defaults to catalog.Default().
	Catalog *catalog.Catalog
	// OnEvent, if set, receives the events the graph itself produces (a
	// skipped job, an error in a condition); the instances' events reach it
	// through Remote's own plumbing.
	OnEvent func(runevent.Event)
	// Jobs limits the run to these jobs and the jobs they need.
	Jobs []string
	// MaxParallel caps how many job instances run at once (default 1).
	MaxParallel int
	// Unaffected, if set, is asked for each job whose own `if:` allows it to
	// run. Returning true leaves the job out because the changes do not
	// concern it, with the reason. Jobs that need it see it as succeeded.
	Unaffected func(j *workflow.Job) (reason string, skip bool)
	// Remote runs a job instance (here or on another machine) and returns how
	// it ended. It is called once the instance holds a MaxParallel slot; an
	// error fails the instance.
	Remote func(ctx context.Context, inst RemoteInstance) (runevent.Result, error)
}

// RemoteInstance is one job instance as the graph resolved it: the inputs an
// executor needs, besides the workflow and the run's contexts, to run it.
type RemoteInstance struct {
	Job      *workflow.Job
	Name     string
	Matrix   value.Object
	Strategy value.Object
	Needs    value.Object
	// Toolchains are the names of the toolchains the instance uses, as a scan
	// found them (sorted by first use). The graph leaves it empty; an
	// executor that scans fills it.
	Toolchains []string
	// Context is the expression context of the instance (github, needs,
	// vars, inputs, matrix, strategy) and RunsOn its evaluated runs-on, for the
	// executor to scan and to choose an image with.
	Context *expr.Context
	RunsOn  any
}

// InstanceResult is how one instance ended.
type InstanceResult struct {
	Conclusion runevent.Conclusion
	Outputs    value.Object
	Summary    string // the job summary, as the runner collected it
}

// RunInstance runs one job instance (as Run does for each matrix combination,
// after it has resolved the matrix and the needs).
func RunInstance(ctx context.Context, w *workflow.Workflow, opts Options, job *workflow.Job, matrix, strategy, needs value.Object) InstanceResult {
	g := newGraph(w, opts)
	r := g.runInstance(ctx, job, matrix, strategy, needs)
	return InstanceResult{Conclusion: r.conclusion, Outputs: r.outputs, Summary: r.summary}
}

type JobResult struct {
	ID         string
	Conclusion runevent.Conclusion
	Outputs    value.Object
	// Unchanged is set, with the reason, for a job that was left out because
	// nothing it depends on changed (Options.Unaffected). Its Conclusion is
	// Success, so jobs that need it go on.
	Unchanged string
}

type Result struct {
	Conclusion runevent.Conclusion
	Jobs       []*JobResult // in workflow order
}

func newGraph(w *workflow.Workflow, opts Options) *engine {
	if opts.MaxParallel < 1 {
		opts.MaxParallel = 1
	}
	if opts.Catalog == nil {
		opts.Catalog = catalog.Default()
	}
	return &engine{w: w, opts: opts, sem: make(chan struct{}, opts.MaxParallel),
		done: map[string]chan struct{}{}, res: map[string]*JobResult{}}
}

type engine struct {
	w    *workflow.Workflow
	opts Options
	sem  chan struct{}
	seq  int
	mu   sync.Mutex
	done map[string]chan struct{}
	res  map[string]*JobResult
}

func Run(ctx context.Context, w *workflow.Workflow, opts Options) (*Result, error) {
	selected, err := selectJobs(w, opts.Jobs)
	if err != nil {
		return nil, err
	}
	e := newGraph(w, opts)
	for _, j := range selected {
		e.done[j.ID] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for _, j := range selected {
		wg.Go(func() {
			defer close(e.done[j.ID])
			r := e.runJob(ctx, j)
			e.mu.Lock()
			e.res[j.ID] = r
			e.mu.Unlock()
		})
	}
	wg.Wait()

	out := &Result{Conclusion: runevent.Success}
	for _, j := range selected {
		r := e.res[j.ID]
		out.Jobs = append(out.Jobs, r)
		switch r.Conclusion {
		case runevent.Failure:
			out.Conclusion = runevent.Failure
		case runevent.Cancelled:
			if out.Conclusion == runevent.Success {
				out.Conclusion = runevent.Cancelled
			}
		}
	}
	return out, nil
}

// selectJobs returns the named jobs plus everything they need, in workflow
// order; all jobs when none are named.
func selectJobs(w *workflow.Workflow, ids []string) ([]*workflow.Job, error) {
	if len(ids) == 0 {
		return w.Jobs, nil
	}
	want := map[string]bool{}
	var add func(id string) error
	add = func(id string) error {
		j := w.Job(id)
		if j == nil {
			return fmt.Errorf("no job %q in the workflow", id)
		}
		if want[id] {
			return nil
		}
		want[id] = true
		for _, n := range j.Needs {
			if err := add(n); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range ids {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	var out []*workflow.Job
	for _, j := range w.Jobs {
		if want[j.ID] {
			out = append(out, j)
		}
	}
	return out, nil
}

func (e *engine) emit(ev runevent.Event) {
	if e.opts.OnEvent != nil {
		e.opts.OnEvent(ev)
	}
}

func (e *engine) result(id string) *JobResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.res[id]
}

// ancestorFailed reports whether any job j depends on, directly or not,
// failed: failure() is true for the whole chain below a failed job.
func (e *engine) ancestorFailed(j *workflow.Job) bool {
	for _, n := range j.Needs {
		if e.result(n).Conclusion == runevent.Failure || e.ancestorFailed(e.w.Job(n)) {
			return true
		}
	}
	return false
}

func (e *engine) runJob(ctx context.Context, j *workflow.Job) *JobResult {
	res := &JobResult{ID: j.ID, Conclusion: runevent.Skipped}
	needs := value.Object{}
	skippedNeed := false
	for _, n := range j.Needs {
		select {
		case <-e.done[n]:
		case <-ctx.Done():
			<-e.done[n]
		}
		r := e.result(n)
		if r.Conclusion != runevent.Success {
			skippedNeed = true
		}
		outputs := r.Outputs
		if outputs == nil {
			outputs = value.Object{}
		}
		needs = append(needs, value.Field{Key: n, Value: value.Object{
			{Key: "result", Value: string(r.Conclusion)},
			{Key: "outputs", Value: outputs},
		}})
	}

	ectx := &expr.Context{Contexts: map[string]any{
		"github": e.opts.Github,
		"needs":  needs,
		"vars":   orEmpty(e.opts.Vars),
		"inputs": orEmpty(e.opts.Inputs),
	}, Status: &expr.Status{
		Failed:    e.ancestorFailed(j),
		Cancelled: ctx.Err() != nil,
		Skipped:   skippedNeed,
	}}
	fail := func(format string, args ...any) *JobResult {
		e.emit(runevent.Event{Kind: runevent.Error, Job: j.ID, Text: fmt.Sprintf(format, args...)})
		res.Conclusion = runevent.Failure
		return res
	}
	ok, err := expr.EvalCondition(j.If, ectx)
	if err != nil {
		return fail("if: %v", err)
	}
	if !ok {
		e.emit(runevent.Event{Kind: runevent.JobFinished, Job: j.ID, Conclusion: runevent.Skipped})
		return res
	}
	if e.opts.Unaffected != nil {
		if reason, skip := e.opts.Unaffected(j); skip {
			// Not run, but not a failure either: a job that depends on this one
			// sees it as succeeded, as if it had run on code that did not change.
			e.emit(runevent.Event{Kind: runevent.Notice, Job: j.ID, Text: "not run: " + reason})
			e.emit(runevent.Event{Kind: runevent.JobFinished, Job: j.ID, Conclusion: runevent.Skipped})
			res.Conclusion, res.Unchanged = runevent.Success, reason
			return res
		}
	}
	if j.Uses != "" {
		return fail("calling reusable workflows (%s) isn't supported yet", j.Uses)
	}
	ectx.Status = nil

	var rawMatrix any
	failFast, maxParallel := any(true), any(nil)
	if s := j.Strategy; s != nil {
		rawMatrix = s.Matrix
		if s.FailFast != nil {
			failFast = s.FailFast
		}
		maxParallel = s.MaxParallel
	}
	m, err := expr.EvalValue(rawMatrix, ectx)
	if err != nil {
		return fail("strategy.matrix: %v", err)
	}
	combos, err := workflow.ExpandMatrix(m)
	if err != nil {
		return fail("strategy.matrix: %v", err)
	}
	if ff, err := expr.EvalValue(failFast, ectx); err == nil {
		failFast = expr.Truthy(ff)
	}
	limit := len(combos)
	if maxParallel != nil {
		mp, err := expr.EvalValue(maxParallel, ectx)
		if err != nil {
			return fail("strategy.max-parallel: %v", err)
		}
		if n := int(expr.ToNumber(mp)); n > 0 && n < limit {
			limit = n
		}
	}

	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	jobSem := make(chan struct{}, limit)
	results := make([]instanceResult, len(combos))
	var wg sync.WaitGroup
	for i, combo := range combos {
		strategy := value.Object{
			{Key: "fail-fast", Value: failFast},
			{Key: "job-index", Value: float64(i)},
			{Key: "job-total", Value: float64(len(combos))},
			{Key: "max-parallel", Value: float64(limit)},
		}
		wg.Go(func() {
			jobSem <- struct{}{}
			defer func() { <-jobSem }()
			r := e.runInstance(jobCtx, j, combo, strategy, needs)
			results[i] = r
			if r.conclusion == runevent.Failure && !r.continueOnError && failFast == true {
				cancelJob()
			}
		})
	}
	wg.Wait()

	res.Conclusion = runevent.Success
	for _, r := range results {
		switch {
		case r.conclusion == runevent.Failure && !r.continueOnError:
			res.Conclusion = runevent.Failure
		case r.conclusion == runevent.Cancelled && res.Conclusion == runevent.Success:
			res.Conclusion = runevent.Cancelled
		}
		// For matrix jobs, the last instance to set an output wins, as on
		// GitHub, where the order isn't defined either.
		for _, f := range r.outputs {
			if f.Value != "" {
				res.Outputs = res.Outputs.Set(f.Key, f.Value)
			}
		}
	}
	return res
}

func orEmpty(o value.Object) value.Object {
	if o == nil {
		return value.Object{}
	}
	return o
}

type instanceResult struct {
	conclusion      runevent.Conclusion
	continueOnError bool
	outputs         value.Object
	summary         string // the job summary ($GITHUB_STEP_SUMMARY)
}

func (e *engine) runInstance(ctx context.Context, j *workflow.Job, combo, strategy, needs value.Object) instanceResult {
	ectx := &expr.Context{Contexts: map[string]any{
		"github":   e.opts.Github,
		"needs":    needs,
		"vars":     orEmpty(e.opts.Vars),
		"inputs":   orEmpty(e.opts.Inputs),
		"matrix":   combo,
		"strategy": strategy,
	}}
	name := instanceName(j, combo, ectx)
	res := instanceResult{conclusion: runevent.Failure}
	fail := func(format string, args ...any) instanceResult {
		e.emit(runevent.Event{Kind: runevent.Error, Job: name, Text: fmt.Sprintf(format, args...)})
		e.emit(runevent.Event{Kind: runevent.JobFinished, Job: name, Conclusion: runevent.Failure})
		return res
	}
	if v, err := expr.EvalValue(j.ContinueOnError, ectx); err == nil {
		res.continueOnError = expr.Truthy(v)
	}
	// A run that cannot happen here fails at once, without waiting for a slot
	// behind runs that can (unless fail-fast has already cancelled it).
	runsOn, err := expr.EvalValue(j.RunsOn, ectx)
	if err != nil {
		return fail("runs-on: %v", err)
	}
	cat := e.opts.Catalog
	if os := platform.FromRunsOn(cat, runsOn); os != "" && !cat.OSAvailable(os) && ctx.Err() == nil {
		return fail("%s", cat.OSUnavailable(os))
	}

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		e.emit(runevent.Event{Kind: runevent.JobFinished, Job: name, Conclusion: runevent.Cancelled})
		res.conclusion = runevent.Cancelled
		return res
	}
	if ctx.Err() != nil { // fail-fast cancelled it while it waited
		e.emit(runevent.Event{Kind: runevent.JobFinished, Job: name, Conclusion: runevent.Cancelled})
		res.conclusion = runevent.Cancelled
		return res
	}

	r, err := e.opts.Remote(ctx, RemoteInstance{Job: j, Name: name, Matrix: combo, Strategy: strategy, Needs: needs, Context: ectx, RunsOn: runsOn})
	if err != nil {
		return fail("%v", err)
	}
	res.conclusion, res.outputs, res.summary = r.Conclusion, r.Outputs, r.Summary
	return res
}

// instanceName is the job's name: its name: (which may use matrix values),
// or GitHub's default of the id followed by the matrix values.
func instanceName(j *workflow.Job, combo value.Object, ctx *expr.Context) string {
	if j.Name != "" {
		if v, err := expr.Interpolate(j.Name, ctx); err == nil {
			return expr.ToString(v)
		}
		return j.Name
	}
	if len(combo) == 0 {
		return j.ID
	}
	var vals []string
	var leaves func(v any)
	leaves = func(v any) {
		switch v := v.(type) {
		case value.Object:
			for _, f := range v {
				leaves(f.Value)
			}
		case []any:
			for _, e := range v {
				leaves(e)
			}
		default:
			vals = append(vals, expr.ToString(v))
		}
	}
	leaves(combo)
	return j.ID + " (" + strings.Join(vals, ", ") + ")"
}
