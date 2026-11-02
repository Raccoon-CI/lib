// Package dispatch sends a job to a machine that runs it, over a connection
// the machine opens itself.
//
// The controller serves a small HTTPS API; an agent on the machine (agent.go)
// dials it, fetches its assignment and the source tree, runs `raccoonci run`,
// and streams the log back. Nothing is pushed to the machine: it needs no
// public address, the provider's remote-exec endpoint (rate limited on
// InstaBox) is not used for control, and a machine that stops calling is
// noticed by the controller (Lease).
//
// Every request carries the bearer token of one assignment. Its hash is the
// key of the assignment, so a token opens that job and nothing else. The
// controller's address may be public, so there is no unauthenticated route.
package dispatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Job is what one machine is asked to do.
type Job struct {
	// Command is the raccoonci subcommand the machine runs; default "run".
	Command string `json:"command,omitempty"`
	// Args follow it on the machine (for "run": flags, then the workflow path
	// relative to the source tree).
	Args []string `json:"args"`
	// Input is fed to the command's standard input.
	Input []byte `json:"input,omitempty"`
	// Env is passed as is; Secrets are environment variables the machine hands
	// to the run as secrets.NAME. Both reach the machine over the connection,
	// so the controller must be served over https.
	Env     map[string]string `json:"env,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`

	// Source writes the source tree as a .tar.gz whose entries sit under one
	// top-level directory. Not sent to the machine as part of the assignment.
	Source func(w io.Writer) error `json:"-"`
	// Log receives the job's output as it arrives. May be nil.
	Log io.Writer `json:"-"`
}

// Result is how a job ended on the machine.
type Result struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"` // the agent could not run it at all
}

// Controller hands jobs to agents.
type Controller struct {
	// Lease is how long a machine may stay silent after its first request.
	// Heartbeats come every few seconds. Zero means 30 seconds.
	Lease time.Duration
	// StartTimeout is how long a machine may take to make its first request.
	// Zero means 2 minutes.
	StartTimeout time.Duration

	version int // the protocol it speaks; ProtocolVersion when zero (tests set another)

	mu   sync.Mutex
	runs map[[sha256.Size]byte]*Run
}

// Run is one submitted job.
type Run struct {
	// Token is what the machine's agent presents. Hand it to the machine over
	// a private channel (the provider's file upload) and nowhere else.
	Token string

	job          Job
	done         chan struct{}
	lease, start time.Duration

	mu       sync.Mutex
	seen     time.Time
	received int64 // log bytes written so far
	res      Result
	finished bool
}

