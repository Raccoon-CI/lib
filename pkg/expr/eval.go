package expr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unsafe"

	"github.com/Raccoon-CI/lib/pkg/value"
)

// Context is what an expression can see.
type Context struct {
	// Contexts maps lower-case context names (github, env, matrix, needs, ...)
	// to their values. Referencing a name that is absent is an error, which is
	// also how callers detect expressions that can't be evaluated yet.
	Contexts map[string]any

	// Status backs success(), failure(), cancelled() and always(). It is nil
	// outside if: conditions, where those functions are not allowed.
	Status *Status

	// Funcs adds functions that need the runner, such as hashFiles. Keys are
	// lower case.
	Funcs map[string]Func
}

// Status is the state of the work before the condition being evaluated: the
// previous steps of a job, or the jobs a job needs.
type Status struct {
	Failed    bool
	Cancelled bool
	// Skipped is set for a job when a job it depends on was skipped: then
	// neither success() nor failure() holds, so by default it is skipped too.
	Skipped bool
}

type Func func(args []any) (any, error)

// UnknownContextError reports a reference to a context the Context doesn't
// provide. Callers evaluating early (e.g. at plan time, with only matrix
// known) use errors.As to tell "not known yet" apart from real mistakes.
type UnknownContextError struct {
	Name string
}

func (e *UnknownContextError) Error() string {
	return fmt.Sprintf("unrecognized named-value: %q", e.Name)
}

// filtered is the result of the * object filter. Property and index access on
// it apply to every element, so labels.*.name collects each label's name.
type filtered []any

func eval(n node, ctx *Context) (any, error) {
	switch n := n.(type) {
	case literal:
		return n.v, nil
	case variable:
		v, ok := ctx.Contexts[n.name]
		if !ok {
			return nil, &UnknownContextError{Name: n.name}
		}
		return v, nil
	case property:
		recv, err := eval(n.recv, ctx)
		if err != nil {
			return nil, err
		}
		return access(recv, n.name), nil
	case index:
		recv, err := eval(n.recv, ctx)
		if err != nil {
			return nil, err
		}
		idx, err := eval(n.idx, ctx)
		if err != nil {
			return nil, err
		}
		return access(recv, idx), nil
	case filter:
		recv, err := eval(n.recv, ctx)
		if err != nil {
			return nil, err
		}
		return applyFilter(recv), nil
	case not:
		x, err := eval(n.x, ctx)
		if err != nil {
			return nil, err
		}
		return !Truthy(x), nil
	case binary:
		l, err := eval(n.l, ctx)
		if err != nil {
			return nil, err
		}
		switch n.op { // && and || short-circuit and return an operand
		case tokAnd:
			if !Truthy(l) {
				return l, nil
			}
			return eval(n.r, ctx)
		case tokOr:
			if Truthy(l) {
				return l, nil
			}
			return eval(n.r, ctx)
		}
		r, err := eval(n.r, ctx)
		if err != nil {
			return nil, err
		}
		switch n.op {
		case tokEq:
			return looseEqual(l, r), nil
		case tokNe:
			return !looseEqual(l, r), nil
		}
		c, ok := compare(l, r)
		if !ok {
			return false, nil
		}
		switch n.op {
		case tokLt:
			return c < 0, nil
		case tokLe:
			return c <= 0, nil
		case tokGt:
			return c > 0, nil
		default:
			return c >= 0, nil
		}
	case call:
		return callFunc(n, ctx)
	}
	return nil, fmt.Errorf("internal error: unknown node %T", n)
}

// access returns recv[key], or nil when there is no such element.
func access(recv, key any) any {
	switch recv := recv.(type) {
	case filtered:
		var out filtered
		for _, e := range recv {
			v := access(e, key)
			if f, ok := v.(filtered); ok {
				out = append(out, f...)
			} else if v != nil {
				out = append(out, v)
			}
		}
		return out
	case value.Object:
		v, _ := recv.Lookup(ToString(key))
		return v
	case []any:
		f := ToNumber(key)
		if f != math.Trunc(f) || f < 0 || f >= float64(len(recv)) {
			return nil
		}
		return recv[int(f)]
	}
	return nil
}

