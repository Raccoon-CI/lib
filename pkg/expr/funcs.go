package expr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Raccoon-CI/raccoonci/pkg/value"
)

func isStatusFunc(name string) bool {
	switch name {
	case "success", "failure", "cancelled", "always":
		return true
	}
	return false
}

func callFunc(c call, ctx *Context) (any, error) {
	args := make([]any, len(c.args))
	for i, a := range c.args {
		v, err := eval(a, ctx)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	arity := func(min, max int) error {
		if len(args) < min || len(args) > max {
			if min == max {
				return fmt.Errorf("%s() takes %d argument(s), got %d", c.name, min, len(args))
			}
			return fmt.Errorf("%s() takes %d to %d arguments, got %d", c.name, min, max, len(args))
		}
		return nil
	}

	if isStatusFunc(c.name) {
		if err := arity(0, 0); err != nil {
			return nil, err
		}
		s := ctx.Status
		if s == nil {
			return nil, fmt.Errorf("%s() is only available in if: conditions", c.name)
		}
		switch c.name {
		case "success":
			return !s.Failed && !s.Cancelled && !s.Skipped, nil
		case "failure":
			return s.Failed, nil
		case "cancelled":
			return s.Cancelled, nil
		}
		return true, nil // always
	}

	switch c.name {
	case "contains":
		if err := arity(2, 2); err != nil {
			return nil, err
		}
		switch search := args[0].(type) {
		case []any:
			return containsElem(search, args[1]), nil
		case filtered:
			return containsElem(search, args[1]), nil
		}
		return strings.Contains(strings.ToLower(ToString(args[0])), strings.ToLower(ToString(args[1]))), nil
	case "startswith":
		if err := arity(2, 2); err != nil {
			return nil, err
		}
		return strings.HasPrefix(strings.ToLower(ToString(args[0])), strings.ToLower(ToString(args[1]))), nil
	case "endswith":
		if err := arity(2, 2); err != nil {
			return nil, err
		}
		return strings.HasSuffix(strings.ToLower(ToString(args[0])), strings.ToLower(ToString(args[1]))), nil
	case "format":
		if len(args) < 1 {
			return nil, fmt.Errorf("format() needs a format string")
		}
		return format(ToString(args[0]), args[1:])
	case "join":
		if err := arity(1, 2); err != nil {
			return nil, err
		}
		sep := ","
		if len(args) == 2 {
			sep = ToString(args[1])
		}
		var elems []any
		switch a := args[0].(type) {
		case []any:
			elems = a
		case filtered:
			elems = a
		default:
			return ToString(a), nil
		}
		parts := make([]string, len(elems))
		for i, e := range elems {
			parts[i] = ToString(e)
		}
		return strings.Join(parts, sep), nil
	case "tojson":
		if err := arity(1, 1); err != nil {
			return nil, err
		}
		var b strings.Builder
		writeJSON(&b, export(args[0]), "")
		return b.String(), nil
	case "fromjson":
		if err := arity(1, 1); err != nil {
			return nil, err
		}
		return decodeJSON(ToString(args[0]))
	}

	if f, ok := ctx.Funcs[c.name]; ok {
		return f(args)
	}
	return nil, fmt.Errorf("unknown function %s()", c.name)
}

func containsElem(arr []any, item any) bool {
	for _, e := range arr {
		if looseEqual(e, item) {
			return true
		}
	}
	return false
}

// format replaces {0}, {1}, ... with arguments; {{ and }} are literal braces.
func format(f string, args []any) (string, error) {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		switch c := f[i]; {
		case c == '{' && i+1 < len(f) && f[i+1] == '{':
			b.WriteByte('{')
			i++
		case c == '}' && i+1 < len(f) && f[i+1] == '}':
			b.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(f[i:], '}')
			if end < 2 {
				return "", fmt.Errorf("format(): invalid format string %q", f)
			}
			n := 0
			for _, d := range f[i+1 : i+end] {
				if d < '0' || d > '9' {
					return "", fmt.Errorf("format(): invalid format string %q", f)
				}
				n = n*10 + int(d-'0')
				if n > len(args) {
					break
				}
			}
			if n >= len(args) {
				return "", fmt.Errorf("format(): %q references argument {%d}, but only %d given", f, n, len(args))
			}
			b.WriteString(ToString(args[n]))
			i += end
		case c == '}':
			return "", fmt.Errorf("format(): unmatched '}' in %q", f)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// writeJSON writes v pretty-printed with two-space indentation, like
// GitHub's toJSON, keeping object key order and not escaping HTML.
func writeJSON(b *strings.Builder, v any, indent string) {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool, float64:
		b.WriteString(ToString(v))
	case string:
		writeJSONString(b, v)
	case value.Object:
		if len(v) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, f := range v {
			b.WriteString(indent + "  ")
			writeJSONString(b, f.Key)
			b.WriteString(": ")
			writeJSON(b, f.Value, indent+"  ")
			if i < len(v)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range v {
			b.WriteString(indent + "  ")
			writeJSON(b, e, indent+"  ")
			if i < len(v)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	default:
		writeJSONString(b, ToString(v))
	}
}

func writeJSONString(b *strings.Builder, s string) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // cannot fail for a string
	b.WriteString(strings.TrimSuffix(buf.String(), "\n"))
}

// maxJSONDepth bounds fromJSON nesting; its input can come from step
// outputs written by untrusted code.
const maxJSONDepth = 1000

// decodeJSON parses JSON into values, keeping object key order.
func decodeJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	v, err := decodeJSONValue(dec, 0)
	if err != nil {
		return nil, fmt.Errorf("fromJSON(): %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("fromJSON(): unexpected data after the JSON value")
	}
	return v, nil
}

func decodeJSONValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, fmt.Errorf("JSON nested too deeply")
	}
	t, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("empty input")
		}
		return nil, err
	}
	switch t := t.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := value.Object{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := decodeJSONValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj = obj.Set(kt.(string), v)
			}
			_, err := dec.Token() // }
			return obj, err
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeJSONValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token() // ]
			return arr, err
		}
		return nil, fmt.Errorf("unexpected %q", t)
	}
	return t, nil // string, float64, bool or nil
}
