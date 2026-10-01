package service

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"adesgo/internal/textutil"
)

// Building the Prodotti table from the listings: every wine offered in a
// listing becomes one product named "<wine> <vintage>" (prices ignored).

// ProductCandidate is a wine found in the listings.
type ProductCandidate struct {
	Description string // "Château Pontet-Canet 2014"
	Winery      string // producer, when known (LLM)
	Area        string // region / appellation, when known (LLM)
	Vintage     string // "2014", "" for non-vintage
	Listings    int    // listings mentioning it
	Example     string // a source line, for review
}

var (
	// "[A. ]N x <wine> a <price>€" — the option letter or the quantity is
	// required, so prose like "prezzo speciale a 25€" isn't taken as a wine.
	wineLineRe = regexp.MustCompile(`(?i)^\s*(?:([A-J])\s*[.)\-]\s*)?(?:(\d+)\s*[x×]\s+)?(.+?)\s+a\s+[\d.,]+\s*€`)
	optLineRe  = regexp.MustCompile(`^\s*[A-J]\s*[.)\-]\s*`)
	parenRe    = regexp.MustCompile(`\s*\([^)]*\)`)
	vintageRe  = regexp.MustCompile(`\b(19[5-9]\d|20[0-4]\d)\b`)
	// a format / packaging prefix: "Magnum", "Jeroboam", "CASSA INTERA" …
	formatRe   = regexp.MustCompile(`(?i)^(?:mezz[ae]\s+bottigli[ae]|bottigli[ae]|magnum|doppi[ao]\s+magnum|jeroboam|rehoboam|mathusalem|imperial[ei]?|cass[ae](?:\s+intera)?|cartone|mixed\s+case|formato|demi\s+bouteilles?)\b[\s:,-]*`)
	leadingXRe = regexp.MustCompile(`(?i)^(?:[x×]\s+|[A-J]\.\s+)`)
	spacesRe   = regexp.MustCompile(`\s+`)
	// header noise: "disponibili:", categories ("Bianchi 2023"), salutations
	headerDispRe = regexp.MustCompile(`(?i)\s*(?:cru\s+parcellari\s+|demi\s+bouteilles\s+)?disponib\w*.*$`)
	// bottle sizes inside a name: "375ml", "Bott 750ml", "1,5 lt"
	sizeRe = regexp.MustCompile(`(?i)\s*\b(?:(?:bott\.?\s*)?\d+(?:[.,]\d+)?\s*(?:ml|cl|lt|l)\b\.?|demi(?:\s+bouteilles?)?\b|mezz[ae]\s+bottigli[ae]\b)`)
	categoryRe   = regexp.MustCompile(`(?i)^(?:bianchi|rossi|rosati|bollicine|spumanti|champagne|dolci)\s*(?:\d{4})?\s*:?$`)
	greetingRe   = regexp.MustCompile(`(?i)^(?:buongiorno|buonasera|ciao|salve|ecco|oggi|nuov[oiae]|ultim|esclusiv|offert|promo|novit|arriv|in\s+arrivo|prezz|minimo|consegn)`)
)

// listingHeader reads the lines before the first lettered option: a short
// producer name ("Marc Colin", "Domaine Bertrand-Bachelet") and a vintage
// ("Marc Colin 2023 disponibili:", "Bianchi 2024").
func listingHeader(lines []string) (producer, vintage string) {
	for _, l := range lines {
		l = strings.TrimSpace(textutil.StripEmoji(l))
		if optLineRe.MatchString(l) {
			break
		}
		if l == "" {
			continue
		}
		if v := vintageRe.FindAllString(l, -1); len(v) > 0 && vintage == "" {
			vintage = v[len(v)-1]
		}
		h := strings.TrimSpace(headerDispRe.ReplaceAllString(l, ""))
		h = strings.Trim(strings.TrimSpace(vintageRe.ReplaceAllString(h, "")), " -–—:.,;|")
		if producer != "" || h == "" || categoryRe.MatchString(h) || greetingRe.MatchString(h) {
			continue
		}
		words := strings.Fields(h)
		// a name, not a sentence: few words, starts uppercase, no sentence punctuation, no prices
		if len(words) > 6 || strings.ContainsAny(h, ".!?€") || !unicode.IsUpper([]rune(h)[0]) {
			continue
		}
		producer = h
	}
	return producer, vintage
}