func applyFilter(recv any) filtered {
	switch recv := recv.(type) {
	case filtered:
		var out filtered
		for _, e := range recv {
			out = append(out, applyFilter(e)...)
		}
		return out
	case []any:
		return append(filtered{}, recv...)
	case value.Object:
		out := make(filtered, 0, len(recv))
		for _, f := range recv {
			out = append(out, f.Value)
		}
		return out
	}
	return filtered{}
}

// Truthy reports whether v counts as true: everything except false, 0, -0,
// NaN, "" and null.
func Truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0 && !math.IsNaN(v)
	case string:
		return v != ""
	}
	return true
}

// ToNumber coerces v the way GitHub does before comparing mismatched types.
func ToNumber(v any) float64 {
	switch v := v.(type) {
	case nil:
		return 0
	case bool:
		if v {
			return 1
		}
		return 0
	case float64:
		return v
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0
		}
		if f, err := parseNumberLiteral(s); err == nil {
			return f
		}
	}
	return math.NaN()
}

// ToString converts v for string interpolation and string functions.
func ToString(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return formatNumber(v)
	case string:
		return v
	case value.Object:
		return "Object"
	case []any, filtered:
		return "Array"
	}
	return fmt.Sprint(v)
}

func formatNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0" // also for -0
	case math.Abs(f) < 1e21:
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'e', -1, 64)
}

type kind int

const (
	kindNull kind = iota
	kindBool
	kindNumber
	kindString
	kindArray
	kindObject
)

func kindOf(v any) kind {
	switch v.(type) {
	case nil:
		return kindNull
	case bool:
		return kindBool
	case float64:
		return kindNumber
	case string:
		return kindString
	case value.Object:
		return kindObject
	}
	return kindArray
}

// looseEqual is ==: mismatched types are compared as numbers, strings ignore
// case, and arrays/objects are equal only to themselves.
func looseEqual(a, b any) bool {
	ka, kb := kindOf(a), kindOf(b)
	if ka != kb {
		return ToNumber(a) == ToNumber(b)
	}
	switch ka {
	case kindNull:
		return true
	case kindBool:
		return a.(bool) == b.(bool)
	case kindNumber:
		return a.(float64) == b.(float64)
	case kindString:
		return strings.EqualFold(a.(string), b.(string))
	}
	return sameInstance(a, b)
}

// sameInstance compares backing arrays. Empty values have no backing array of
// their own, so they are never considered the same instance.
func sameInstance(a, b any) bool {
	switch a := a.(type) {
	case value.Object:
		b := b.(value.Object)
		return len(a) > 0 && len(a) == len(b) && unsafe.SliceData(a) == unsafe.SliceData(b)
	case []any:
		b, ok := b.([]any)
		return ok && len(a) > 0 && len(a) == len(b) && unsafe.SliceData(a) == unsafe.SliceData(b)
	case filtered:
		b, ok := b.(filtered)
		return ok && len(a) > 0 && len(a) == len(b) && unsafe.SliceData(a) == unsafe.SliceData(b)
	}
	return false
}

// compare orders a and b for < <= > >=. ok is false when they are not
// comparable (NaN, arrays, objects), making every such comparison false.
func compare(a, b any) (c int, ok bool) {
	ka, kb := kindOf(a), kindOf(b)
	if ka == kindString && kb == kindString {
		return strings.Compare(strings.ToUpper(a.(string)), strings.ToUpper(b.(string))), true
	}
	if ka == kindArray || ka == kindObject || kb == kindArray || kb == kindObject {
		return 0, false
	}
	x, y := ToNumber(a), ToNumber(b)
	switch {
	case math.IsNaN(x) || math.IsNaN(y):
		return 0, false
	case x < y:
		return -1, true
	case x > y:
		return 1, true
	}
	return 0, true
}

// export turns internal filtered arrays into plain arrays, recursively, so
// results never leak the filtered type.
func export(v any) any {
	switch v := v.(type) {
	case filtered:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = export(e)
		}
		return out
	}
	return v
}

