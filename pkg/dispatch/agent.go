package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/archive"
)

// maxSource caps what the agent unpacks from the controller.
const maxSource = 8 << 30

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SlotConfig is the config file of slot i of a machine that runs several jobs
// at once: slot 0 uses base, the others base with ".<i>" appended.
func SlotConfig(base string, i int) string {
	if i == 0 {
		return base
	}
	return base + "." + strconv.Itoa(i)
}

// Agent is the machine's side: it fetches its one assignment, runs it with
// `raccoonci run`, and reports back.
type Agent struct {
	URL   string // the controller; https, or http on a loopback address
	Token string
	// Exe is the raccoonci binary that runs the job. Default: this one.
	Exe string
	// Dir is where the source is unpacked (as Dir/src). It must exist.
	Dir string

	Client    *http.Client
	Heartbeat time.Duration // default 5 seconds

	version int // the protocol it speaks; ProtocolVersion when zero (tests set another)
}

func (a *Agent) protocol() int {
	if a.version > 0 {
		return a.version
	}
	return ProtocolVersion
}

// Run does the whole assignment. An error means the controller could not be
// reached or the assignment was unusable; a failing job is a Result with a
// non-zero ExitCode, delivered to the controller, and Run returns nil.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.check(); err != nil {
		return err
	}
	if a.Client == nil {
		a.Client = &http.Client{Timeout: 5 * time.Minute}
	}
	if a.Heartbeat <= 0 {
		a.Heartbeat = 5 * time.Second
	}

	var job Job
	if err := a.get(ctx, "/v1/job", func(r io.Reader) error { return json.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&job) }); err != nil {
		return fmt.Errorf("fetching the assignment: %w", err)
	}
	src := filepath.Join(a.Dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return a.fail(ctx, err)
	}
	err := a.get(ctx, "/v1/source", func(r io.Reader) error { return archive.ExtractTarGz(r, src, maxSource) })
	if err != nil {
		return a.fail(ctx, fmt.Errorf("fetching the source: %w", err))
	}

	command := job.Command
	if command == "" {
		command = "run"
	}
	if command != "run" && command != "run-instance" { // the controller cannot pick other subcommands
		return a.fail(ctx, fmt.Errorf("command %q", command))
	}
	args := []string{command}
	env := os.Environ()
	for k, v := range job.Env {
		if !envName.MatchString(k) {
			return a.fail(ctx, fmt.Errorf("env name %q", k))
		}
		env = append(env, k+"="+v)
	}
	for k, v := range job.Secrets {
		if !envName.MatchString(k) {
			return a.fail(ctx, fmt.Errorf("secret name %q", k))
		}
		env = append(env, k+"="+v)
		args = append(args, "-secret", k)
	}
	args = append(args, job.Args...)

	exe := a.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return a.fail(ctx, err)
		}
	}
	ship := a.startShipper(ctx)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = src, env, ship, ship
	if len(job.Input) > 0 {
		cmd.Stdin = bytes.NewReader(job.Input)
	}
	res := Result{}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res = Result{ExitCode: -1, Error: err.Error()}
		}
	}
	ship.stop()
	return a.done(ctx, res)
}

func (a *Agent) check() error {
	u, err := url.Parse(a.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("controller URL %q", a.URL)
	}
	if u.Scheme == "https" {
		return nil
	}
	// The token and the job's secrets travel in these requests.
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("controller URL %q: must be https", a.URL)
}

func (a *Agent) request(ctx context.Context, method, path string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set(ProtocolHeader, strconv.Itoa(a.protocol()))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return a.Client.Do(req)
}

// get fetches path and hands the body to read, retrying connection failures
// for a minute (the machine may be up before the network is).
func (a *Agent) get(ctx context.Context, path string, read func(io.Reader) error) error {
	var last error
	for try := 0; try < 6; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(try) * 2 * time.Second):
			}
		}
		resp, err := a.request(ctx, "GET", path, nil, nil)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("controller: %s", resp.Status)
		}
		err = read(resp.Body)
		resp.Body.Close()
		return err
	}
	return last
}

// fail reports that the assignment could not be run, and returns the cause.
func (a *Agent) fail(ctx context.Context, cause error) error {
	a.done(ctx, Result{ExitCode: -1, Error: cause.Error()})
	return cause
}

func (a *Agent) done(ctx context.Context, res Result) error {
	body, _ := json.Marshal(res)
	var last error
	for try := 0; try < 5; try++ {
		if try > 0 {
			time.Sleep(time.Duration(try) * time.Second)
		}
		resp, err := a.request(ctx, "POST", "/v1/done", bytes.NewReader(body), nil)
		if err != nil {
			last = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusGone {
			return nil
		}
		last = fmt.Errorf("controller: %s", resp.Status)
	}
	return fmt.Errorf("reporting the result: %w", last)
}

// shipper sends the job's output to the controller as it is written, and
// doubles as the heartbeat when there is none.
type shipper struct {
	a    *Agent
	ctx  context.Context
	mu   sync.Mutex
	buf  []byte // not yet acknowledged
	sent int64  // acknowledged bytes: the offset buf starts at
	quit chan struct{}
	end  sync.WaitGroup
}

func (a *Agent) startShipper(ctx context.Context) *shipper {
	s := &shipper{a: a, ctx: ctx, quit: make(chan struct{})}
	s.end.Add(1)
	go func() {
		defer s.end.Done()
		tick := time.NewTicker(min(a.Heartbeat, 500*time.Millisecond))
		defer tick.Stop()
		last := time.Now()
		for {
			select {
			case <-s.quit:
				return
			case <-tick.C:
				if s.flush() {
					last = time.Now() // a log request counts as a heartbeat
				} else if time.Since(last) >= a.Heartbeat {
					if resp, err := a.request(ctx, "POST", "/v1/heartbeat", nil, nil); err == nil {
						resp.Body.Close()
					}
					last = time.Now()
				}
			}
		}
	}()
	return s
}

func (s *shipper) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	s.mu.Unlock()
	return len(p), nil
}

func (s *shipper) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buf)
}

// flush sends what is buffered; it reports whether it sent anything.
func (s *shipper) flush() bool {
	s.mu.Lock()
	chunk := append([]byte(nil), s.buf[:min(len(s.buf), 1<<20)]...)
	off := s.sent
	s.mu.Unlock()
	if len(chunk) == 0 {
		return false
	}
	resp, err := s.a.request(s.ctx, "POST", "/v1/log", bytes.NewReader(chunk),
		map[string]string{"X-Offset": strconv.FormatInt(off, 10)})
	if err != nil {
		return false // retried on the next tick
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return false
	}
	s.mu.Lock()
	s.buf = s.buf[len(chunk):]
	s.sent += int64(len(chunk))
	s.mu.Unlock()
	return true
}

// stop sends what is left (retrying for up to 30 seconds) and ends the loop.
func (s *shipper) stop() {
	close(s.quit)
	s.end.Wait()
	deadline := time.Now().Add(30 * time.Second)
	for s.pending() > 0 && time.Now().Before(deadline) && s.ctx.Err() == nil {
		if !s.flush() {
			time.Sleep(500 * time.Millisecond)
		}
	}
}
