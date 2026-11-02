package instabox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/dispatch"
)

// agentSlots is how many jobs the agent on a pooled machine can take at once
// (`raccoonci agent -loop -slots 8`, in the runner snapshot's unit). The pool
// limits how many it gives a machine; this is only the range of slot numbers.
const agentSlots = 8

// Pool runs several jobs, one after another and side by side, on the same
// machines. Provider.Run gives every job a machine; for jobs that take a few
// seconds the machine's start-up is most of the cost, so the pool puts them on
// a machine that is up or starting, and makes a new one only when all are
// full. A machine stays Linger after its last job so that jobs which become
// ready just then (those that needed it) can use it, and is deleted by Close
// at the latest.
type Pool struct {
	P *Provider
	// Slots is how many jobs a machine takes at once. Zero means one per vCPU
	// of the machine (two until its size is known).
	Slots int
	// Linger is how long an empty machine is kept. Default 15 seconds.
	Linger time.Duration
	// Prefix starts the machines' names.
	Prefix string

	mu       sync.Mutex
	machines []*pooled
	seq      int
	closed   bool
	// vcpus is the size of the machines seen so far (they all start from one
	// snapshot), the guess for one whose size is not known yet.
	vcpus int
}

type pooled struct {
	id    string
	use   *Usage
	ready chan struct{} // closed when id/err are set
	err   error
	vcpus int // 0 until known
	taken [agentSlots]bool
	used  int
	dead  bool
	timer *time.Timer
}

func (p *Pool) linger() time.Duration {
	if p.Linger > 0 {
		return p.Linger
	}
	return 15 * time.Second
}

// capacity is how many jobs m may hold now. Called with p.mu held.
func (p *Pool) capacity(m *pooled) int {
	c := p.Slots
	if c <= 0 {
		c = 2
		switch {
		case m.vcpus > 0:
			c = m.vcpus
		case p.vcpus > 0:
			c = p.vcpus
		}
	}
	return max(1, min(c, agentSlots))
}

// acquire reserves a slot on a machine with room, starting a machine when
// there is none. The caller waits for m.ready before using it.
func (p *Pool) acquire(ctx context.Context) (*pooled, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, 0, errors.New("the machine pool is closed")
	}
	var m *pooled
	for _, c := range p.machines {
		if !c.dead && c.used < p.capacity(c) && (m == nil || c.used > m.used) {
			m = c // fill machines up before using an emptier one
		}
	}
	if m == nil {
		p.seq++
		m = &pooled{ready: make(chan struct{})}
		p.machines = append(p.machines, m)
		name := fmt.Sprintf("%s-m%d", p.Prefix, p.seq)
		go p.start(context.WithoutCancel(ctx), m, name)
	}
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	slot := slices.Index(m.taken[:], false)
	m.taken[slot] = true
	m.used++
	return m, slot, nil
}

// start creates the machine behind m and, for a snapshot that does not start
// the agent itself, starts it.
func (p *Pool) start(ctx context.Context, m *pooled, name string) {
	pr := p.P
	life := pr.Lifetime
	if life <= 0 {
		life = 2 * time.Hour
	}
	began := time.Now()
	id, use, err := pr.obtain(ctx, name, pr.Snapshot, life, Size{})
	spec := use.Spec
	if err == nil && pr.StartAgent {
		cmd := fmt.Sprintf("setsid nohup raccoonci agent -loop -slots %d -wait 24h -config %s >/tmp/raccoonci-agent.log 2>&1 </dev/null &", agentSlots, pr.configPath())
		if code, out, xerr := pr.Client.Exec(ctx, id, cmd); xerr != nil || code != 0 {
			err = fmt.Errorf("starting the agent: exit %d %q: %v", code, out, xerr)
		}
	}
	p.mu.Lock()
	m.id, m.use, m.vcpus = id, use, spec.VCPUs
	if spec.VCPUs > 0 {
		p.vcpus = spec.VCPUs
	}
	if err != nil {
		m.err = fmt.Errorf("creating the machine: %w", err)
		m.dead = true
	} else {
		pr.logf("machine %s up in %v", id, time.Since(began).Round(100*time.Millisecond))
	}
	p.mu.Unlock()
	close(m.ready)
	if err != nil {
		p.discard(m)
	}
}

// Run puts job on a machine of the pool and waits for the agent's result.
func (p *Pool) Run(ctx context.Context, job dispatch.Job) (dispatch.Result, error) {
	m, slot, err := p.acquire(ctx)
	if err != nil {
		return dispatch.Result{}, err
	}
	defer p.release(m, slot)
	select {
	case <-m.ready:
	case <-ctx.Done():
		return dispatch.Result{}, ctx.Err()
	}
	if m.err != nil {
		return dispatch.Result{}, m.err
	}
	pr := p.P
	run, err := pr.Controller.Submit(job)
	if err != nil {
		return dispatch.Result{}, err
	}
	defer pr.Controller.Forget(run)
	cfg, _ := json.Marshal(map[string]string{"url": pr.ControllerURL, "token": run.Token})
	path := dispatch.SlotConfig(pr.configPath(), slot)
	if err := pr.Client.PutFile(ctx, m.id, path, "0600", cfg); err != nil {
		p.drop(m) // we cannot reach it: do not hand it more
		return dispatch.Result{}, fmt.Errorf("sending the agent its config: %w", err)
	}
	res, err := run.Wait(ctx)
	if err != nil {
		p.drop(m) // silent or unreachable: its slots may never come back
	}
	return res, err
}

// release gives the slot back. An empty machine that may take no more jobs
// is deleted; one that may waits Linger for more work.
func (p *Pool) release(m *pooled, slot int) {
	p.mu.Lock()
	m.taken[slot] = false
	m.used--
	switch {
	case m.used > 0:
		p.mu.Unlock()
	case m.dead || p.closed:
		m.dead = true
		p.mu.Unlock()
		p.discard(m)
	default:
		m.timer = time.AfterFunc(p.linger(), func() {
			p.mu.Lock()
			if m.used > 0 || m.dead {
				p.mu.Unlock()
				return
			}
			m.dead = true
			p.mu.Unlock()
			p.discard(m)
		})
		p.mu.Unlock()
	}
}

// drop stops giving jobs to m; it is deleted when its last job is released.
func (p *Pool) drop(m *pooled) {
	p.mu.Lock()
	m.dead = true
	p.mu.Unlock()
}

// discard deletes the machine of m, once. It waits for the machine to exist,
// so a job cancelled during creation does not leak it.
func (p *Pool) discard(m *pooled) {
	<-m.ready
	p.mu.Lock()
	id, use := m.id, m.use
	m.id = ""
	p.mu.Unlock()
	if id == "" {
		return
	}
	p.P.bg.Add(1)
	go func() {
		defer p.P.bg.Done()
		p.P.deleteMachine(context.Background(), id, use)
	}()
}

// Close deletes every machine that runs no job now, and each other one when
// its last job is released. Provider.Wait waits for the deletes.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	var idle []*pooled
	for _, m := range p.machines {
		if m.timer != nil {
			m.timer.Stop()
			m.timer = nil
		}
		m.dead = true
		if m.used == 0 {
			idle = append(idle, m)
		}
	}
	p.mu.Unlock()
	for _, m := range idle {
		p.discard(m)
	}
}

// Machines is how many machines the pool has made.
func (p *Pool) Machines() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seq
}