// Eval evaluates a bare expression, written without ${{ }}.
func Eval(src string, ctx *Context) (any, error) {
	n, _, err := parse(src, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	v, err := eval(n, ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	return export(v), nil
}

// Interpolate evaluates the ${{ }} expressions in s. If s is exactly one
// expression, the result keeps its type, so `${{ fromJSON(x) }}` can produce a
// matrix and `${{ matrix.experimental }}` a bool. Otherwise each result is
// converted to a string and spliced into the surrounding text.
func Interpolate(s string, ctx *Context) (any, error) {
	var b strings.Builder
	rest := s
	for first := true; ; first = false {
		i := strings.Index(rest, "${{")
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		b.WriteString(rest[:i])
		src := rest[i+3:]
		n, end, err := parse(src, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s, err)
		}
		v, err := eval(n, ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", strings.TrimSpace(src[:end-2]), err)
		}
		rest = src[end:]
		if first && i == 0 && rest == "" {
			return export(v), nil
		}
		b.WriteString(ToString(v))
	}
}

// InterpolateLoose is Interpolate for static analysis: each expression that
// can't be evaluated yet (or at all) becomes placeholder instead of failing
// the whole string.
func InterpolateLoose(s string, ctx *Context, placeholder string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i])
		src := rest[i+3:]
		n, end, err := parse(src, true)
		if err != nil { // unterminated: keep the text as is
			b.WriteString(rest[i:])
			return b.String()
		}
		if v, err := eval(n, ctx); err == nil {
			b.WriteString(ToString(v))
		} else {
			b.WriteString(placeholder)
		}
		rest = src[end:]
	}
}

// EvalValue interpolates every string inside v (see Interpolate). Object keys
// are left alone.
func EvalValue(v any, ctx *Context) (any, error) {
	switch v := v.(type) {
	case string:
		return Interpolate(v, ctx)
	case value.Object:
		out := make(value.Object, len(v))
		for i, f := range v {
			ev, err := EvalValue(f.Value, ctx)
			if err != nil {
				return nil, err
			}
			out[i] = value.Field{Key: f.Key, Value: ev}
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			ev, err := EvalValue(e, ctx)
			if err != nil {
				return nil, err
			}
			out[i] = ev
		}
		return out, nil
	}
	return v, nil
}

// EvalCondition evaluates an if: condition. The ${{ }} wrapper is optional,
// an empty condition means success(), and a condition that calls none of the
// status functions gets an implicit `success() &&` in front, as on GitHub.
func EvalCondition(cond string, ctx *Context) (bool, error) {
	src := strings.TrimSpace(cond)
	if src == "" {
		src = "success()"
	}
	if strings.Contains(src, "${{") {
		inner, ok := singleExpression(src)
		if !ok {
			// Mixed text and expressions: GitHub evaluates this as a string,
			// which is true whenever it is non-empty.
			v, err := Interpolate(src, ctx)
			if err != nil {
				return false, err
			}
			return Truthy(v), nil
		}
		src = inner
	}
	n, _, err := parse(src, false)
	if err != nil {
		return false, fmt.Errorf("%s: %w", src, err)
	}
	if !callsStatusFunc(n) {
		n = binary{tokAnd, call{name: "success"}, n}
	}
	v, err := eval(n, ctx)
	if err != nil {
		return false, fmt.Errorf("%s: %w", src, err)
	}
	return Truthy(v), nil
}

// singleExpression returns the inside of s if s is exactly one ${{ }}.
func singleExpression(s string) (string, bool) {
	if !strings.HasPrefix(s, "${{") {
		return "", false
	}
	src := s[3:]
	if _, end, err := parse(src, true); err == nil && end == len(src) {
		return src[:end-2], true
	}
	return "", false
}

func callsStatusFunc(n node) bool {
	switch n := n.(type) {
	case call:
		if isStatusFunc(n.name) {
			return true
		}
		for _, a := range n.args {
			if callsStatusFunc(a) {
				return true
			}
		}
	case property:
		return callsStatusFunc(n.recv)
	case index:
		return callsStatusFunc(n.recv) || callsStatusFunc(n.idx)
	case filter:
		return callsStatusFunc(n.recv)
	case not:
		return callsStatusFunc(n.x)
	case binary:
		return callsStatusFunc(n.l) || callsStatusFunc(n.r)
	}
	return false
}
