package textutil

import (
	"regexp"
	"strconv"
	"strings"
)

// Best-effort extraction of wine name / price / vintage from a listing body
// (port of app/parser.py). The LLM parser in package llm is tried first when
// configured; this is the offline fallback.

var (
	// €25 , 25€ , EUR 25 , 25 euro , $25
	priceRe     = pyRe(`(?i)(?:€|eur|euro|\$)\s*([0-9]+(?:[.,][0-9]{1,2})?)|([0-9]+(?:[.,][0-9]{1,2})?)\s*(?:€|eur|euro)`)
	yearRe      = pyRe(`\b(19[0-9]{2}|20[0-2][0-9])\b`)
	priceTailRe = pyRe(`(?i)[,;]?\s*(?:a bottiglia|cad(?:auno)?|l'una|each).*$`)

	// variant used by the .txt chat import (no "$")
	importPriceRe = pyRe(`(?i)(?:€|eur|euro)\s*([0-9]+(?:[.,][0-9]{1,2})?)|([0-9]+(?:[.,][0-9]{1,2})?)\s*(?:€|eur|euro)`)
)

const nameTrim = " -–—:•·,;"

type ListingFields struct {
	WineName *string
	Price    *float64
	Vintage  *int
}

func firstPrice(re *regexp.Regexp, text string) *float64 {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	raw := m[1]
	if raw == "" {
		raw = m[2]
	}
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64)
	if err != nil {
		return nil
	}
	return &v
}

func ParsePrice(text string) *float64 { return firstPrice(priceRe, text) }

func ParseVintage(text string) *int {
	m := yearRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	v, _ := strconv.Atoi(m[1])
	return &v
}

func firstLine(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	line, _, _ := strings.Cut(t, "\n")
	return strings.TrimRight(line, "\r")
}

// ParseWineName: first non-empty line stripped of price phrasings.
func ParseWineName(text string) *string {
	first := firstLine(text)
	if first == "" {
		return nil
	}
	first = priceRe.ReplaceAllString(first, "")
	first = priceTailRe.ReplaceAllString(first, "")
	first = firstRunes(strings.Trim(first, nameTrim), 120)
	if first == "" {
		return nil
	}
	return &first
}

func ParseListing(text string) ListingFields {
	return ListingFields{WineName: ParseWineName(text), Price: ParsePrice(text), Vintage: ParseVintage(text)}
}

// HasImportPrice reports a price in a message of a .txt chat export.
func HasImportPrice(text string) bool { return importPriceRe.MatchString(text) }

// ParseImportFields mirrors ParseListing with the .txt-import price pattern.
func ParseImportFields(body string) ListingFields {
	f := ListingFields{Price: firstPrice(importPriceRe, body), Vintage: ParseVintage(body)}
	if first := firstLine(body); first != "" {
		name := firstRunes(strings.Trim(importPriceRe.ReplaceAllString(first, ""), nameTrim), 120)
		if name != "" {
			f.WineName = &name
		}
	}
	return f
}
