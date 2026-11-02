package value

import (
	"encoding/json"
	"testing"
)

func TestJSONRoundTripKeepsOrderAndTypes(t *testing.T) {
	in := Object{
		{"z", float64(1)},
		{"a", Object{{"y", true}, {"b", []any{"s", nil, float64(2.5), Object{{"k", "v"}}}}}},
		{"m", ""},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := FromJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(out)
	if string(b) != string(b2) {
		t.Errorf("order or content changed:\n%s\n%s", b, b2)
	}
	if !Equal(in, out) {
		t.Errorf("not equal: %#v", out)
	}
	if _, ok := out.(Object); !ok {
		t.Errorf("object decoded as %T", out)
	}
}

func TestFromJSONRejectsGarbage(t *testing.T) {
	for _, s := range []string{`{"a":1} x`, `{"a"`, `[1,`, ``} {
		if _, err := FromJSON([]byte(s)); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
