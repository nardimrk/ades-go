package textutil

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ── helpers for Python-regex features RE2 lacks (lookbehind/lookahead) ──────

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// isWord mirrors Python's Unicode \w.
func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

var wsRe = pyRe(`\s+`)

// collapseWS mirrors re.sub(r"\s+", " ", s).strip().
func collapseWS(s string) string { return strings.TrimSpace(wsRe.ReplaceAllString(s, " ")) }

// firstRunes mirrors Python's s[:n].
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// ── numbers ──────────────────────────────────────────────────────────────────

var numberRe = pyRe(`\d+(?:[.,]\d+)?`)

// StripNumbers removes every number and collapses whitespace, so reposts that
// only change a digit compare as near-identical.
func StripNumbers(body string) string { return collapseWS(numberRe.ReplaceAllString(body, "")) }

// StripQuantityAndPrice removes quantity/price numbers but keeps a plausible
// vintage (a standalone 19xx/20xx token). Port of
// (?<!\d)(?!(?:19|20)\d{2}(?!\d))\d+(?:[.,]\d+)? replaced by "".
func StripQuantityAndPrice(text string) string {
	r := []rune(text)
	var b strings.Builder
	i := 0
	for i < len(r) {
		if !isDigit(r[i]) || (i > 0 && isDigit(r[i-1])) {
			b.WriteRune(r[i])
			i++
			continue
		}
		// start of a digit run: is it a vintage year?
		if i+4 <= len(r) && ((r[i] == '1' && r[i+1] == '9') || (r[i] == '2' && r[i+1] == '0')) &&
			isDigit(r[i+2]) && isDigit(r[i+3]) && (i+4 == len(r) || !isDigit(r[i+4])) {
			b.WriteRune(r[i])
			i++
			continue
		}
		j := i
		for j < len(r) && isDigit(r[j]) {
			j++
		}
		if j+1 < len(r) && (r[j] == '.' || r[j] == ',') && isDigit(r[j+1]) {
			j++
			for j < len(r) && isDigit(r[j]) {
				j++
			}
		}
		i = j
	}
	return collapseWS(b.String())
}

// ── campaign key ─────────────────────────────────────────────────────────────

var (
	optLineRe     = pyRe(`^[A-J]\s*[.\-\)]\s*`)
	parenNoteRe   = pyRe(`\s*\(.*?\)`)
	vendutoOnlyRe = pyRe(`(?i)^venduto\.?$`)
	disponibRe    = pyRe(`(?i)^disponib\w*`)
	salutationRe  = pyRe(`(?i)^(?:buongiorno|buonasera|buonanotte|ciao|salve)\w*(?:\s+a\s+tutt[ei])?[\s,!.:]*$`)
	categoryRe    = pyRe(`(?i)^(?:bianchi|rossi|rosati|bollicine|spumanti)\s*\d{0,4}\s*$`)
	disponibiliRe = pyRe(`(?i)\s*(cru\s+parcellari\s+)?disponibili.*`)
)

// CampaignKey extracts the producer/winery name used to group a listing and
// its reposts into one "campaign" (quantity/price stripped, vintage kept).
func CampaignKey(body string) string {
	seenOption := false
	firstOptionName := ""
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if loc := optLineRe.FindStringIndex(line); loc != nil {
			seenOption = true
			if firstOptionName == "" {
				rest := strings.TrimSpace(line[loc[1]:])
				rest = strings.TrimSpace(parenNoteRe.ReplaceAllString(rest, ""))
				// drop the "174 x " count: a repost that only adds or changes it
				// stays in the same campaign as the post without it
				rest = optQtyRe.ReplaceAllString(rest, "")
				// an already-sold option ("A. Venduto.") has no wine name
				if rest != "" && len([]rune(rest)) > 3 && !vendutoOnlyRe.MatchString(rest) {
					firstOptionName = StripQuantityAndPrice(rest)
				}
			}
			continue
		}
		if seenOption {
			continue // lines after options are descriptions, not headers
		}
		if disponibRe.MatchString(line) || salutationRe.MatchString(line) || categoryRe.MatchString(line) {
			continue
		}
		key := strings.TrimSpace(disponibiliRe.ReplaceAllString(line, ""))
		if key != "" && len([]rune(key)) > 3 {
			return StripQuantityAndPrice(key)
		}
	}
	if firstOptionName != "" {
		return firstOptionName
	}
	return StripQuantityAndPrice(firstRunes(body, 50))
}

