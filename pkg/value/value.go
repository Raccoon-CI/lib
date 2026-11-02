// Package value is the dynamic data model shared by workflow parsing and
// expression evaluation. A value is one of: nil, bool, float64, string,
// []any or Object. Numbers are always float64, matching GitHub Actions
// (which is why `python-version: 3.10` famously becomes 3.1 there too).
package value

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Object is a mapping that keeps its keys in source order. Order matters for
// matrix expansion and the job names GitHub derives from matrix values.
type Object []Field

type Field struct {
	Key   string
	Value any
}

func (o Object) Get(key string) (any, bool) {
	for _, f := range o {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

// Lookup is Get with case-insensitive keys, as property access in GitHub
// expressions is (github.event and GITHUB.EVENT are the same).
func (o Object) Lookup(key string) (any, bool) {
	for _, f := range o {
		if strings.EqualFold(f.Key, key) {
			return f.Value, true
		}
	}
	return nil, false
}

// Set returns o with key set to v, replacing an existing value in place or
// appending the key. Like append, it may modify o's backing array.
func (o Object) Set(key string, v any) Object {
	for i, f := range o {
		if f.Key == key {
			o[i].Value = v
			return o
		}
	}
	return append(o, Field{key, v})
}

func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(f.Key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(f.Value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Equal reports deep equality. Object key order is ignored.
func Equal(a, b any) bool {
	switch a := a.(type) {
	case Object:
		b, ok := b.(Object)
		if !ok || len(a) != len(b) {
			return false
		}
		for _, f := range a {
			bv, ok := b.Get(f.Key)
			if !ok || !Equal(f.Value, bv) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !Equal(a[i], b[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// ContainsExpr reports whether any string in v contains a ${{ }} expression.
func ContainsExpr(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, "${{")
	case Object:
		for _, f := range v {
			if ContainsExpr(f.Value) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if ContainsExpr(e) {
				return true
			}
		}
	}
	return false
}

// DefaultNodeBudget bounds how many YAML nodes one Decoder will produce.
// Workflow files can come from untrusted pull requests, and walking a
// yaml.Node tree ourselves bypasses yaml.v3's own alias-expansion limits, so
// without a cap a "billion laughs" alias bomb would exhaust memory.
const DefaultNodeBudget = 1_000_000

// Decoder converts yaml.Node trees into values. Its node budget is shared
// across calls, so use one Decoder per document.
type Decoder struct {
	budget int
}

func NewDecoder(budget int) *Decoder {
	return &Decoder{budget: budget}
}

func (d *Decoder) Decode(n *yaml.Node) (any, error) {
	if d.budget--; d.budget < 0 {
		return nil, fmt.Errorf("line %d: document too large (alias expansion limit reached)", n.Line)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return d.Decode(n.Content[0])
	case yaml.AliasNode:
		return d.Decode(n.Alias)
	case yaml.MappingNode:
		obj := make(Object, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: mapping keys must be scalars", k.Line)
			}
			if _, dup := obj.Get(k.Value); dup {
				return nil, fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			v, err := d.Decode(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			obj = append(obj, Field{k.Value, v})
		}
		return obj, nil
	case yaml.SequenceNode:
		arr := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := d.Decode(c)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	case yaml.ScalarNode:
		return scalar(n)
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

func scalar(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, err
		}
		return b, nil
	case "!!int", "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, err
		}
		return f, nil
	default:
		return n.Value, nil
	}
}
