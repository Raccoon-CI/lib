package value

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func decode(t *testing.T, src string) any {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	v, err := NewDecoder(DefaultNodeBudget).Decode(&n)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDecodeScalars(t *testing.T) {
	obj := decode(t, `
int: 18
hex: 0x1F
float: 3.10
yes: true
str: "18"
null_: ~
on: push
`).(Object)
	want := map[string]any{
		"int":   18.0,
		"hex":   31.0,
		"float": 3.1,
		"yes":   true,
		"str":   "18",
		"null_": nil,
		"on":    "push",
	}
	for k, w := range want {
		if got, ok := obj.Get(k); !ok || got != w {
			t.Errorf("%s = %#v, want %#v", k, got, w)
		}
	}
}

func TestDecodeKeepsKeyOrder(t *testing.T) {
	b, err := json.Marshal(decode(t, "z: 1\na: [x, {c: 2, b: 3}]\nm: null"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":1,"a":["x",{"c":2,"b":3}],"m":null}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestDecodeRejectsDuplicateKeys(t *testing.T) {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("a: 1\na: 2"), &n); err != nil {
		t.Skipf("yaml.v3 already rejects duplicate keys: %v", err)
	}
	if _, err := NewDecoder(DefaultNodeBudget).Decode(&n); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

func TestEqual(t *testing.T) {
	a := decode(t, "{x: 1, y: [a, {z: true}]}")
	b := decode(t, "{y: [a, {z: true}], x: 1.0}")
	if !Equal(a, b) {
		t.Error("objects differing only in key order should be equal")
	}
	if Equal(a, decode(t, "{x: 1, y: [a, {z: false}]}")) {
		t.Error("objects with different nested values should differ")
	}
	if Equal("1", 1.0) {
		t.Error("string and number should differ")
	}
}
