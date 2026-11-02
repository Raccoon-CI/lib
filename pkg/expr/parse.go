// Package expr parses and evaluates GitHub Actions ${{ }} expressions.
//
// Semantics follow GitHub's documentation ("Evaluate expressions in
// workflows and actions"): loose typing with coercion to numbers,
// case-insensitive string comparison and property access, && / || returning
// operand values, and the `*` object filter.
package expr

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokEnd           // }} closing an interpolation
	tokIdent
	tokNumber
	tokString
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokDot
	tokStar
	tokComma
	tokNot
	tokLt
	tokLe
	tokGt
	tokGe
	tokEq
	tokNe
	tokAnd
	tokOr
)

type token struct {
	kind tokenKind
	text string // raw source text
	pos  int    // byte offset in the source
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) next() (token, error) {
	for l.pos < len(l.src) && strings.IndexByte(" \t\r\n", l.src[l.pos]) >= 0 {
		l.pos++
	}
	start := l.pos
	if start >= len(l.src) {
		return token{kind: tokEOF, pos: start}, nil
	}
	tok := func(k tokenKind, n int) (token, error) {
		l.pos += n
		return token{kind: k, text: l.src[start:l.pos], pos: start}, nil
	}
	rest := l.src[start:]
	c := rest[0]
	switch {
	case strings.HasPrefix(rest, "}}"):
		return tok(tokEnd, 2)
	case strings.HasPrefix(rest, "<="):
		return tok(tokLe, 2)
	case strings.HasPrefix(rest, ">="):
		return tok(tokGe, 2)
	case strings.HasPrefix(rest, "=="):
		return tok(tokEq, 2)
	case strings.HasPrefix(rest, "!="):
		return tok(tokNe, 2)
	case strings.HasPrefix(rest, "&&"):
		return tok(tokAnd, 2)
	case strings.HasPrefix(rest, "||"):
		return tok(tokOr, 2)
	}
	switch c {
	case '(':
		return tok(tokLParen, 1)
	case ')':
		return tok(tokRParen, 1)
	case '[':
		return tok(tokLBracket, 1)
	case ']':
		return tok(tokRBracket, 1)
	case '.':
		return tok(tokDot, 1)
	case '*':
		return tok(tokStar, 1)
	case ',':
		return tok(tokComma, 1)
	case '!':
		return tok(tokNot, 1)
	case '<':
		return tok(tokLt, 1)
	case '>':
		return tok(tokGt, 1)
	case '\'':
		for i := 1; i < len(rest); i++ {
			if rest[i] != '\'' {
				continue
			}
			if i+1 < len(rest) && rest[i+1] == '\'' { // '' is an escaped quote
				i++
				continue
			}
			return tok(tokString, i+1)
		}
		return token{}, fmt.Errorf("unterminated string starting at offset %d", start)
	}
	if isDigit(c) || c == '-' && len(rest) > 1 && isDigit(rest[1]) {
		n := 1
		for n < len(rest) && (isIdentChar(rest[n]) || rest[n] == '.' ||
			(rest[n] == '-' || rest[n] == '+') && (rest[n-1] == 'e' || rest[n-1] == 'E')) {
			n++
		}
		return tok(tokNumber, n)
	}
	if isIdentStart(c) {
		n := 1
		for n < len(rest) && isIdentChar(rest[n]) {
			n++
		}
		return tok(tokIdent, n)
	}
	return token{}, fmt.Errorf("unexpected character %q at offset %d", c, start)
}

