package wire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/runevent"
)

func TestEventsAndResultSurviveTheRoundTripWhereverTheWritesSplit(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	enc.Write(Line{Event: &runevent.Event{Kind: runevent.Output, Job: "build", Step: "Run x", Text: "héllo"}})
	enc.Write(Line{Event: &runevent.Event{Kind: runevent.StepFinished, Job: "build", Conclusion: runevent.Failure}})
	enc.Write(Line{Event: &runevent.Event{Kind: runevent.Error, Job: "build", Text: "boom", Props: map[string]string{"file": "a.go", "line": "3"}}})
	enc.Write(Line{Result: &Result{Conclusion: runevent.Success, Outputs: json.RawMessage(`{"z":"1","a":"2"}`), DurationMS: 1500, Summary: "## Résumé\n- ok\n"}})
	wire := buf.Bytes()

	for _, size := range []int{1, 3, 7, len(wire)} {
		var events []runevent.Event
		r := &Reader{Name: "build", Event: func(e runevent.Event) { events = append(events, e) }}
		for i := 0; i < len(wire); i += size {
			r.Write(wire[i:min(i+size, len(wire))])
		}
		r.Flush()
		if len(events) != 3 || events[2].Props["file"] != "a.go" || events[2].Props["line"] != "3" || events[0].Text != "héllo" || events[1].Conclusion != runevent.Failure {
			t.Fatalf("size %d: events = %+v", size, events)
		}
		res, ok, err := r.Result()
		if !ok || err != nil || res.Conclusion != runevent.Success || res.Summary != "## Résumé\n- ok\n" {
			t.Fatalf("size %d: result = %+v %v %v", size, res, ok, err)
		}
		if keys := res.Outputs; len(keys) != 2 || keys[0].Key != "z" || keys[1].Key != "a" {
			t.Fatalf("size %d: outputs lost their order: %v", size, keys)
		}
		if d, ok := r.Duration(); !ok || d != 1500*time.Millisecond {
			t.Fatalf("size %d: duration = %v %v", size, d, ok)
		}
	}
}

func TestPlainTextBecomesAnOutputLineOfTheInstance(t *testing.T) {
	var events []runevent.Event
	r := &Reader{Name: "test (2)", Event: func(e runevent.Event) { events = append(events, e) }}
	r.Write([]byte("panic: oops\n{not json}\n\n"))
	r.Write([]byte("no newline at the end"))
	r.Flush()
	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	for _, e := range events {
		if e.Kind != runevent.Output || e.Job != "test (2)" {
			t.Errorf("event = %+v", e)
		}
	}
	if events[1].Text != "{not json}" || !strings.Contains(events[2].Text, "no newline") {
		t.Fatalf("events = %+v", events)
	}
	if _, ok, _ := r.Result(); ok {
		t.Fatal("a result appeared from plain text")
	}
}
