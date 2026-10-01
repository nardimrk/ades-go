// Package classify is the hybrid rule + LLM classification of WhatsApp
// messages (port of app/classify.py).
//
// Fast path (no LLM call, handles most messages): compare an owner message
// against the owner's recent listings in the chat with numbers stripped, so a
// plain price/quantity bump still matches its original listing. Only the gray
// zone between the two thresholds asks the LLM, with a safe rule-based
// fallback when it's unavailable.
package classify

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"adesgo/internal/db"
	"adesgo/internal/llm"
	"adesgo/internal/textutil"
)

const (
	updateThreshold = 0.82 // at/above: confidently a repost/edit of the same listing
	createThreshold = 0.45 // below: confidently unrelated to any recent listing
)

var priceMarkRe = regexp.MustCompile(`(?i)€|\beuro\b`)

type Kind string

const (
	Create Kind = "create"
	Update Kind = "update"
	Reply  Kind = "reply"
)

type Classifier struct {
	LLM *llm.Client
}

func bestMatch(body string, candidates []db.Candidate) (*db.Candidate, float64) {
	normalized := textutil.StripNumbers(body)
	var best *db.Candidate
	bestRatio := 0.0
	for i := range candidates {
		r := textutil.Ratio(normalized, textutil.StripNumbers(candidates[i].Body))
		if r > bestRatio {
			best, bestRatio = &candidates[i], r
		}
	}
	return best, bestRatio
}

// OwnerMessage classifies an owner-authored message given their recent own
// listings in the chat (newest first). The matched candidate is returned for
// updates.
func (c *Classifier) OwnerMessage(ctx context.Context, body string, candidates []db.Candidate) (Kind, *db.Candidate) {
	match, ratio := bestMatch(body, candidates)
	hasPrice := priceMarkRe.MatchString(body)
	fallback := Reply
	if hasPrice {
		fallback = Create
	}

	if ratio >= updateThreshold {
		return Update, match
	}
	if ratio < createThreshold {
		return fallback, nil
	}
	candBody := ""
	if match != nil {
		candBody = match.Body
	}
	switch c.LLM.ClassifyListing(ctx, body, candBody) {
	case "update":
		if match != nil {
			return Update, match
		}
	case "create":
		return Create, nil
	case "reply":
		return Reply, nil
	}
	return fallback, nil
}

// ResolveOrderUpdate: a reply without an order code may still modify the
// sender's latest order for this listing in natural language. Returns the
// canonical replacement code, or "". ownerNotes are the listing owner's
// messages on it, newest first: a claim of the bottles they offer ("Mie!")
// adds them to the order, by rules when the LLM gives no answer.
func (c *Classifier) ResolveOrderUpdate(ctx context.Context, body, listing string, ts int64, priorReplies, ownerNotes []db.Candidate) string {
	if len(textutil.ParseOrderCodes(body)) > 0 {
		return ""
	}
	notes, offer := recentOffer(ownerNotes, ts)
	for _, r := range priorReplies {
		codes := textutil.ParseOrderCodes(r.Body)
		if len(codes) == 0 {
			continue
		}
		if code := c.LLM.ResolveOrderUpdate(ctx, body, textutil.FormatOrderCodes(codes), joinNotes(notes)); code != "" {
			return code
		}
		claim := textutil.ParseOrderCodes(ClaimOffer(body, offer, listing))
		if len(claim) == 0 {
			return ""
		}
		qty := claim[0].Qty
		for _, p := range codes {
			if p.Option == claim[0].Option {
				qty = p.Qty + claim[0].Qty
			}
		}
		return strconv.Itoa(qty) + claim[0].Option
	}
	return ""
}

// HasPriorOrder reports whether one of these replies holds an order code.
func HasPriorOrder(replies []db.Candidate) bool {
	for _, r := range replies {
		if len(textutil.ParseOrders(r.Body)) > 0 {
			return true
		}
	}
	return false
}

const offerWindow = 2 * 86400 // an owner offer is claimable for two days

// recentOffer keeps the owner notes of the last offerWindow before ts and
// returns, as offer, the latest one when it offers bottles ("rimangono
// disponibili 4 ...").
func recentOffer(ownerNotes []db.Candidate, ts int64) (notes []db.Candidate, offer string) {
	for _, n := range ownerNotes {
		if ts-n.Timestamp <= offerWindow {
			notes = append(notes, n)
		}
	}
	if len(notes) > 0 && offerRe.MatchString(notes[0].Body) {
		offer = notes[0].Body
	}
	return notes, offer
}

// ResolveNewOrder: a customer with no order yet in this listing writes one
// without option codes — claiming the bottles the owner just offered
// ("Mie!" after "rimangono disponibili 4 mezze bottiglie di Coutet 2019") or
// naming the wine ("1 Coutet 2019"). Returns canonical codes or "".
// The LLM is asked only when the reply contains a number or follows a recent
// owner offer; a claim of an offer falls back to rules when it gives no answer
// (no API key, quota exhausted).
func (c *Classifier) ResolveNewOrder(ctx context.Context, body, listing string, ts int64, ownerNotes []db.Candidate) string {
	if len(textutil.ParseOrders(body)) > 0 || listing == "" {
		return ""
	}
	notes, offer := recentOffer(ownerNotes, ts)
	if offer == "" && !digitRe.MatchString(body) {
		return ""
	}
	if codes := validCodes(c.LLM.ResolveNewOrder(ctx, body, listing, joinNotes(notes)), textutil.ParseOptions(listing)); codes != "" {
		return codes
	}
	return ClaimOffer(body, offer, listing)
}

