package textutil

import (
	"strings"
	"unicode"
)

// isEmoji reports pictographic emoji and the invisible characters that build
// emoji sequences (variation selectors, zero-width joiner, skin tones, tags,
// keycap combiner).
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, symbols, flags, skin tones
		r >= 0x2600 && r <= 0x27BF,   // misc symbols & dingbats (☀ ✨ ✅ ✕ …)
		r >= 0x2B00 && r <= 0x2BFF,   // stars, arrows used as emoji (⭐ …)
		r >= 0xE0020 && r <= 0xE007F, // tag sequences
		r == 0xFE0F || r == 0xFE0E || r == 0x200D || r == 0x20E3,
		r == 0x2122 || r == 0x2139 || r == 0x3030 || r == 0x303D || r == 0x3297 || r == 0x3299:
		return true
	}
	return false
}

// StripEmoji removes emoji from text shown in the UI (e.g. "Luca ☀️☀️" →
// "Luca") and tidies the spaces they leave behind. Stored data is never
// changed: this is applied at display time only.
func StripEmoji(s string) string {
	if !strings.ContainsFunc(s, isEmoji) {
		return s
	}
	out := strings.Map(func(r rune) rune {
		if isEmoji(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimFunc(collapseWS(out), func(r rune) bool { return unicode.IsSpace(r) || r == '-' || r == '·' })
}
