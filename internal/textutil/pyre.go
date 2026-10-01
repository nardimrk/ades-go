package textutil

import (
	"regexp"
	"strings"
)

// Python's \s and \w are Unicode-aware on str patterns, RE2's are ASCII-only.
// The ported heuristics were tuned on real messages that contain e.g. NBSP
// (U+00A0), so pyRe rewrites them to the Unicode classes Python uses.
const (
	pySpace = `\s\p{Z}\x{85}\x{1c}-\x{1f}`
	pyWord  = `\p{L}\p{N}_`
)

func pyRe(pattern string) *regexp.Regexp {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\\' && i+1 < len(pattern) {
			n := pattern[i+1]
			i++
			switch {
			case n == 's' && inClass:
				b.WriteString(pySpace)
			case n == 's':
				b.WriteString("[" + pySpace + "]")
			case n == 'w' && inClass:
				b.WriteString(pyWord)
			case n == 'w':
				b.WriteString("[" + pyWord + "]")
			default:
				b.WriteByte('\\')
				b.WriteByte(n)
			}
			continue
		}
		if c == '[' && !inClass {
			inClass = true
		} else if c == ']' && inClass {
			inClass = false
		}
		b.WriteByte(c)
	}
	return regexp.MustCompile(b.String())
}
