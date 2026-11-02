package instabox

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/dispatch"
)

// Provider runs jobs on InstaBox machines, one machine per job.
type Provider struct {
	Client     *Client
	Controller *dispatch.Controller
	// ControllerURL is where machines reach Controller.Handler (https; a
	// machine has no route to this process otherwise).
	ControllerURL string
	// Snapshot is the runner snapshot machines are created from.
	Snapshot string
	// Template, used when Snapshot is empty, is the template machines are
	// created from (InstaBox's "ci": GitHub's Ubuntu image with an unconfigured
	// Actions runner). There is no agent on such a machine.
	Template string
	// ConfigPath is where the agent finds its config. Default
	// /data/home/.raccoonci-agent.json: the home of the runner snapshot's user
	// (the file API writes as that user, and /etc is not writable by it).
	ConfigPath string
	// StartAgent starts the agent through the exec endpoint, for snapshots
	// without a service that does it at boot. It costs one request of the
	// account's 32 concurrent ones, for a moment.
	StartAgent bool
	// Lifetime is the server-side deadline after which the machine is deleted
	// whatever happens to this process. Default 2 hours (the API allows 1 minute
	// to 24 hours).
	Lifetime time.Duration
	// Logf receives progress lines. May be nil.
	Logf func(format string, args ...any)

	// Spares, if set, supplies machines from the paused ones.
	Spares *Spares

	// Limit, if set, makes starts wait for the organization's machines-a-minute
	// limit and try again after a 429. Without it a 429 is an error.
	Limit *StartLimit

	bg    sync.WaitGroup
	usage usageLog
}

func (p *Provider) configPath() string {
	if p.ConfigPath != "" {
		return p.ConfigPath
	}
	return "/data/home/.raccoonci-agent.json"
}

// StartupTime is the median time machines of this provider took from the
// create request to ready, or zero when none has come up yet.
func (p *Provider) StartupTime() time.Duration {
	var ds []time.Duration
	for _, u := range p.usage.snapshot() {
		if !u.Ready.IsZero() {
			ds = append(ds, u.Ready.Sub(u.Requested))
		}
	}
	if len(ds) == 0 {
		return 0
	}
	slices.Sort(ds)
	return ds[len(ds)/2]
}

func (p *Provider) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// RunOptions are the per-run choices of Provider.Run.
type RunOptions struct {
	// From is the snapshot to create the machine from; default Provider.Snapshot.
	From string
	// Keep is asked when the job has ended, with how. If it returns a name,
	// the machine is cleaned (the agent's config and the job's workspaces go,
	// the caches stay) and saved as a snapshot of that name before it is
	// deleted. That happens in the background; Provider.Wait waits for it.
	Keep func(dispatch.Result) (snapshotName string)
	// Kept hears how saving went: the snapshot's id, or the error. On an error
	// the id may still be set (a snapshot that was made but did not become
	// ready), and the caller should delete it.
	Kept func(snapshotID string, err error)
}

// Wait blocks until the background work of finished runs (saving snapshots,
// deleting machines) is done. Call it before the process exits.
func (p *Provider) Wait() { p.bg.Wait() }

// Run creates a machine, has its agent run job, and deletes the machine
// again. name should be unique among the account's machines
// (lowercase letters, digits, dashes).
func (p *Provider) Run(ctx context.Context, name string, job dispatch.Job, opts *RunOptions) (dispatch.Result, error) {
	if opts == nil {
		opts = &RunOptions{}
	}
	from := opts.From
	if from == "" {
		from = p.Snapshot
	}
	life := p.Lifetime
	if life <= 0 {
		life = 2 * time.Hour
	}
	cfgPath := p.configPath()
	run, err := p.Controller.Submit(job)
	if err != nil {
		return dispatch.Result{}, err
	}
	defer p.Controller.Forget(run)

	started := time.Now()
	id, use, err := p.obtain(ctx, name, from, life, Size{})
	handedOff := false // the background task deletes the machine
	if id != "" {
		// Deleted even when the run was cancelled, so not with ctx.
		defer func() {
			if !handedOff {
				p.deleteMachine(ctx, id, use)
			}
		}()
	}
	if err != nil {
		return dispatch.Result{}, fmt.Errorf("creating the machine: %w", err)
	}
	p.logf("machine %s up in %v", id, time.Since(started).Round(100*time.Millisecond))

	cfg, _ := json.Marshal(map[string]string{"url": p.ControllerURL, "token": run.Token})
	if err := p.Client.PutFile(ctx, id, cfgPath, "0600", cfg); err != nil {
		return dispatch.Result{}, fmt.Errorf("sending the agent its config: %w", err)
	}
	if p.StartAgent {
		cmd := fmt.Sprintf("setsid nohup raccoonci agent -config %s >/tmp/raccoonci-agent.log 2>&1 </dev/null &", cfgPath)
		if code, out, err := p.Client.Exec(ctx, id, cmd); err != nil || code != 0 {
			return dispatch.Result{}, fmt.Errorf("starting the agent: exit %d %q: %v", code, out, err)
		}
	}
	res, err := run.Wait(ctx)
	if err == nil && opts.Keep != nil {
		if snapName := opts.Keep(res); snapName != "" {
			handedOff = true
			p.bg.Add(1)
			go func() {
				defer p.bg.Done()
				sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
				defer cancel()
				snapID, serr := p.save(sctx, id, snapName, cfgPath)
				if serr == nil {
					p.usage.set(use, func(u *Usage) { u.Saved = snapID })
				}
				if opts.Kept != nil {
					opts.Kept(snapID, serr)
				}
				p.deleteMachine(ctx, id, use)
			}()
		}
	}
	return res, err
}

