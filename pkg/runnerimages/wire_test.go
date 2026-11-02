package runnerimages

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestASnapshotSurvivesTheRoundTripToAMachine(t *testing.T) {
	s, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 100<<10 {
		t.Errorf("a snapshot is %d bytes; it travels with every job", len(b))
	}
	got, err := ParseSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Aliases, s.Aliases) || !reflect.DeepEqual(got.Images, s.Images) {
		t.Fatal("the snapshot changed on the way")
	}
	// What a plan asks for is the same on both sides.
	if a, b := s.ForLabel(nil, "ubuntu-latest"), got.ForLabel(nil, "ubuntu-latest"); a == nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("ubuntu-latest: %+v vs %+v", a, b)
	}
}

func TestAMachineRefusesASnapshotThatCannotBeReal(t *testing.T) {
	good := `{"aliases":{"ubuntu-latest":"ubuntu24"},"images":{"ubuntu24":{"image":"ubuntu24","version":"1.1","tools":{"node":"20.1.0"}}}}`
	if s, err := ParseSnapshot([]byte(good)); err != nil || s.ForLabel(nil, "ubuntu-latest").Tools["node"] != "20.1.0" {
		t.Fatalf("a good one: %v", err)
	}
	for name, bad := range map[string]string{
		"not JSON":              `{`,
		"no images":             `{"aliases":{},"images":{}}`,
		"null":                  `null`,
		"an image under a name": `{"images":{"ubuntu24":{"image":"ubuntu22"}}}`,
		"a null image":          `{"images":{"ubuntu24":null}}`,
		"too big":               `{"images":{"a":{"image":"a","packages":["` + strings.Repeat("x", 2<<20) + `"]}}}`,
	} {
		if _, err := ParseSnapshot([]byte(bad)); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	// An alias to an image the snapshot lacks is dropped, not trusted.
	s, err := ParseSnapshot([]byte(`{"aliases":{"ubuntu-latest":"ubuntu99"},"images":{"ubuntu24":{"image":"ubuntu24"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Aliases["ubuntu-latest"]; ok {
		t.Errorf("a dangling alias was kept: %v", s.Aliases)
	}
	var tooMany = map[string]any{}
	for i := 0; i < 65; i++ {
		name := "img" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		tooMany[name] = map[string]string{"image": name}
	}
	b, _ := json.Marshal(map[string]any{"images": tooMany})
	if _, err := ParseSnapshot(b); err == nil {
		t.Error("accepted 65 images")
	}
}