func isDigit(c byte) bool      { return '0' <= c && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// Context names may contain dashes (steps.my-step.outputs).
func isIdentChar(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '-' }

// node is a parsed expression.
type node interface{}

type (
	literal  struct{ v any }
	variable struct{ name string } // a context such as github or matrix
	property struct {
		recv node
		name string
	}
	index  struct{ recv, idx node }
	filter struct{ recv node } // recv.* or recv[*]
	not    struct{ x node }
	binary struct {
		op   tokenKind
		l, r node
	}
	call struct {
		name string // lower case
		args []node
	}
)

// maxDepth bounds nesting so hostile expressions can't blow the stack.
const maxDepth = 100

type parser struct {
	lex   lexer
	tok   token
	depth int
}

// parse parses one expression. When inTemplate is true the expression must
// end with }}, and the returned offset is just past it.
func parse(src string, inTemplate bool) (node, int, error) {
	p := &parser{lex: lexer{src: src}}
	if err := p.advance(); err != nil {
		return nil, 0, err
	}
	n, err := p.or()
	if err != nil {
		return nil, 0, err
	}
	want := tokEOF
	if inTemplate {
		want = tokEnd
	}
	if p.tok.kind != want {
		if inTemplate && p.tok.kind == tokEOF {
			return nil, 0, fmt.Errorf("missing closing }}")
		}
		return nil, 0, p.unexpected()
	}
	return n, p.lex.pos, nil
}

func (p *parser) advance() error {
	t, err := p.lex.next()
	p.tok = t
	return err
}

func (p *parser) unexpected() error {
	if p.tok.kind == tokEOF {
		return fmt.Errorf("unexpected end of expression")
	}
	return fmt.Errorf("unexpected %q at offset %d", p.tok.text, p.tok.pos)
}

func (p *parser) expect(k tokenKind, what string) error {
	if p.tok.kind != k {
		return fmt.Errorf("expected %s, got %w", what, p.unexpected())
	}
	return p.advance()
}

// binaryLevel parses left-associative operators of one precedence level.
func (p *parser) binaryLevel(operand func() (node, error), ops ...tokenKind) (node, error) {
	l, err := operand()
	if err != nil {
		return nil, err
	}
	for slices.Contains(ops, p.tok.kind) {
		op := p.tok.kind
		if err := p.advance(); err != nil {
			return nil, err
		}
		r, err := operand()
		if err != nil {
			return nil, err
		}
		l = binary{op, l, r}
	}
	return l, nil
}

// Precedence, loosest first: ||, &&, == !=, < <= > >=, !, postfix.
func (p *parser) or() (node, error) {
	if p.depth++; p.depth > maxDepth {
		return nil, fmt.Errorf("expression nested too deeply")
	}
	defer func() { p.depth-- }()
	return p.binaryLevel(p.and, tokOr)
}

func (p *parser) and() (node, error) { return p.binaryLevel(p.equality, tokAnd) }
func (p *parser) equality() (node, error) {
	return p.binaryLevel(p.comparison, tokEq, tokNe)
}
func (p *parser) comparison() (node, error) {
	return p.binaryLevel(p.unary, tokLt, tokLe, tokGt, tokGe)
}

func (p *parser) unary() (node, error) {
	if p.tok.kind != tokNot {
		return p.postfix()
	}
	if p.depth++; p.depth > maxDepth {
		return nil, fmt.Errorf("expression nested too deeply")
	}
	defer func() { p.depth-- }()
	if err := p.advance(); err != nil {
		return nil, err
	}
	x, err := p.unary()
	if err != nil {
		return nil, err
	}
	return not{x}, nil
}

func (p *parser) postfix() (node, error) {
	n, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		switch p.tok.kind {
		case tokDot:
			if err := p.advance(); err != nil {
				return nil, err
			}
			switch p.tok.kind {
			case tokStar:
				n = filter{n}
			case tokIdent:
				n = property{n, p.tok.text}
			default:
				return nil, fmt.Errorf("expected a property name or * after '.', got %w", p.unexpected())
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		case tokLBracket:
			if err := p.advance(); err != nil {
				return nil, err
			}
			if p.tok.kind == tokStar {
				if err := p.advance(); err != nil {
					return nil, err
				}
				n = filter{n}
			} else {
				idx, err := p.or()
				if err != nil {
					return nil, err
				}
				n = index{n, idx}
			}
			if err := p.expect(tokRBracket, "']'"); err != nil {
				return nil, err
			}
		default:
			return n, nil
		}
	}
}

func (p *parser) primary() (node, error) {
	t := p.tok
	switch t.kind {
	case tokNumber:
		f, err := parseNumberLiteral(t.text)
		if err != nil {
			return nil, err
		}
		return literal{f}, p.advance()
	case tokString:
		s := strings.ReplaceAll(t.text[1:len(t.text)-1], "''", "'")
		return literal{s}, p.advance()
	case tokLParen:
		if err := p.advance(); err != nil {
			return nil, err
		}
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		return n, p.expect(tokRParen, "')'")
	case tokIdent:
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.tok.kind == tokLParen {
			return p.call(t.text)
		}
		switch t.text { // keywords are case-sensitive
		case "true":
			return literal{true}, nil
		case "false":
			return literal{false}, nil
		case "null":
			return literal{nil}, nil
		}
		return variable{strings.ToLower(t.text)}, nil
	}
	return nil, p.unexpected()
}

func (p *parser) call(name string) (node, error) {
	c := call{name: strings.ToLower(name)}
	if err := p.advance(); err != nil { // (
		return nil, err
	}
	if p.tok.kind == tokRParen {
		return c, p.advance()
	}
	for {
		arg, err := p.or()
		if err != nil {
			return nil, err
		}
		c.args = append(c.args, arg)
		if p.tok.kind != tokComma {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	return c, p.expect(tokRParen, "',' or ')' in call to "+name)
}

// parseNumberLiteral accepts JSON numbers plus 0x hex integers, as GitHub does.
func parseNumberLiteral(s string) (float64, error) {
	neg := strings.HasPrefix(s, "-")
	digits := strings.TrimPrefix(s, "-")
	if strings.HasPrefix(digits, "0x") || strings.HasPrefix(digits, "0X") {
		n, err := strconv.ParseUint(digits[2:], 16, 64)
		if err != nil || digits[2:] == "" {
			return 0, fmt.Errorf("invalid number %q", s)
		}
		f := float64(n)
		if neg {
			f = -f
		}
		return f, nil
	}
	if !jsonNumber(s) {
		return 0, fmt.Errorf("invalid number %q", s)
	}
	return strconv.ParseFloat(s, 64)
}

// jsonNumber reports whether s is a number in JSON syntax.
func jsonNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && isDigit(s[i]):
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		if i >= len(s) || !isDigit(s[i]) {
			return false
		}
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if i >= len(s) || !isDigit(s[i]) {
			return false
		}
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	return i == len(s)
}
