package instabox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Paused spare machines.
//
// A machine made from the runner snapshot takes 6 to 20 seconds to come up. A
// machine that was paused with its agent already running comes back in under a
// second, where it left off (InstaBox keeps its programs). So between runs a
// few machines wait, paused: Claim resumes one instead of creating a machine,
// and TopUp, at the end of a run, makes new ones so the next run finds them.
//
// A paused machine costs its disks and nothing else (a figure to check against
// the bill: Prices.Cost counts a spare's paused seconds at the storage rate).
// A spare is found by its metadata, so it outlives the process that made it. A
// used machine is deleted, not paused again: what a job left on it (secrets
// included) must not reach the next one.
const (
	metaWarm   = "raccoonci"           // "warm" while the machine waits, "in-use" once claimed
	metaSnap   = "raccoonci-snapshot"  // the snapshot it was made from
	metaPaused = "raccoonci-paused-at" // when it was paused, unix seconds
	metaClaim  = "raccoonci-claim"     // the token of the caller that is taking it
	// spareLifetime is the server-side deadline of a spare: the most the API
	// allows, so one nobody claims does not live for ever.
	spareLifetime = 24 * time.Hour
)

// Spares keeps paused machines of one provider's runner snapshot.
type Spares struct {
	P *Provider
	// Want is how many to keep paused.
	Want int

	mu    sync.Mutex
	taken map[string]bool // spares this process claimed
}

func (s *Spares) filter() map[string]string {
	return map[string]string{metaWarm: "warm", metaSnap: s.P.baseKey()}
}

// Claim resumes a paused spare and returns it, with how long it had been
// paused. ok is false when there is none (or none that would resume).
func (s *Spares) Claim(ctx context.Context) (id string, spec Spec, pausedSec float64, ok bool) {
	list, err := s.P.Client.ListMachines(ctx, s.filter())
	if err != nil {
		s.P.logf("looking for paused machines: %v", err)
		return "", Spec{}, 0, false
	}
	slices.SortFunc(list, func(a, b MachineInfo) int { return strings.Compare(a.Name, b.Name) })
	for _, m := range list {
		if m.State != "suspended" || !s.claim(ctx, m) {
			continue
		}
		if err := s.P.Client.Resume(ctx, m.ID); err != nil {
			s.P.Client.SetMetadata(ctx, m.ID, m.Metadata) // give it back
			s.release(m.ID)
			continue
		}
		if at, err := strconv.ParseInt(m.Metadata[metaPaused], 10, 64); err == nil {
			pausedSec = max(0, time.Since(time.Unix(at, 0)).Seconds())
		}
		// Marked, so a later listing does not take it for a spare.
		if err := s.P.Client.SetMetadata(ctx, m.ID, map[string]string{metaWarm: "in-use", metaSnap: s.P.baseKey()}); err != nil {
			s.P.logf("marking machine %s: %v", m.ID, err)
		}
		return m.ID, m.Spec, pausedSec, true
	}
	return "", Spec{}, 0, false
}

// claim makes m ours alone. Resuming cannot decide that (resuming a machine
// that runs, or is being resumed, succeeds too), so two jobs asking at once
// would be handed the same machine. Within this process a set decides; across
// processes each writes its own token into the machine's metadata and reads it
// back, and whoever finds another's token has lost.
func (s *Spares) claim(ctx context.Context, m MachineInfo) bool {
	s.mu.Lock()
	if s.taken == nil {
		s.taken = map[string]bool{}
	}
	if s.taken[m.ID] {
		s.mu.Unlock()
		return false
	}
	s.taken[m.ID] = true
	s.mu.Unlock()

	var b [6]byte
	rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	md := map[string]string{metaWarm: "claimed", metaSnap: s.P.baseKey(), metaPaused: m.Metadata[metaPaused], metaClaim: token}
	if err := s.P.Client.SetMetadata(ctx, m.ID, md); err == nil {
		if got, err := s.P.Client.GetMachine(ctx, m.ID); err == nil && got.Metadata[metaClaim] == token {
			return true
		}
	}
	s.release(m.ID) // someone else's, or we could not tell
	return false
}

func (s *Spares) release(id string) {
	s.mu.Lock()
	delete(s.taken, id)
	s.mu.Unlock()
}

// TopUp brings the number of paused spares to Want: machines made from an
// older snapshot are deleted, missing ones are created, wait for their agent,
// and are paused. It returns the first error; the rest still go on.
func (s *Spares) TopUp(ctx context.Context) error {
	if s.Want <= 0 {
		return nil
	}
	// Every spare, whatever snapshot it came from.
	list, err := s.P.Client.ListMachines(ctx, map[string]string{metaWarm: "warm"})
	if err != nil {
		return err
	}
	have := 0
	for _, m := range list {
		switch {
		case m.Metadata[metaSnap] != s.P.baseKey():
			s.P.deleteMachine(ctx, m.ID, nil) // made from a snapshot we no longer use
		case m.State == "suspended":
			have++
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for i := have; i < s.Want; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.make(ctx); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return first
}

// make creates one spare and pauses it once its agent runs.
func (s *Spares) make(ctx context.Context) error {
	pr := s.P
	name := fmt.Sprintf("rc-warm-%d", time.Now().UnixNano()%1e12)
	began := time.Now()
	req := CreateRequest{
		Name: name, SnapshotID: pr.Snapshot, DeleteAfterSeconds: int(spareLifetime.Seconds()),
		Metadata: map[string]string{metaWarm: "warm", metaSnap: pr.baseKey()},
	}
	if pr.Snapshot == "" {
		req.TemplateID = pr.Template
	}
	id, spec, err := pr.Client.CreateMachineSpec(ctx, req)
	use := &Usage{Machine: id, Name: name, Spec: spec, From: pr.baseKey(), Requested: began, Spare: true}
	if id != "" {
		pr.usage.add(use)
	}
	fail := func(err error) error {
		if id != "" {
			pr.deleteMachine(ctx, id, use)
		}
		return fmt.Errorf("making a spare machine: %w", err)
	}
	if err != nil {
		return fail(err)
	}
	pr.usage.set(use, func(u *Usage) { u.Ready = time.Now() })
	// A machine paused before its agent runs would wake up without one.
	if err := pr.waitAgent(ctx, id); err != nil {
		return fail(err)
	}
	if err := pr.Client.Suspend(ctx, id); err != nil {
		return fail(err)
	}
	paused := time.Now()
	pr.usage.set(use, func(u *Usage) { u.Deleted = paused }) // it stops running now; storage goes on
	md := map[string]string{metaWarm: "warm", metaSnap: pr.baseKey(), metaPaused: strconv.FormatInt(paused.Unix(), 10)}
	if err := pr.Client.SetMetadata(ctx, id, md); err != nil {
		return fail(err)
	}
	pr.logf("spare machine %s paused after %v", id, time.Since(began).Round(100*time.Millisecond))
	return nil
}

// waitAgent waits until the agent service on the machine is running (a
// machine of a template has no agent: until it answers a command).
func (p *Provider) waitAgent(ctx context.Context, id string) error {
	check := "systemctl is-active raccoonci-agent"
	want := "active"
	if p.Snapshot == "" {
		check, want = "echo ready", "ready"
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, out, err := p.Client.Exec(ctx, id, check)
		if err == nil && code == 0 && strings.TrimSpace(out) == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the agent did not start on %s (%q, %v)", id, strings.TrimSpace(out), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