// ── listing options (A./B./C. lines) ─────────────────────────────────────────

var (
	hasOptionsRe  = pyRe(`(?m)^\s*[A-J]\s*[.\-\)]`)
	optStartRe    = pyRe(`^([A-J])\s*[.\-\)]\s+`)
	vendutoRe     = pyRe(`(?i)^venduto`)
	optPriceRe    = pyRe(`(\d+[,.]?\d*)\s*€`)
	optQtyRe      = pyRe(`^(\d+)\s*[×xX]\s*`)
	optPriceTail  = pyRe(`\s+[aà]\s+[\d.,]+\s*€.*`)
	optVintageEnd = pyRe(`\s+\d{4}$`)
	// "(CASSA INTERA a 78€)" — the full-case price next to a bottle price
	optCaseRe = pyRe(`(?i)\(?\s*cassa\s+intera\s+(?:a\s+)?([\d.,]+)\s*€\s*\)?`)
)

func HasOptions(body string) bool { return hasOptionsRe.MatchString(body) }

// CaseSuffix marks the full-case variant of an option in quotation_items.option
// ("A-CASSA"): ordered with "1 cassa A", priced per case.
const CaseSuffix = "-CASSA"

type Option struct {
	Letter   string
	WineName string
	Quantity int // available bottles; 0 = not stated
	Price    float64
	Case     bool // full-case variant of Letter
}

// Code is the value stored in quotation_items.option.
func (o Option) Code() string {
	if o.Case {
		return o.Letter + CaseSuffix
	}
	return o.Letter
}

type optSegment struct{ letter, text string }

// optionSegments splits a body into lettered options, joining multi-line text.
func optionSegments(body string) []optSegment {
	var segs []optSegment
	var cur *optSegment
	for _, line := range strings.Split(body, "\n") {
		stripped := strings.TrimSpace(line)
		if m := optStartRe.FindStringSubmatchIndex(stripped); m != nil {
			if cur != nil {
				segs = append(segs, *cur)
			}
			cur = &optSegment{letter: strings.ToUpper(stripped[m[2]:m[3]]), text: strings.TrimSpace(stripped[m[1]:])}
		} else if cur != nil && stripped != "" {
			cur.text += " " + stripped
		} else if stripped == "" && cur != nil {
			segs = append(segs, *cur)
			cur = nil
		}
	}
	if cur != nil {
		segs = append(segs, *cur)
	}
	return segs
}

func lastPrice(text string) (float64, bool) {
	prices := optPriceRe.FindAllStringSubmatch(text, -1)
	if len(prices) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(prices[len(prices)-1][1], ",", "."), 64)
	return v, err == nil
}

func optionWineName(text string) string {
	wine := strings.TrimSpace(optPriceTail.ReplaceAllString(text, ""))
	return strings.TrimSpace(optVintageEnd.ReplaceAllString(wine, ""))
}

// ParseOptions parses the lettered wine options of a listing body.
//
// The original rule (port of the Python app) only accepts options with an
// explicit bottle count ("A. 6x Pinot Noir a 25€"). Listings written without
// counts ("A. Riesling 2025 a 14,50€ (CASSA INTERA a 78€)") fall back to
// parseOptionsNoQty, so listings that already parsed keep the same options.
// A listing can also mix both ("A. Cassa intera X a 119€" next to "B. 20 x X
// a 24,50€"): the options without a count are kept too, else option A would
// be lost.
func ParseOptions(body string) []Option {
	withQty := ParseOptionsWithQty(body)
	noQty := parseOptionsNoQty(body)
	if len(withQty) == 0 {
		return noQty
	}
	have := map[string]bool{}
	for _, o := range withQty {
		have[o.Letter] = true
	}
	out := withQty
	for _, o := range noQty {
		if !have[o.Letter] {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Letter < out[j].Letter })
	return out
}

