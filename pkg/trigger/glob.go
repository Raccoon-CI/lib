package trigger

import (
	"fmt"
	"regexp"
	"strings"
)

// compile turns one of GitHub's filter patterns (the ones branches, tags and
// paths take) into a regexp. They are not shell globs. A single star matches
// any characters except a slash; two stars match anything, slashes included,
// and "**/" may also match nothing, so "**/README.md" matches README.md at
// the top. A question mark means zero or one of the character before it, a
// plus sign one or more of it. Square brackets hold a character set or range.
// A backslash takes the next character literally.
func compile(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch c {
		case '*':
			if i+1 < len(rs) && rs[i+1] == '*' {
				i++
				if i+1 < len(rs) && rs[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?', '+':
			if i == 0 {
				return nil, fmt.Errorf("pattern %q: %c needs a character before it", pattern, c)
			}
			b.WriteRune(c)
		case '[':
			j := i + 1
			for j < len(rs) && rs[j] != ']' {
				j++
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("pattern %q: unclosed [", pattern)
			}
			b.WriteString("[")
			for _, k := range rs[i+1 : j] {
				if k == '\\' || k == '[' {
					b.WriteRune('\\')
				}
				b.WriteRune(k)
			}
			b.WriteString("]")
			i = j
		case '\\':
			if i+1 >= len(rs) {
				return nil, fmt.Errorf("pattern %q: trailing backslash", pattern)
			}
			i++
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("pattern %q: %v", pattern, err)
	}
	return re, nil
}

// MatchList is matchList for other packages that read the same patterns
// (workspace globs, path footprints).
func MatchList(patterns []string, s string) (bool, error) { return matchList(patterns, s) }

// matchList reports whether s is selected by an ordered list of patterns, as
// GitHub evaluates them: a pattern that matches selects s, a "!pattern" that
// matches deselects it again, and the last one that matched decides. A list
// with "!" patterns must also have a positive one.
func matchList(patterns []string, s string) (bool, error) {
	positive := false
	for _, p := range patterns {
		if !strings.HasPrefix(p, "!") {
			positive = true
		}
	}
	if !positive && len(patterns) > 0 {
		return false, fmt.Errorf("patterns %q: a list of negative patterns needs at least one positive one", patterns)
	}
	selected := false
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		re, err := compile(strings.TrimPrefix(p, "!"))
		if err != nil {
			return false, err
		}
		if re.MatchString(s) {
			selected = !neg
		}
	}
	return selected, nil
}