// wineFromLine returns the wine of a listing line ("" if none) and its vintage.
func wineFromLine(line string) (string, string) {
	line = strings.TrimSpace(textutil.StripEmoji(line))
	line = strings.TrimSpace(regexp.MustCompile(`(?i)^disponib\w*\s*:?\s*`).ReplaceAllString(line, ""))
	m := wineLineRe.FindStringSubmatch(line)
	if m == nil || (m[1] == "" && m[2] == "") {
		return "", ""
	}
	name := parenRe.ReplaceAllString(m[3], "")
	name = leadingXRe.ReplaceAllString(strings.TrimSpace(name), "")
	name = strings.TrimSpace(formatRe.ReplaceAllString(name, ""))
	name = sizeRe.ReplaceAllString(name, "")
	name = strings.Trim(spacesRe.ReplaceAllString(name, " "), " -–—:.,;*_\"'")
	if name == "" || strings.Contains(strings.ToLower(name), "venduto") || len([]rune(name)) < 4 || len([]rune(name)) > 90 {
		return "", ""
	}
	vintage := ""
	if v := vintageRe.FindAllString(name, -1); len(v) > 0 {
		vintage = v[len(v)-1]
		name = strings.Trim(strings.TrimSpace(spacesRe.ReplaceAllString(strings.Replace(name, vintage, "", 1), " ")), " -–—:.,;")
	}
	return name, vintage
}

// withVintage puts the vintage at the end of the name.
func withVintage(name, vintage string) string {
	if vintage == "" {
		return name
	}
	return name + " " + vintage
}

// sharesWords reports whether name already contains at least half of the
// producer's distinctive words ("Theo Dancer - Roc Breïa" + "Roc Breïa
// Chardonnay"), so the producer isn't prepended twice.
func sharesWords(name, producer string) bool {
	nk := " " + ProductKey(name) + " "
	total, found := 0, 0
	for _, w := range strings.Fields(ProductKey(producer)) {
		if len(w) <= 2 {
			continue
		}
		total++
		if strings.Contains(nk, " "+w+" ") {
			found++
		}
	}
	return total > 0 && found*2 >= total
}

// ProductKey identifies the same wine + vintage across spellings:
// case, accents, "Château/Chateau/Ch." and punctuation are ignored.
func ProductKey(desc string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	s, _, _ := transform.String(t, strings.ToLower(desc))
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return ' '
	}, s)
	var words []string
	for _, w := range strings.Fields(s) {
		if w == "chateau" || w == "ch" || w == "domaine" || w == "dom" {
			continue
		}
		words = append(words, w)
	}
	return strings.Join(words, " ")
}

// ProductCandidatesFromListings extracts the wines of every listing with
// the rules above, merged by ProductKey, most mentioned first.
func (s *Service) ProductCandidatesFromListings(ctx context.Context) ([]ProductCandidate, int, error) {
	rows, err := s.db().QueryContext(ctx, "SELECT COALESCE(body,'') FROM listings")
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	type agg struct {
		ProductCandidate
		spellings map[string]int
	}
	byKey := map[string]*agg{}
	var order []string
	withoutWine := 0
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, 0, err
		}
		found := map[string]bool{}
		lines := strings.Split(body, "\n")
		producer, headerVintage := listingHeader(lines)
		for _, line := range lines {
			name, vintage := wineFromLine(line)
			if name == "" {
				continue
			}
			if vintage == "" {
				vintage = headerVintage
			}
			winery := ""
			if producer != "" {
				winery = producer
				if !sharesWords(name, producer) {
					name = producer + " " + name
				}
			}
			desc := withVintage(name, vintage)
			k := ProductKey(desc)
			a, ok := byKey[k]
			if !ok {
				a = &agg{ProductCandidate: ProductCandidate{Vintage: vintage, Winery: winery, Example: strings.TrimSpace(line)}, spellings: map[string]int{}}
				byKey[k] = a
				order = append(order, k)
			}
			a.spellings[desc]++
			if !found[k] {
				a.Listings++
				found[k] = true
			}
		}
		if len(found) == 0 {
			withoutWine++
		}
	}
	out := make([]ProductCandidate, 0, len(order))
	for _, k := range order {
		a := byKey[k]
		// the most used spelling, preferring the accented one on ties ("Château")
		best, bestN := "", 0
		for sp, n := range a.spellings {
			if n > bestN || (n == bestN && sp > best) {
				best, bestN = sp, n
			}
		}
		a.Description = best
		out = append(out, a.ProductCandidate)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Listings > out[j].Listings })
	return out, withoutWine, rows.Err()
}

// ExistingProductKeys returns the ProductKey of every product in Prodotti.
func (s *Service) ExistingProductKeys(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db().QueryContext(ctx, "SELECT COALESCE(description,'') FROM items WHERE deleted_at IS NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[string]bool{}
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil && d != "" {
			keys[ProductKey(d)] = true
		}
	}
	return keys, rows.Err()
}

// InsertProducts adds the candidates as new products (next ITM codes).
func (s *Service) InsertProducts(ctx context.Context, cands []ProductCandidate) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, c := range cands {
			if _, err := tx.ExecContext(ctx, `INSERT INTO items (itemCode, description, winery, area, created_at)
				VALUES (?, ?, ?, ?, datetime('now'))`, nextItemCode(ctx, tx), c.Description, c.Winery, c.Area); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