// obtain gets a machine started from snapshot from: a paused spare resumed
// when there is one (the runner snapshot only), else a new one. The returned id
// is set once the API has assigned one, even on failure, so the caller can
// delete a half-made machine.
func (p *Provider) obtain(ctx context.Context, name, from string, life time.Duration, size Size) (string, *Usage, error) {
	started := time.Now()
	if p.Spares != nil && from == p.Snapshot && size.VCPUs == 0 { // a machine of the base's own size, not of an environment snapshot
		if id, spec, paused, ok := p.Spares.Claim(ctx); ok {
			use := &Usage{Machine: id, Name: name, Spec: spec, From: from, Requested: started, Ready: time.Now(), Resumed: true, PausedSec: paused}
			p.usage.add(use)
			p.logf("machine %s resumed in %v", id, time.Since(started).Round(100*time.Millisecond))
			return id, use, nil
		}
	}
	req := CreateRequest{Name: name, SnapshotID: from, DeleteAfterSeconds: int(life.Seconds()), VCPUs: size.VCPUs, MemMiB: size.MemMiB}
	if from == "" {
		req.TemplateID = p.Template
	}
	var id string
	var spec Spec
	var err error
	for refused := 0; ; refused++ {
		waited, werr := p.Limit.Wait(ctx)
		if werr != nil {
			return "", nil, fmt.Errorf("waiting for a free machine start: %w", werr)
		}
		if waited > time.Second {
			p.logf("%s waited %v for the machine start limit", name, waited.Round(100*time.Millisecond))
		}
		id, spec, err = p.Client.CreateMachineSpec(ctx, req)
		// A 429 that keeps coming is not the minute's limit: give up after ~7 minutes.
		if p.Limit == nil || id != "" || !IsRateLimit(err) || refused >= 20 {
			break
		}
		// InstaBox refused: every start waits, then this one is asked again.
		p.Limit.Refused()
		p.logf("%s: InstaBox refused the start (429); trying again shortly", name)
	}
	use := &Usage{Machine: id, Name: name, Spec: spec, From: cmpOr(from, "template:"+p.Template), Requested: started}
	if err == nil {
		use.Ready = time.Now()
	}
	if id != "" {
		p.usage.add(use)
	}
	return id, use, err
}

// Acquire gets a machine of the provider's base (a paused spare if one waits,
// else a new one) for a caller that runs its own thing on it. The caller must
// hand it back with Release.
func (p *Provider) Acquire(ctx context.Context, name string) (string, *Usage, error) {
	return p.AcquireSize(ctx, name, Size{})
}

// AcquireSize is Acquire for a machine of a given size (zero: the default
// size, which a paused spare can supply).
func (p *Provider) AcquireSize(ctx context.Context, name string, size Size) (string, *Usage, error) {
	life := p.Lifetime
	if life <= 0 {
		life = 2 * time.Hour
	}
	return p.obtain(ctx, name, p.Snapshot, life, size)
}

// Release deletes a machine from Acquire (even if ctx is cancelled).
func (p *Provider) Release(ctx context.Context, id string, use *Usage) { p.deleteMachine(ctx, id, use) }

// baseKey names what machines of this provider start from, for labelling
// spares: the snapshot, or "template:<id>".
func (p *Provider) baseKey() string { return cmpOr(p.Snapshot, "template:"+p.Template) }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (p *Provider) deleteMachine(ctx context.Context, id string, use *Usage) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	err := p.Client.DeleteMachine(dctx, id)
	if err != nil {
		p.logf("could not delete machine %s (the server deletes it at the deadline): %v", id, err)
		return // its end is unknown, so it keeps counting until the summary
	}
	if use != nil {
		p.usage.set(use, func(u *Usage) { u.Deleted = time.Now() })
	}
}

// Usage lists the machines this provider made, with when each was requested,
// became ready and was deleted. Raw facts, not money: the prices change, so
// cost is worked out from these at the time it is asked for (Prices.Cost).
func (p *Provider) Usage() []Usage { return p.usage.snapshot() }

// save cleans the machine of what belongs to this job and snapshots it. The
// agent's config holds the run's token; the job's workspaces are the source
// and build output of one commit. What stays is the machine's caches (module
// and package caches, the tool cache), which is the point.
func (p *Provider) save(ctx context.Context, id, name, cfgPath string) (string, error) {
	clean := fmt.Sprintf(`rm -f %q; rm -rf "${XDG_CACHE_HOME:-$HOME/.cache}/raccoonci/runs"; sync`, cfgPath)
	if code, out, err := p.Client.Exec(ctx, id, clean); err != nil || code != 0 {
		return "", fmt.Errorf("cleaning the machine before the snapshot: exit %d %q: %v", code, out, err)
	}
	return p.Client.Snapshot(ctx, id, name, "Raccoon CI environment: dependencies of one lockfile state")
}
