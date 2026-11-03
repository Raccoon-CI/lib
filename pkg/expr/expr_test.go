package expr

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/Raccoon-CI/lib/pkg/value"
)

func fromJSONObject(t *testing.T, s string) value.Object {
	t.Helper()
	v, err := decodeJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	return v.(value.Object)
}

func testContext(t *testing.T) *Context {
	github := fromJSONObject(t, `{
		"event_name": "push",
		"ref": "refs/heads/main",
		"event": {"issue": {"labels": [{"name": "bug"}, {"name": "help wanted"}, {"id": 3}]}}
	}`)
	return &Context{Contexts: map[string]any{
		"github": github,
		"matrix": fromJSONObject(t, `{"os": "ubuntu-latest", "node": 18, "experimental": false}`),
		"vars":   fromJSONObject(t, `{"fruits": [{"name": "apple", "quantity": 1}, {"name": "orange", "quantity": 2}]}`),
		"env":    value.Object{},
	}}
}

func toJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEval(t *testing.T) {
	tests := []struct {
		expr string
		want any // compared as JSON
	}{
		// Literals.
		{`null`, nil},
		{`'It''s open source!'`, "It's open source!"},
		{`0x9C`, 156.0},
		{`-9.2`, -9.2},
		{`-2.99e-2`, -0.0299},
		{`true`, true},

		// Loose equality: mismatched types compare as numbers, strings ignore case.
		{`1 == '1'`, true},
		{`'' == 0`, true},
		{`null == 0`, true},
		{`true == 1`, true},
		{`'abc' == 'ABC'`, true},
		{`'abc' == 1`, false},
		{`'abc' != 1`, true},
		{`'a' < 'B'`, true},
		{`2 > '10'`, false},
		{`'x' < 1`, false},
		{`github == github`, true},
		{`fromJSON('{}') == fromJSON('{}')`, false},

		// && and || return operands.
		{`null || 'default'`, "default"},
		{`'a' && 'b'`, "b"},
		{`0 && 'b'`, 0.0},
		{`!''`, true},
		{`!fromJSON('[]')`, false},
		{`1 == 1 && 2 == 3 || 'fallback'`, "fallback"},

		// Property access is case-insensitive; missing properties are null.
		{`github.EVENT_NAME`, "push"},
		{`github['event_name']`, "push"},
		{`github.nope.deeper`, nil},
		{`matrix.node`, 18.0},
		{`fromJSON('[10, 20]')[1]`, 20.0},
		{`fromJSON('[10, 20]')[5]`, nil},

		// Object filters.
		{`vars.fruits.*.name`, []any{"apple", "orange"}},
		{`github.event.issue.labels.*.name`, []any{"bug", "help wanted"}},
		{`contains(github.event.issue.labels.*.name, 'BUG')`, true},
		{`fromJSON('{"a": {"x": 1}, "b": {"x": 2}}').*.x`, []any{1.0, 2.0}},

		// Functions; names are case-insensitive.
		{`contains('Hello world', 'LLO')`, true},
		{`contains(fromJSON('["push", "pull_request"]'), github.event_name)`, true},
		{`startsWith('Hello world', 'he')`, true},
		{`endsWith('Hello world', 'LD')`, true},
		{`format('Hello {0} {1} {2}', 'Mona', 'the', 'Octocat')`, "Hello Mona the Octocat"},
		{`format('{{Hello {0} {1} {2}!}}', 'Mona', 'the', 'Octocat')`, "{Hello Mona the Octocat!}"},
		{`join(github.event.issue.labels.*.name, ', ')`, "bug, help wanted"},
		{`join(fromJSON('[1, true, null]'))`, "1,true,"},
		{`join('abc')`, "abc"},
		{`FromJSON('{"a": [1, 2]}').a[1]`, 2.0},
		{`fromJSON('true')`, true},
		{`toJSON(matrix.node)`, "18"},
		{`format('{0}', fromJSON('3.10'))`, "3.1"},
		{`format('{0}', matrix)`, "Object"},
	}
	ctx := testContext(t)
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got, err := Eval(tt.expr, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if g, w := toJSONString(t, got), toJSONString(t, tt.want); g != w {
				t.Errorf("got %s, want %s", g, w)
			}
		})
	}
}