// ParseOptionsWithQty is the original rule: sold-out options and options
// without an explicit bottle count or price are skipped.
func ParseOptionsWithQty(body string) []Option {
	var out []Option
	for _, sg := range optionSegments(body) {
		text := sg.text
		if vendutoRe.MatchString(text) {
			continue
		}
		// last price match, so a vintage like "2019€" isn't taken as the price
		price, ok := lastPrice(text)
		if !ok {
			continue
		}
		qty := 0
		wineRaw := text
		if m := optQtyRe.FindStringSubmatchIndex(text); m != nil {
			qty, _ = strconv.Atoi(text[m[2]:m[3]])
			wineRaw = text[m[1]:]
			if qty >= 1900 { // a vintage year, not a bottle count
				qty = 0
				wineRaw = text
			}
		}
		if qty == 0 {
			continue // e.g. "Cassa intera" lines without a bottle count
		}
		if wine := optionWineName(wineRaw); wine != "" {
			out = append(out, Option{Letter: sg.letter, WineName: wine, Quantity: qty, Price: price})
		}
	}
	return out
}

// "Cassa intera (12 x 375ml) sconto 15%." — a case offer for every option of
// the listing, on a line of its own: the case size and the discount
var caseOfferRe = pyRe(`(?im)^\s*(?:cass[ae]|carton[ei])\s+(?:intera|intere|chiusa|completa|da)?\s*\(?\s*(?:da\s+)?(\d{1,2})\s*(?:[x×]\s*\d+\s*(?:ml|cl|l)\b|bott\w*)?\s*\)?[^\n]*?sconto\s+(?:del\s+)?(\d{1,2}(?:[.,]\d+)?)\s*%`)

// CaseOffer reads a listing-wide case offer with a discount ("Cassa intera
// (12 x 375ml) sconto 15%"): the bottles per case and the discount in %.
func CaseOffer(body string) (size int, discount float64, ok bool) {
	m := caseOfferRe.FindStringSubmatch(body)
	if m == nil {
		return 0, 0, false
	}
	size, _ = strconv.Atoi(m[1])
	discount, err := strconv.ParseFloat(strings.ReplaceAll(m[2], ",", "."), 64)
	if err != nil || size < 2 || discount <= 0 || discount >= 100 {
		return 0, 0, false
	}
	return size, discount, true
}

// WithCaseOptions adds a case variant ("A-CASSA") of every bottle option when
// the listing offers discounted full cases (see CaseOffer) and has no case
// options yet: size × bottle price, minus the discount, rounded to the cent.
// The name carries the size ("(cassa da 12, sconto 15%)"), which
// CaseSizeFromName reads back.
func WithCaseOptions(body string, opts []Option) []Option {
	size, discount, ok := CaseOffer(body)
	if !ok {
		return opts
	}
	for _, o := range opts {
		if o.Case {
			return opts
		}
	}
	out := append([]Option(nil), opts...)
	for _, o := range opts {
		price := math.Round(float64(size)*o.Price*(100-discount)) / 100
		name := fmt.Sprintf("%s (cassa da %d, sconto %s%%)", o.WineName, size, strconv.FormatFloat(discount, 'f', -1, 64))
		out = append(out, Option{Letter: o.Letter, WineName: name, Price: price, Case: true})
	}
	return out
}

var caseSizeRe = pyRe(`(?i)\(cassa da (\d+)`)

// CaseSizeFromName reads the bottles per case from a case option's name
// ("Roederer Brut Rose (cassa da 6)" → 6); 0 when it isn't stated.
func CaseSizeFromName(name string) int {
	m := caseSizeRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// parseOptionsNoQty reads options without a bottle count: the bottle price,
// plus a separate case option when a "CASSA INTERA a N€" price is given.
func parseOptionsNoQty(body string) []Option {
	var out []Option
	for _, sg := range optionSegments(body) {
		text := sg.text
		if vendutoRe.MatchString(text) {
			continue
		}
		var casePrice float64
		hasCase := false
		if m := optCaseRe.FindStringSubmatch(text); m != nil {
			if v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64); err == nil {
				casePrice, hasCase = v, true
			}
			text = strings.TrimSpace(optCaseRe.ReplaceAllString(text, " "))
		}
		price, ok := lastPrice(text)
		wine := optionWineName(text)
		if wine == "" {
			continue
		}
		if ok {
			out = append(out, Option{Letter: sg.letter, WineName: wine, Price: price})
		}
		if hasCase {
			out = append(out, Option{Letter: sg.letter, WineName: wine + " (cassa intera)", Price: casePrice, Case: true})
		}
	}
	return out
}

