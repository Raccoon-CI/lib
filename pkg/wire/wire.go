// Package wire is what a controller and the machine it sends a job instance to
// say to each other besides the dispatch protocol itself: the instance to run
// (InstanceSpec), and the lines the machine prints back (Line, written with an
// Encoder and read with a Reader). `raccoonci run-instance` on the machine and
// every controller (`raccoonci run -on instabox`, the server) use it, so it is
// the contract between them.
package wire

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/runevent"
	"github.com/Raccoon-CI/raccoonci/pkg/value"
)

// Version is the version of the assignment (InstanceSpec) and of the lines a
// machine prints back. The same rules as for the dispatch protocol apply (see
// dispatch.ProtocolVersion): adding an optional field keeps the version, which
// is how Images and Summary were added; changing what a field means, or
// removing one, raises it. A controller always sends its version; one that
// predates the field sends none, which is version 1. A machine refuses an
// assignment newer than it understands, and says so.
const Version = 1

// InstanceSpec is what a controller sends a machine to run one job instance
// (see engine.RemoteInstance): the instance as the engine resolved it, plus
// the run's contexts. Secrets travel separately, in the environment.
type InstanceSpec struct {
	// Protocol is the Version the controller speaks; 0 (absent) means 1.
	Protocol int    `json:"protocol,omitempty"`
	Workflow string `json:"workflow"` // path in the source tree
	Job      string `json:"job"`
	// Contexts is an object with github, matrix, strategy, needs, inputs and
	// vars. JSON object order is kept (value.FromJSON).
	Contexts   json.RawMessage   `json:"contexts"`
	ServiceEnv map[string]string `json:"service_env"`
	Install    string            `json:"install"`
	// Images is the runner-images snapshot (runnerimages.Snapshot.JSON) the
	// controller plans with, so that the machine does too. Empty: the one built
	// into the machine's binary. An older machine ignores the field.
	Images json.RawMessage `json:"images,omitempty"`
}

// Line is one line of the machine's output: an event, the final result, or
// (anything that is not JSON) plain text, which the controller shows as it is.
type Line struct {
	Event  *runevent.Event `json:"event,omitempty"`
	Result *Result         `json:"result,omitempty"`
}

// Result is the last line: how the instance ended.
type Result struct {
	Conclusion runevent.Conclusion `json:"conclusion"`
	Outputs    json.RawMessage     `json:"outputs"`
	// DurationMS is how long the job took on the machine, from provisioning
	// to its last step: what the scheduler weighs against a machine's start-up.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Summary is the job summary the steps wrote to $GITHUB_STEP_SUMMARY, with
	// secrets masked (the runner masks it as it collects it).
	Summary string `json:"summary,omitempty"`
}

// Encoder writes one JSON line at a time, safe for concurrent use (the events
// of parallel steps come from several goroutines).
type Encoder struct {
	mu  sync.Mutex
	out io.Writer
}

// NewEncoder writes to out.
func NewEncoder(out io.Writer) *Encoder { return &Encoder{out: out} }

// Write prints one line.
func (e *Encoder) Write(v Line) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.out.Write(append(b, '\n'))
}

// Reader turns the controller's copy of a machine's output into events and a
// result. Writes may split lines anywhere.
type Reader struct {
	Name  string // the instance, for lines that are plain text
	Event func(runevent.Event)

	mu      sync.Mutex
	partial []byte
	result  *Result
}

// Write implements io.Writer.
func (r *Reader) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partial = append(r.partial, p...)
	for {
		i := bytes.IndexByte(r.partial, '\n')
		if i < 0 {
			break
		}
		r.line(r.partial[:i])
		r.partial = r.partial[i+1:]
	}
	return len(p), nil
}

// Flush handles a last line that has no newline.
func (r *Reader) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.partial) > 0 {
		r.line(r.partial)
		r.partial = nil
	}
}

func (r *Reader) line(b []byte) {
	var l Line
	if len(b) > 0 && b[0] == '{' && json.Unmarshal(b, &l) == nil && (l.Event != nil || l.Result != nil) {
		if l.Event != nil {
			r.Event(*l.Event)
		}
		if l.Result != nil {
			r.result = l.Result
		}
		return
	}
	if len(b) > 0 {
		r.Event(runevent.Event{Kind: runevent.Output, Time: time.Now(), Job: r.Name, Text: string(b)})
	}
}

// Duration is how long the job took on the machine, if it said.
func (r *Reader) Duration() (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.result == nil || r.result.DurationMS <= 0 {
		return 0, false
	}
	return time.Duration(r.result.DurationMS) * time.Millisecond, true
}

// Result is the instance's final result, if the machine sent one.
func (r *Reader) Result() (runevent.Result, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.result == nil {
		return runevent.Result{}, false, nil
	}
	out := runevent.Result{Conclusion: r.result.Conclusion, Summary: r.result.Summary}
	if len(r.result.Outputs) > 0 {
		v, err := value.FromJSON(r.result.Outputs)
		if err != nil {
			return out, true, err
		}
		out.Outputs, _ = v.(value.Object)
	}
	return out, true, nil
}