// Submit registers a job and returns its Run. Call Run.Wait to get the result.
func (c *Controller) Submit(job Job) (*Run, error) {
	if job.Source == nil {
		return nil, errors.New("dispatch: a job needs a source")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	r := &Run{Token: base64.RawURLEncoding.EncodeToString(raw[:]), job: job, done: make(chan struct{}),
		lease: c.Lease, start: c.StartTimeout}
	if r.lease <= 0 {
		r.lease = 30 * time.Second
	}
	if r.start <= 0 {
		r.start = 2 * time.Minute
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runs == nil {
		c.runs = map[[sha256.Size]byte]*Run{}
	}
	c.runs[sha256.Sum256([]byte(r.Token))] = r
	return r, nil
}

// Forget drops a run's token. Call it when the run is over.
func (c *Controller) Forget(r *Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.runs, sha256.Sum256([]byte(r.Token)))
}

// Wait blocks until the agent reports the job done, the machine goes silent,
// or ctx ends.
func (r *Run) Wait(ctx context.Context) (Result, error) {
	lease, start := r.lease, r.start
	began := time.Now()
	tick := time.NewTicker(min(lease, start) / 4)
	defer tick.Stop()
	for {
		select {
		case <-r.done:
			return r.res, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case now := <-tick.C:
			r.mu.Lock()
			seen := r.seen
			r.mu.Unlock()
			switch {
			case seen.IsZero() && now.Sub(began) > start:
				return Result{}, fmt.Errorf("the machine did not contact the controller within %v", start)
			case !seen.IsZero() && now.Sub(seen) > lease:
				return Result{}, fmt.Errorf("the machine has been silent for %v", now.Sub(seen).Round(time.Second))
			}
		}
	}
}

func (c *Controller) lookup(r *http.Request) *Run {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[sha256.Sum256([]byte(tok))]
}

func (c *Controller) protocol() int {
	if c.version > 0 {
		return c.version
	}
	return ProtocolVersion
}

// acceptAgent checks that the agent's protocol is one this controller serves.
// An agent that is too new (or malformed) is refused and its run is ended on
// the spot with the reason, which reaches whoever waits for it: letting it go on
// would mean it works from a picture of the protocol this controller does not
// share, and waiting for it to time out would hide the cause.
func (c *Controller) acceptAgent(run *Run, w http.ResponseWriter, req *http.Request) bool {
	v, err := agentVersion(req)
	if err == nil && v > c.protocol() {
		err = fmt.Errorf("the agent on the machine speaks protocol %d; this controller speaks up to %d: the controller is older than the machine's snapshot and must be upgraded", v, c.protocol())
	} else if err == nil && v < MinProtocolVersion {
		err = fmt.Errorf("the agent on the machine speaks protocol %d; this controller no longer serves it (the oldest is %d): rebuild the machine's snapshot", v, MinProtocolVersion)
	}
	if err == nil {
		return true
	}
	run.mu.Lock()
	run.seen = time.Now()
	if !run.finished {
		run.finished, run.res = true, Result{ExitCode: -1, Error: err.Error()}
		close(run.done)
	}
	run.mu.Unlock()
	http.Error(w, err.Error(), http.StatusBadRequest)
	return false
}

// Handler serves the agent API under /v1/.
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h func(*Run, http.ResponseWriter, *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set(ProtocolHeader, strconv.Itoa(c.protocol()))
			run := c.lookup(req)
			if run == nil {
				http.Error(w, "unknown token", http.StatusUnauthorized)
				return
			}
			if !c.acceptAgent(run, w, req) {
				return
			}
			run.mu.Lock()
			finished := run.finished
			if !finished {
				run.seen = time.Now()
			}
			run.mu.Unlock()
			if finished {
				http.Error(w, "the job is over", http.StatusGone)
				return
			}
			h(run, w, req)
		})
	}
	route("GET /v1/job", func(run *Run, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(run.job)
	})
	route("GET /v1/source", func(run *Run, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		// A failure mid-stream shows on the agent as a truncated archive.
		run.job.Source(w)
	})
	route("POST /v1/heartbeat", func(_ *Run, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	route("POST /v1/log", appendLog)
	route("POST /v1/done", func(run *Run, w http.ResponseWriter, req *http.Request) {
		var res Result
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&res); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		run.mu.Lock()
		if !run.finished {
			run.finished, run.res = true, res
			close(run.done)
		}
		run.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// log appends a chunk at X-Offset. A resent chunk (the reply was lost) is
// accepted and only its new tail written; a gap is refused with the offset the
// controller expects.
func appendLog(run *Run, w http.ResponseWriter, req *http.Request) {
	off, err := strconv.ParseInt(req.Header.Get("X-Offset"), 10, 64)
	if err != nil || off < 0 {
		http.Error(w, "bad X-Offset", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 4<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if off > run.received {
		w.Header().Set("X-Expected-Offset", strconv.FormatInt(run.received, 10))
		http.Error(w, "gap in the log", http.StatusConflict)
		return
	}
	if skip := run.received - off; skip < int64(len(body)) {
		fresh := body[skip:]
		if run.job.Log != nil {
			run.job.Log.Write(fresh)
		}
		run.received += int64(len(fresh))
	}
	w.WriteHeader(http.StatusNoContent)
}