// ParseSingleWine reads a listing that offers one wine without option
// letters ("Disponibili:\n30 x Château Langoa Barton 2022 a 47,50€"): the
// wine becomes option A (plus A-CASSA for a "(CASSA INTERA a N€)" price).
// ok is false when the listing has lettered options or more than one priced
// line. The vintage stays in the name: there is no letter to tell wines apart.
func ParseSingleWine(body string) (opts []Option, ok bool) {
	if HasOptions(body) {
		return nil, false
	}
	var found string
	for _, line := range strings.Split(StripEmoji(body), "\n") {
		line = strings.TrimSpace(availPrefixRe.ReplaceAllString(strings.TrimSpace(line), ""))
		if line == "" || caseOfferRe.MatchString(line) || !optPriceRe.MatchString(line) {
			continue
		}
		if found != "" || vendutoRe.MatchString(line) {
			return nil, false
		}
		found = line
	}
	if found == "" {
		return nil, false
	}
	text := found
	var casePrice float64
	hasCase := false
	if m := optCaseRe.FindStringSubmatch(text); m != nil {
		if v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64); err == nil {
			casePrice, hasCase = v, true
		}
		text = strings.TrimSpace(optCaseRe.ReplaceAllString(text, " "))
	}
	price, okPrice := lastPrice(text)
	if !okPrice {
		return nil, false
	}
	qty := 0
	if m := optQtyRe.FindStringSubmatchIndex(text); m != nil {
		if n, _ := strconv.Atoi(text[m[2]:m[3]]); n < 1900 {
			qty, text = n, text[m[1]:]
		}
	}
	wine := strings.Trim(strings.TrimSpace(optPriceTail.ReplaceAllString(text, "")), " -–—:.,;*")
	if wine == "" || len([]rune(wine)) > 90 {
		return nil, false
	}
	opts = []Option{{Letter: "A", WineName: wine, Quantity: qty, Price: price}}
	if hasCase {
		opts = append(opts, Option{Letter: "A", WineName: wine + " (cassa intera)", Price: casePrice, Case: true})
	}
	return WithCaseOptions(body, opts), true
}

// "Disponibili:" in front of the wine line
var availPrefixRe = pyRe(`(?i)^disponib\w*\s*:?\s*`)

// ── order codes in replies ("3A, 2b") ────────────────────────────────────────

type OrderCode struct {
	Qty    int
	Option string
	Case   bool // "1 cassa A": a full case, not bottles
}

// ParseOrderCodes is a port of findall on
// (?<!\w)(\d+)\s*[xX×\-]?\s*([A-Ja-j])(?!\w)
// returning quantity + upper-cased option letter.
func ParseOrderCodes(body string) []OrderCode {
	r := []rune(body)
	var out []OrderCode
	i := 0
	for i < len(r) {
		if !isDigit(r[i]) || (i > 0 && isWord(r[i-1])) {
			i++
			continue
		}
		j := i
		for j < len(r) && isDigit(r[j]) {
			j++
		}
		digits := string(r[i:j])
		k := j
		for k < len(r) && unicode.IsSpace(r[k]) {
			k++
		}
		if k < len(r) && (r[k] == 'x' || r[k] == 'X' || r[k] == '×' || r[k] == '-') {
			k++
		}
		for k < len(r) && unicode.IsSpace(r[k]) {
			k++
		}
		if k < len(r) && ((r[k] >= 'A' && r[k] <= 'J') || (r[k] >= 'a' && r[k] <= 'j')) &&
			(k+1 == len(r) || !isWord(r[k+1])) {
			qty, err := strconv.Atoi(digits)
			if err == nil {
				out = append(out, OrderCode{Qty: qty, Option: strings.ToUpper(string(r[k]))})
			}
			i = k + 1
			continue
		}
		i = j // the rest of this digit run is preceded by a digit: can't match
	}
	return out
}

var (
	// "1 cassa A", "3 casse B", "1cassa C", "una cassa A", "2 cartoni di A",
	// "una cassa intera di B"; without a number only with "intera"
	// ("Cassa intera A")
	caseOrderRe = pyRe(`(?i)(?:^|[^\p{L}\p{N}_])(?:(\d+|una|un)\s*(?:casse|cassa|cartoni|cartone)(?:\s+intera|\s+intere)?|cassa\s+intera|cartone\s+intero)\s+(?:di\s+|da\s+)?([A-J])`)
	// "12x375ml B", "6 x 75cl di A": bottles of a stated size
	sizedOrderRe = pyRe(`(?i)(?:^|[^\p{L}\p{N}_])(\d{1,3})\s*[xX×]\s*\d{2,4}\s*(?:ml|cl|l)\s+(?:di\s+|del\s+)?([A-J])(?:[^\p{L}\p{N}_]|$)`)
	// "1 cassa", "una cassa Roederer…": a case order that doesn't name the
	// option (matched only when the order has a single option)
	caseNoLetterRe = pyRe(`(?i)(?:^|[^\p{L}\p{N}_])(\d+|una|un)\s*(?:casse|cassa|cartoni|cartone)(?:[^\p{L}\p{N}_]|$)`)
	// a line that is only "A 6", "B x 3", "C-2" (letter first)
	letterFirstRe = pyRe(`^\s*([A-J])\s*[xX×\-]?\s*(\d+)\s*(?:bott\w*)?\s*[.,!]?\s*$`)
)