var (
	digitRe = regexp.MustCompile(`\d`)
	offerRe = regexp.MustCompile(`(?i)(rimang|rest[aio]n|ancora disponibil|disponibil[ei] ancora|ultim[ei] \d)[^\d]{0,40}\d+`)
	// the offered quantity and what follows it: "4 mezze bottiglie di Coutet 2019"
	offerQtyRe = regexp.MustCompile(`(?i)(?:rimang\w*|rest\w*|disponibil\w*|ultim\w*)[^\d]{0,40}?(\d+)\s+([^.\n!]+)`)
	claimRe    = regexp.MustCompile(`(?i)^\s*(mie|mia|mio|miei|io|per me|prendo io|le prendo( io)?|la prendo( io)?|prenoto( io)?|prenoto tutto|tutte mie)\s*[!.]*\s*(grazie)?\s*[!.]*\s*$`)
	yearRe     = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	wordRe     = regexp.MustCompile(`[\p{L}]{4,}`)
	// a listing option line: "B. 9 x Château Coutet 2019 a 23,50€"
	optionLineRe = regexp.MustCompile(`(?m)^\s*([A-Ja-j])[.)]\s+(.+)$`)
)

// ClaimOffer is the rule-based reading of a claim ("Mie!") of the bottles
// the owner offered: the offered quantity on the only listing option line
// ("B. 9 x Château Coutet 2019 a 23,50€") that has the offer's vintage and
// a word of its wine name.
func ClaimOffer(body, offer, listing string) string {
	if !claimRe.MatchString(body) {
		return ""
	}
	m := offerQtyRe.FindStringSubmatch(offer)
	if m == nil {
		return ""
	}
	qty, _ := strconv.Atoi(m[1])
	what := strings.ToLower(m[2])
	year := yearRe.FindString(what)
	match := ""
	for _, l := range optionLineRe.FindAllStringSubmatch(listing, -1) {
		letter, name := strings.ToUpper(l[1]), strings.ToLower(l[2])
		if year != "" && !strings.Contains(name, year) {
			continue
		}
		shared := false
		for _, w := range wordRe.FindAllString(name, -1) {
			if strings.Contains(what, w) {
				shared = true
				break
			}
		}
		if !shared {
			continue
		}
		if match != "" && match != letter {
			return "" // ambiguous
		}
		match = letter
	}
	if qty < 1 || match == "" {
		return ""
	}
	return strconv.Itoa(qty) + match
}

// validCodes keeps the codes whose option exists in the listing (all of
// them when the listing's options can't be parsed).
func validCodes(codes string, opts []textutil.Option) string {
	parsed := textutil.ParseOrderCodes(codes)
	if len(opts) == 0 {
		return textutil.FormatOrderCodes(parsed)
	}
	letters := map[string]bool{}
	for _, o := range opts {
		letters[o.Letter] = true
	}
	var kept []textutil.OrderCode
	for _, c := range parsed {
		if letters[c.Option] {
			kept = append(kept, c)
		}
	}
	return textutil.FormatOrderCodes(kept)
}

func joinNotes(notes []db.Candidate) string {
	var parts []string
	for i, n := range notes {
		if i == 3 {
			break
		}
		parts = append(parts, strings.TrimSpace(n.Body))
	}
	return strings.Join(parts, "\n---\n")
}

// FindNamedCustomer looks for a known customer's display name mentioned
// verbatim in body (e.g. the owner writing "Erica Nardi toglie due bottiglie
// ora"). Longest name first, so a name that is a substring of another
// doesn't shadow the fuller match.
func FindNamedCustomer(body string, customers []db.Customer) *db.Customer {
	lower := strings.ToLower(body)
	sorted := append([]db.Customer(nil), customers...)
	sort.SliceStable(sorted, func(i, j int) bool { return len([]rune(sorted[i].Name)) > len([]rune(sorted[j].Name)) })
	for i := range sorted {
		name := strings.TrimSpace(sorted[i].Name)
		if name != "" && strings.Contains(lower, strings.ToLower(name)) {
			return &sorted[i]
		}
	}
	return nil
}

// ResolveAdminOrderNote: a third-person note about a named customer's order.
// Anchors on that customer's latest parseable order anywhere in the chat and
// returns (listing_msg_id, new_code), or ok=false.
func (c *Classifier) ResolveAdminOrderNote(ctx context.Context, body string, targetReplies []db.Candidate) (string, string, bool) {
	for _, r := range targetReplies {
		if r.ListingMsgID == "" {
			continue
		}
		if codes := textutil.ParseOrderCodes(r.Body); len(codes) > 0 {
			resolved := c.LLM.ResolveOrderUpdate(ctx, body, textutil.FormatOrderCodes(codes), "")
			if resolved == "" {
				return "", "", false
			}
			return r.ListingMsgID, resolved, true
		}
	}
	return "", "", false
}
