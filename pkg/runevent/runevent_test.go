package runevent

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This package is data and nothing else: it may import the standard library
// and package value. If it grew to need more, it would drag that into every
// package that only wants to read what a runner said.
func TestRuneventImportsOnlyDataPackages(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(path, "github.com/Raccoon-CI/lib/") && path != "github.com/Raccoon-CI/lib/pkg/value" {
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
}

func TestTheNamesAreTheWireFormatsAndCannotChangeByAccident(t *testing.T) {
	// Conclusions travel as these strings, and the kinds as these numbers, in
	// the events of machines already running an older binary.
	for c, want := range map[Conclusion]string{Success: "success", Failure: "failure", Cancelled: "cancelled", Skipped: "skipped"} {
		if string(c) != want {
			t.Errorf("conclusion %q, want %q", c, want)
		}
	}
	for k, want := range map[EventKind]int{JobStarted: 0, StepStarted: 1, Output: 2, Debug: 3, Notice: 4, Warning: 5, Error: 6, GroupStart: 7, GroupEnd: 8, StepFinished: 9, JobFinished: 10} {
		if int(k) != want {
			t.Errorf("event kind %d, want %d", k, want)
		}
	}
}