// ParseOrders reads every order in a reply: the standard codes ("3A", "2 x B",
// see ParseOrderCodes), letter-first lines ("A 6", "C x 3") and full-case
// orders ("1 cassa A" → Case, matched against the option's "-CASSA" variant).
//
// Letter-first lines are taken out before the standard parser runs: on
// "A 1\nB 1" it would otherwise pair each number with the next line's letter.
func ParseOrders(body string) []OrderCode {
	var out, letterFirst []OrderCode
	var rest []string
	for _, line := range strings.Split(body, "\n") {
		if m := letterFirstRe.FindStringSubmatch(line); m != nil {
			if qty, err := strconv.Atoi(m[2]); err == nil && qty < 1900 {
				letterFirst = append(letterFirst, OrderCode{Qty: qty, Option: m[1]})
				continue
			}
		}
		rest = append(rest, line)
	}
	restText := strings.Join(rest, "\n")
	out = append(ParseOrderCodes(restText), letterFirst...)
	for _, m := range sizedOrderRe.FindAllStringSubmatch(restText, -1) {
		if qty, err := strconv.Atoi(m[1]); err == nil && qty > 0 {
			out = append(out, OrderCode{Qty: qty, Option: strings.ToUpper(m[2])})
		}
	}
	var lettered [][2]int // spans of "N cassa X", so the letterless rule skips them
	for _, loc := range caseOrderRe.FindAllStringSubmatchIndex(restText, -1) {
		end := loc[1]
		if r, _ := utf8.DecodeRuneInString(restText[end:]); end < len(restText) && isWord(r) {
			continue // "1 cassa Amarone": the letter starts a word
		}
		qty, start := 1, loc[0]
		if loc[2] >= 0 {
			start = loc[2]
			if n, err := strconv.Atoi(restText[loc[2]:loc[3]]); err == nil {
				qty = n
			}
		}
		out = append(out, OrderCode{Qty: qty, Option: strings.ToUpper(restText[loc[4]:loc[5]]), Case: true})
		lettered = append(lettered, [2]int{start, loc[5]})
	}
	for _, loc := range caseNoLetterRe.FindAllStringSubmatchIndex(restText, -1) {
		inside := false
		for _, sp := range lettered {
			if loc[2] >= sp[0] && loc[2] < sp[1] {
				inside = true
			}
		}
		if inside {
			continue
		}
		qty := 1
		if n, err := strconv.Atoi(restText[loc[2]:loc[3]]); err == nil {
			qty = n
		}
		out = append(out, OrderCode{Qty: qty, Case: true}) // Option "" = not named
	}
	return out
}

// Key is the quotation_items.option an order refers to ("A" or "A-CASSA").
// A case order without a letter has Key "-CASSA": the caller resolves it to
// the order's only option, if it has just one.
func (c OrderCode) Key() string {
	if c.Case {
		return c.Option + CaseSuffix
	}
	return c.Option
}

// FormatOrderCodes writes codes the way ParseOrders reads them back:
// "3A, 1 cassa B".
func FormatOrderCodes(codes []OrderCode) string {
	parts := make([]string, len(codes))
	for i, c := range codes {
		if c.Case {
			parts[i] = strconv.Itoa(c.Qty) + " cassa " + c.Option
		} else {
			parts[i] = strconv.Itoa(c.Qty) + c.Option
		}
	}
	return strings.Join(parts, ", ")
}

var quotRe = pyRe(`\b\d{2}S\d{4}\b`)

// MentionsQuotationNumber reports a quotation number like 26S0001 in body.
func MentionsQuotationNumber(body string) bool { return quotRe.MatchString(body) }
