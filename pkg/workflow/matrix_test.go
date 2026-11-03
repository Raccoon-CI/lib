package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Raccoon-CI/lib/pkg/value"
)

func decodeYAML(t *testing.T, src string) any {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	v, err := value.NewDecoder(value.DefaultNodeBudget).Decode(&n)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func expandJSON(t *testing.T, src string) []string {
	t.Helper()
	combos, err := ExpandMatrix(decodeYAML(t, src))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(combos))
	for i, c := range combos {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = string(b)
	}
	return out
}

func assertCombos(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("combinations:\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
}

func TestExpandMatrixCrossProductOrder(t *testing.T) {
	got := expandJSON(t, `
version: [10, 12]
os: [ubuntu-latest, windows-latest]
`)
	assertCombos(t, got,
		`{"version":10,"os":"ubuntu-latest"}`,
		`{"version":10,"os":"windows-latest"}`,
		`{"version":12,"os":"ubuntu-latest"}`,
		`{"version":12,"os":"windows-latest"}`,
	)
}

// The "Expanding or adding matrix configurations" example from GitHub's docs.
func TestExpandMatrixIncludeDocsExample(t *testing.T) {
	got := expandJSON(t, `
fruit: [apple, pear]
animal: [cat, dog]
include:
  - color: green
  - color: pink
    animal: cat
  - fruit: apple
    shape: circle
  - fruit: banana
  - fruit: banana
    animal: cat
`)
	assertCombos(t, got,
		`{"fruit":"apple","animal":"cat","color":"pink","shape":"circle"}`,
		`{"fruit":"apple","animal":"dog","color":"green","shape":"circle"}`,
		`{"fruit":"pear","animal":"cat","color":"pink"}`,
		`{"fruit":"pear","animal":"dog","color":"green"}`,
		`{"fruit":"banana"}`,
		`{"fruit":"banana","animal":"cat"}`,
	)
}

func TestExpandMatrixIncludeOnly(t *testing.T) {
	got := expandJSON(t, `
include:
  - site: production
    datacenter: site-a
  - site: staging
    datacenter: site-b
`)
	assertCombos(t, got,
		`{"site":"production","datacenter":"site-a"}`,
		`{"site":"staging","datacenter":"site-b"}`,
	)
}

// The "Excluding matrix configurations" example from GitHub's docs: 12
// combinations, one removed by a full match and two by a partial match.
func TestExpandMatrixExcludeDocsExample(t *testing.T) {
	got := expandJSON(t, `
os: [macos-latest, windows-latest]
version: [12, 14, 16]
environment: [staging, production]
exclude:
  - os: macos-latest
    version: 12
    environment: production
  - os: windows-latest
    version: 16
`)
	if len(got) != 9 {
		t.Fatalf("got %d combinations, want 9:\n%s", len(got), strings.Join(got, "\n"))
	}
	for _, c := range got {
		if strings.Contains(c, `"os":"windows-latest","version":16`) {
			t.Errorf("excluded combination survived: %s", c)
		}
	}
}

func TestExpandMatrixNoMatrix(t *testing.T) {
	combos, err := ExpandMatrix(nil)
	if err != nil || len(combos) != 1 || len(combos[0]) != 0 {
		t.Fatalf("got %v, %v; want one empty combination", combos, err)
	}
}

func TestExpandMatrixErrors(t *testing.T) {
	tests := []struct {
		name, matrix, wantErr string
	}{
		{"empty vector", `os: []`, `"os" does not contain any values`},
		{"exclude unknown key", "os: [a]\nexclude:\n  - arch: x64", `exclude key "arch" does not match`},
		{"everything excluded", "os: [a]\nexclude:\n  - os: a", "generates no jobs"},
		{"unevaluated", `os: ${{ fromJSON(needs.setup.outputs.os) }}`, "has not been evaluated"},
		{"vector not a list", `os: ubuntu`, `"os" must be a list`},
		{"over job limit", "a: [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17]\nb: [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16]", "limit of 256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExpandMatrix(decodeYAML(t, tt.matrix))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got error %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestExpandMatrixDoesNotAliasCombinations(t *testing.T) {
	combos, err := ExpandMatrix(decodeYAML(t, "a: [1, 2]\nb: [x, y]\ninclude:\n  - b: x\n    extra: true"))
	if err != nil {
		t.Fatal(err)
	}
	combos[0][0].Value = "mutated"
	for _, c := range combos[1:] {
		if v, _ := c.Get("a"); v == "mutated" {
			t.Fatal("combinations share backing storage")
		}
	}
}
