// Package runevent is what a runner says about a job: the events it emits while
// it runs (a line of output, a step starting, an annotation...), how steps and
// jobs conclude, and the result of a job. It holds only data. It does not know
// how a job is run, so the protocol between a controller and a machine (package
// wire), the server's log and anything else that only reads what a runner said
// can use it without bringing in the code that runs steps.
//
// package runner re-exports every name here (type aliases and constants), so
// code that already says runner.Event keeps working.
package runevent

import (
	"time"

	"github.com/Raccoon-CI/lib/pkg/value"
)

// Conclusion is how a step or a job ended.
type Conclusion string

const (
	Success   Conclusion = "success"
	Failure   Conclusion = "failure"
	Cancelled Conclusion = "cancelled"
	Skipped   Conclusion = "skipped"
)

// EventKind says what an Event is.
type EventKind int

const (
	JobStarted EventKind = iota
	StepStarted
	Output // a line of step output, with secrets masked
	Debug
	Notice
	Warning
	Error
	GroupStart
	GroupEnd
	StepFinished // Conclusion is set; Skipped steps get no StepStarted
	JobFinished
)

// Event is one thing a job did or said.
type Event struct {
	Kind  EventKind
	Time  time.Time
	Job   string
	Step  string // display name; "" for job-level events
	Depth int    // 1 for steps inside a composite action, and so on
	Text  string
	// Props are an annotation's file, line, col, endLine, endColumn, title.
	Props      map[string]string
	Conclusion Conclusion
}

// Result is how a job ended.
type Result struct {
	Conclusion Conclusion
	Outputs    value.Object
	// Summary is the job summary written to $GITHUB_STEP_SUMMARY.
	Summary string
}
