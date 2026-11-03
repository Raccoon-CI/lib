package workflow

import (
	"fmt"
	"slices"

	"github.com/Raccoon-CI/lib/pkg/value"
)

// MaxMatrixJobs is GitHub's limit on jobs one matrix may generate per run.
const MaxMatrixJobs = 256

// maxMatrixProduct guards against matrices whose cross product would exhaust
// memory before excludes get a chance to trim it. This limit is ours, not GitHub's.
const maxMatrixProduct = 1 << 16

// ExpandMatrix returns the combinations a strategy.matrix generates, following
// GitHub's rules:
//
//   - Rows multiply in definition order; the first key varies slowest.
//   - Each exclude entry removes every combination it partially matches.
//   - Each include entry, in order, is merged into every combination whose
//     original (row) values it doesn't contradict. Values that earlier includes
//     added may be overwritten. An include that merges into nothing becomes a
//     combination of its own, and later includes never merge into it.
//
// m must be fully evaluated (no ${{ }} left). A nil m means the job has no
// matrix, which yields a single empty combination.
func ExpandMatrix(m any) ([]value.Object, error) {
	if m == nil {
		return []value.Object{{}}, nil
	}
	obj, ok := m.(value.Object)
	if !ok {
		return nil, fmt.Errorf("matrix must be a mapping%s", unevaluatedHint(m))
	}

	var rows value.Object
	var include, exclude []value.Object
	for _, f := range obj {
		switch f.Key {
		case "include", "exclude":
			list, err := combinations(f.Key, f.Value)
			if err != nil {
				return nil, err
			}
			if f.Key == "include" {
				include = list
			} else {
				exclude = list
			}
		default:
			vals, ok := f.Value.([]any)
			if !ok {
				return nil, fmt.Errorf("matrix vector %q must be a list%s", f.Key, unevaluatedHint(f.Value))
			}
			if len(vals) == 0 {
				return nil, fmt.Errorf("matrix vector %q does not contain any values", f.Key)
			}
			rows = append(rows, f)
		}
	}

	combos, err := product(rows)
	if err != nil {
		return nil, err
	}

	for _, ex := range exclude {
		for _, f := range ex {
			if _, ok := rows.Get(f.Key); !ok {
				return nil, fmt.Errorf("matrix exclude key %q does not match any key within the matrix", f.Key)
			}
		}
		combos = slices.DeleteFunc(combos, func(c value.Object) bool { return matches(c, ex) })
	}

	var extra []value.Object
	for _, inc := range include {
		merged := false
		for i, c := range combos {
			if compatible(c, inc, rows) {
				combos[i] = merge(c, inc, rows)
				merged = true
			}
		}
		if !merged {
			extra = append(extra, slices.Clone(inc))
		}
	}
	combos = append(combos, extra...)

	switch {
	case len(combos) == 0:
		return nil, fmt.Errorf("matrix generates no jobs")
	case len(combos) > MaxMatrixJobs:
		return nil, fmt.Errorf("matrix generates %d jobs, more than the limit of %d", len(combos), MaxMatrixJobs)
	}
	return combos, nil
}

func combinations(key string, v any) ([]value.Object, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("matrix %s must be a list of mappings%s", key, unevaluatedHint(v))
	}
	out := make([]value.Object, 0, len(list))
	for _, e := range list {
		obj, ok := e.(value.Object)
		if !ok {
			return nil, fmt.Errorf("matrix %s entries must be mappings%s", key, unevaluatedHint(e))
		}
		out = append(out, obj)
	}
	return out, nil
}

// product returns the cross product of rows. With no rows it returns no
// combinations (not one empty one), so an include-only matrix yields exactly
// its include entries.
func product(rows value.Object) ([]value.Object, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	size := 1
	for _, r := range rows {
		if size *= len(r.Value.([]any)); size > maxMatrixProduct {
			return nil, fmt.Errorf("matrix has more than %d combinations", maxMatrixProduct)
		}
	}
	combos := []value.Object{{}}
	for _, r := range rows {
		next := make([]value.Object, 0, len(combos)*len(r.Value.([]any)))
		for _, c := range combos {
			for _, v := range r.Value.([]any) {
				next = append(next, append(slices.Clone(c), value.Field{Key: r.Key, Value: v}))
			}
		}
		combos = next
	}
	return combos, nil
}

// matches reports whether every key/value of partial appears in c.
func matches(c, partial value.Object) bool {
	for _, f := range partial {
		v, ok := c.Get(f.Key)
		if !ok || !value.Equal(v, f.Value) {
			return false
		}
	}
	return true
}

// compatible reports whether inc can merge into c without changing any of c's
// original row values.
func compatible(c, inc, rows value.Object) bool {
	for _, f := range inc {
		if _, isRow := rows.Get(f.Key); !isRow {
			continue
		}
		if v, ok := c.Get(f.Key); ok && !value.Equal(v, f.Value) {
			return false
		}
	}
	return true
}

func merge(c, inc, rows value.Object) value.Object {
	out := slices.Clone(c)
	for _, f := range inc {
		if _, isRow := rows.Get(f.Key); !isRow {
			out = out.Set(f.Key, f.Value)
		}
	}
	return out
}

func unevaluatedHint(v any) string {
	if value.ContainsExpr(v) {
		return " (it contains a ${{ }} expression that has not been evaluated)"
	}
	return ""
}