func TestToJSON(t *testing.T) {
	got, err := Eval(`toJSON(fromJSON('{"b": 1, "a": [true, null, "<x>"], "e": {}}'))`, testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"b\": 1,\n  \"a\": [\n    true,\n    null,\n    \"<x>\"\n  ],\n  \"e\": {}\n}"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTruthy(t *testing.T) {
	for _, v := range []any{false, 0.0, math.Copysign(0, -1), math.NaN(), "", nil} {
		if Truthy(v) {
			t.Errorf("%#v should be falsy", v)
		}
	}
	for _, v := range []any{true, 1.0, "false", "0", []any{}, value.Object{}} {
		if !Truthy(v) {
			t.Errorf("%#v should be truthy", v)
		}
	}
}

func TestInterpolate(t *testing.T) {
	tests := []struct {
		src  string
		want any
	}{
		{`plain text`, "plain text"},
		{`Hello ${{ matrix.os }}!`, "Hello ubuntu-latest!"},
		{`${{ matrix.node }}`, 18.0},          // a lone expression keeps its type
		{`${{ matrix.experimental }}`, false}, // so false stays falsy
		{` ${{ matrix.node }}`, " 18"},        // surrounding text makes it a string
		{`${{ '' }}${{ matrix.node }}`, "18"}, // two expressions make a string
		{`${{ '}}' }}`, "}}"},                 // }} inside a string literal
		{`${{ fromJSON('{"os": ["a"]}') }}`, value.Object{{Key: "os", Value: []any{"a"}}}},
		{`node-${{ matrix.node }}-${{ matrix.os }}`, "node-18-ubuntu-latest"},
	}
	ctx := testContext(t)
	for _, tt := range tests {
		got, err := Interpolate(tt.src, ctx)
		if err != nil {
			t.Errorf("%s: %v", tt.src, err)
			continue
		}
		if g, w := toJSONString(t, got), toJSONString(t, tt.want); g != w {
			t.Errorf("%s: got %s, want %s", tt.src, g, w)
		}
	}
}

func TestEvalValueBuildsDynamicMatrix(t *testing.T) {
	ctx := testContext(t)
	ctx.Contexts["needs"] = fromJSONObject(t, `{"setup": {"outputs": {"versions": "[18, 20]"}}}`)
	m := value.Object{
		{Key: "node", Value: "${{ fromJSON(needs.setup.outputs.versions) }}"},
		{Key: "os", Value: []any{"ubuntu-latest"}},
	}
	got, err := EvalValue(m, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := toJSONString(t, got), `{"node":[18,20],"os":["ubuntu-latest"]}`; g != w {
		t.Errorf("got %s, want %s", g, w)
	}
}

func TestEvalCondition(t *testing.T) {
	tests := []struct {
		cond   string
		status Status
		want   bool
	}{
		{``, Status{}, true},
		{``, Status{Failed: true}, false},
		{`github.event_name == 'push'`, Status{}, true},
		{`github.event_name == 'push'`, Status{Failed: true}, false}, // implicit success() &&
		{`always() && github.event_name == 'push'`, Status{Failed: true}, true},
		{`failure()`, Status{Failed: true}, true},
		{`${{ failure() }}`, Status{}, false},
		{`cancelled()`, Status{Cancelled: true}, true},
		{`success()`, Status{Cancelled: true}, false},
		{`${{ false }}`, Status{}, false},
		{`!startsWith(github.ref, 'refs/tags/')`, Status{}, true},
		// Text around an expression makes the whole condition a non-empty
		// string, which is always true. A well-known GitHub pitfall, kept as is.
		{`${{ false }} && false`, Status{}, true},
		{`${{ github.event_name == 'pull_request' }} || true`, Status{}, true},
	}
	for _, tt := range tests {
		ctx := testContext(t)
		ctx.Status = &tt.status
		got, err := EvalCondition(tt.cond, ctx)
		if err != nil {
			t.Errorf("%q: %v", tt.cond, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%q with %+v = %v, want %v", tt.cond, tt.status, got, tt.want)
		}
	}
}

func TestErrors(t *testing.T) {
	tests := []struct{ expr, wantErr string }{
		{`secrets.TOKEN`, `unrecognized named-value: "secrets"`},
		{`nope()`, "unknown function nope()"},
		{`success()`, "only available in if: conditions"},
		{`contains('a')`, "takes 2 argument(s), got 1"},
		{`format('{1}', 'a')`, "references argument {1}"},
		{`format('a}', 'a')`, "unmatched '}'"},
		{`fromJSON('{')`, "fromJSON()"},
		{`fromJSON('1 2')`, "after the JSON value"},
		{`1 ==`, "unexpected end of expression"},
		{`'unterminated`, "unterminated string"},
		{`github.`, "expected a property name"},
		{`01`, `invalid number "01"`},
		{strings.Repeat("(", 200) + "1" + strings.Repeat(")", 200), "nested too deeply"},
		{strings.Repeat("!", 200) + "1", "nested too deeply"},
	}
	ctx := testContext(t)
	for _, tt := range tests {
		_, err := Eval(tt.expr, ctx)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%.40s: got error %v, want one containing %q", tt.expr, err, tt.wantErr)
		}
	}
	if _, err := Interpolate("${{ matrix.os", ctx); err == nil || !strings.Contains(err.Error(), "missing closing }}") {
		t.Errorf("unterminated interpolation: got %v", err)
	}
}
