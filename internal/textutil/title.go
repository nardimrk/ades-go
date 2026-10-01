package textutil

import "strings"

var (
	titleLeadQtyRe = pyRe(`^(?:\d+\s*)?[xX×]\s+`)              // "x ", "12 x ", "×"
	titlePriceRe   = pyRe(`(?i)(?:\s+a)?\s*(?:€|\beuro\b).*$`) // " a € + iva", "€.", "a euro"
	titleIvaRe     = pyRe(`(?i)\s*\+?\s*iva\.?$`)              // leftover "+ iva"
	titleDispRe    = pyRe(`(?i)^disponibil[ei]\s*:?\s*`)       // "Disponibili:"
)

const titleTrailing = " .,;:-–—•·!*_"

// MaxTitleLen is the longest title shown; longer ones are cut at a word.
const MaxTitleLen = 60

// CleanTitle turns a raw campaign key (e.g. "x Ducru-Beaucaillou 2025 a €.")
// into a readable title ("Ducru-Beaucaillou 2025"): no quantity "x" prefix,
// no price tail, no trailing punctuation, at most MaxTitleLen characters.
func CleanTitle(s string) string {
	t := collapseWS(s)
	t = titleDispRe.ReplaceAllString(t, "")
	t = titleLeadQtyRe.ReplaceAllString(t, "")
	if cut := titlePriceRe.ReplaceAllString(t, ""); strings.TrimSpace(cut) != "" {
		t = cut
	}
	t = titleIvaRe.ReplaceAllString(t, "")
	t = strings.Trim(t, titleTrailing+"\"'“”«»")
	if t == "" {
		return collapseWS(s)
	}
	return Shorten(t, MaxTitleLen)
}

// Shorten cuts s to at most n characters at a word boundary, adding "…".
func Shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndexAny(cut, " ,;:"); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, titleTrailing) + "…"
}
