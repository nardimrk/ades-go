package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"adesgo/internal/db"
	"adesgo/internal/textutil"
)

// Selection is one wine a customer picked in a quotation, parsed from their
// WhatsApp replies ("3A, 2B") against the quotation's lettered options.
type Selection struct {
	Preventivo  string
	QuotationID int64
	DataPrev    string
	Utente      string
	AuthorID    string
	Opzione     string
	Vino        string
	Vintage     int // 0 when unknown
	Qta         int
	Prezzo      float64
	ReplyID     int64 // the reply the selection was read from
}

// Bottles: Qta, times the case size for a case of known size.
func (x Selection) Bottles() int { return bottles(x.Vino, x.Qta) }

type QuotationItem struct {
	QuotationID int64
	Option      string
	WineName    string
	Vintage     int // 0 when unknown
	Quantity    int
	Price       float64
}

func (s *Service) quotationItems(ctx context.Context, where string, args ...any) ([]QuotationItem, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT quotation_id, COALESCE(option,''), wine_name, COALESCE(vintage,0), quantity, price
		FROM quotation_items `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotationItem
	for rows.Next() {
		var it QuotationItem
		if err := rows.Scan(&it.QuotationID, &it.Option, &it.WineName, &it.Vintage, &it.Quantity, &it.Price); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ComputeSelections parses the replies attached to quotation-linked listings
// into per-customer selections. This is the live state shown on Preventivi
// and used by Consegne, independent of saved orders.
func (s *Service) ComputeSelections(ctx context.Context) ([]Selection, error) {
	sel, err := s.parseSelections(ctx)
	if err != nil {
		return nil, err
	}

	// Mentioning the SAME option again later (a corrected code, or a resolved
	// natural-language change) replaces the earlier quantity: keep the last
	// occurrence per (quotation, author, option).
	type okey struct {
		qid    int64
		author string
		opt    string
	}
	last := map[okey]int{}
	for i, x := range sel {
		last[okey{x.QuotationID, x.AuthorID, x.Opzione}] = i
	}
	out := make([]Selection, 0, len(last))
	for i, x := range sel {
		if last[okey{x.QuotationID, x.AuthorID, x.Opzione}] == i {
			out = append(out, x)
		}
	}

	// Whole cases ordered as bottles ("12B", "12x375ml B") get the case price
	// when the option has a case variant of known size ("(cassa da 12…)").
	items, err := s.quotationItems(ctx, "WHERE option LIKE '%"+textutil.CaseSuffix+"'")
	if err != nil {
		return nil, err
	}
	cases := map[string]QuotationItem{}
	for _, it := range items {
		if textutil.ExplicitCaseSize(it.WineName) > 0 {
			cases[strconv.FormatInt(it.QuotationID, 10)+"|"+strings.ToUpper(it.Option)] = it
		}
	}
	if len(cases) == 0 {
		return out, nil
	}
	conv := make([]Selection, 0, len(out))
	for _, x := range out {
		if ci, ok := cases[strconv.FormatInt(x.QuotationID, 10)+"|"+x.Opzione+textutil.CaseSuffix]; ok {
			if n := textutil.ExplicitCaseSize(ci.WineName); x.Qta >= n && x.Qta%n == 0 {
				if ci.Vintage != 0 {
					x.Vintage = ci.Vintage
				}
				x.Opzione, x.Vino, x.Qta, x.Prezzo = x.Opzione+textutil.CaseSuffix, ci.WineName, x.Qta/n, ci.Price
			}
		}
		conv = append(conv, x)
	}
	// a converted order and an explicit "1 cassa B" are now the same option:
	// the later one wins, as above
	last = map[okey]int{}
	for i, x := range conv {
		last[okey{x.QuotationID, x.AuthorID, x.Opzione}] = i
	}
	out = out[:0]
	for i, x := range conv {
		if last[okey{x.QuotationID, x.AuthorID, x.Opzione}] == i {
			out = append(out, x)
		}
	}
	return out, nil
}

// parsedBody is what the order parser reads from one reply body.
type parsedBody struct {
	codes          []textutil.OrderCode
	mentionsQuotNo bool
}

// parseSelections reads every order code of the quotation-linked replies, in
// order, before later mentions replace earlier ones. Parsing every reply is
// the most expensive thing the dashboard does, so the result is cached until
// the DB's selections version changes. The returned slice is shared: callers
// must not modify it.
func (s *Service) parseSelections(ctx context.Context) ([]Selection, error) {
	s.selMu.Lock()
	defer s.selMu.Unlock()
	// read the version before parsing: a write that lands meanwhile bumps it
	// again, so the next call recomputes
	ver, verErr := s.Store.SelectionsVersion(ctx)
	if verErr == nil && s.selOK && ver == s.selVer {
		return s.selCache, nil
	}
	sel, err := s.loadSelections(ctx)
	if err != nil {
		return nil, err
	}
	s.selVer, s.selOK, s.selCache = ver, verErr == nil, sel
	return sel, nil
}

// loadSelections does the work of parseSelections. Reply bodies parsed on a
// previous run are reused, so after a new message only that one is parsed.
// Call with selMu held.
func (s *Service) loadSelections(ctx context.Context) ([]Selection, error) {
	parsed := map[string]parsedBody{} // replaces s.selParsed: drops bodies no longer stored
	parse := func(body string) parsedBody {
		p, ok := parsed[body]
		if !ok {
			if p, ok = s.selParsed[body]; !ok {
				p = parsedBody{textutil.ParseOrders(body), textutil.MentionsQuotationNumber(body)}
			}
			parsed[body] = p
		}
		return p
	}

	rows, err := s.db().QueryContext(ctx, `
		SELECT r.id, COALESCE(u0.merged_into, r.author_id), `+userNameSQL+`, COALESCE(r.msg_id,''), COALESCE(r.body,''), COALESCE(r.order_override,''), COALESCE(r.timestamp,0),
		       q.quotation_number, q.id, COALESCE(q.quotation_date,'')
		FROM replies r
		LEFT JOIN users u0     ON u0.id    = r.author_id
		LEFT JOIN users u      ON u.id     = COALESCE(u0.merged_into, r.author_id)
		LEFT JOIN listings l   ON l.msg_id = r.listing_msg_id
		LEFT JOIN quotations q ON q.id     = l.quotation_id
		WHERE r.listing_msg_id IS NOT NULL AND q.id IS NOT NULL
		ORDER BY q.id, r.timestamp, r.id`)
	if err != nil {
		return nil, err
	}
	type rep struct {
		id                              int64
		authorID, userName, msgID, body string
		ts                              int64
		qnum                            string
		qid                             int64
		qdate                           string
		parsed                          parsedBody
		override                        bool // codes set by hand
	}
	var reps []rep
	for rows.Next() {
		var r rep
		var author, name, qnum sql.NullString
		var override string
		if err := rows.Scan(&r.id, &author, &name, &r.msgID, &r.body, &override, &r.ts, &qnum, &r.qid, &r.qdate); err != nil {
			rows.Close()
			return nil, err
		}
		r.authorID, r.userName, r.qnum = nullStr(author), nullStr(name), nullStr(qnum)
		r.override = override != ""
		if r.override {
			// codes set by hand replace what the rules read in the body
			// ("-" = not an order: parses into nothing)
			r.parsed = parsedBody{codes: textutil.ParseOrders(override)}
		} else {
			r.parsed = parse(r.body)
		}
		reps = append(reps, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The same physical message can be stored twice (real id + synthetic id,
	// possibly with slightly re-formatted text after a WhatsApp edit). Two
	// distinct messages from one customer in the same second are effectively
	// impossible, so keep one per (quotation, author, second): the copy
	// corrected by hand, else the version that parses into the most option
	// codes, and on a tie the one stored under its real WhatsApp id (the
	// synthetic one is the early, possibly pre-edit copy).
	type key struct {
		qid    int64
		author string
		ts     int64
	}
	best := map[key]int{}
	for i, r := range reps {
		k := key{r.qid, r.authorID, r.ts}
		n := len(r.parsed.codes)
		// a copy corrected by hand always wins
		if j, ok := best[k]; !ok || (r.override && !reps[j].override) ||
			(r.override == reps[j].override && (n > len(reps[j].parsed.codes) ||
				(n == len(reps[j].parsed.codes) && realID(r.msgID) && !realID(reps[j].msgID)))) {
			best[k] = i
		}
	}
	var kept []rep
	for i, r := range reps {
		if best[key{r.qid, r.authorID, r.ts}] == i {
			kept = append(kept, r)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].qid != kept[j].qid {
			return kept[i].qid < kept[j].qid
		}
		return kept[i].ts < kept[j].ts
	})

	items, err := s.quotationItems(ctx, "")
	if err != nil {
		return nil, err
	}
	itemBy := map[string]QuotationItem{}
	letters := map[int64]map[string]bool{} // base option letters per quotation
	for _, it := range items {
		opt := strings.ToUpper(it.Option)
		k := strconv.FormatInt(it.QuotationID, 10) + "|" + opt
		if _, ok := itemBy[k]; !ok {
			itemBy[k] = it
		}
		if letters[it.QuotationID] == nil {
			letters[it.QuotationID] = map[string]bool{}
		}
		letters[it.QuotationID][strings.TrimSuffix(opt, textutil.CaseSuffix)] = true
	}
	// itemKey maps an order code to its item key; a case order that doesn't
	// name the option ("1 cassa") applies to the quotation's only option.
	itemKey := func(qid int64, c textutil.OrderCode) string {
		k := c.Key()
		if c.Option == "" && len(letters[qid]) == 1 {
			for l := range letters[qid] {
				k = l + textutil.CaseSuffix
			}
		}
		return k
	}

	var sel []Selection
	for _, r := range kept {
		if r.parsed.mentionsQuotNo {
			continue
		}
		for _, c := range r.parsed.codes {
			if c.Qty >= 1900 {
				continue
			}
			k := itemKey(r.qid, c)
			it, ok := itemBy[strconv.FormatInt(r.qid, 10)+"|"+k]
			if !ok {
				continue
			}
			sel = append(sel, Selection{
				Preventivo: r.qnum, QuotationID: r.qid, DataPrev: r.qdate,
				Utente: FmtName(r.userName), AuthorID: r.authorID,
				Opzione: k, Vino: it.WineName, Vintage: it.Vintage, Qta: c.Qty, Prezzo: it.Price,
				ReplyID: r.id,
			})
		}
	}
	s.selParsed = parsed
	return sel, nil
}

// realID reports a serialized WhatsApp id ("false_<chat>_<id>_<author>")
// as opposed to a synthetic fallback id.
func realID(msgID string) bool {
	return strings.HasPrefix(msgID, "false_") || strings.HasPrefix(msgID, "true_")
}

// ── import quotations from listings ──────────────────────────────────────────

// NextOrderNumber is the number a new order dated date ("2026-10-04")
// would get: "ORD260556" (see db.Code).
func (s *Service) NextOrderNumber(ctx context.Context, date string) string {
	n, err := db.NextOrderNumber(ctx, s.db(), date)
	if err != nil {
		return ""
	}
	return n
}

// ImportPreventivi creates a quotation for every campaign whose listings
// have parseable options and no quotation yet. ok=false when there are no
// listings at all.
func (s *Service) ImportPreventivi(ctx context.Context) (created, skipped int, ok bool, err error) {
	rows, err := s.db().QueryContext(ctx, `
		SELECT l.msg_id, COALESCE(l.body,''), COALESCE(l.timestamp,0),
		       CASE WHEN q.id IS NOT NULL THEN l.quotation_id ELSE NULL END
		FROM listings l LEFT JOIN quotations q ON q.id = l.quotation_id
		ORDER BY l.timestamp ASC, l.id ASC`)
	if err != nil {
		return 0, 0, false, err
	}
	type lrow struct {
		msgID, body string
		ts          int64
		hasQ        bool
	}
	var order []string
	groups := map[string][]lrow{}
	any := false
	for rows.Next() {
		var r lrow
		var qid sql.NullInt64
		if err := rows.Scan(&r.msgID, &r.body, &r.ts, &qid); err != nil {
			rows.Close()
			return 0, 0, false, err
		}
		any = true
		r.hasQ = qid.Valid
		if !textutil.HasOptions(r.body) {
			continue
		}
		k := textutil.CampaignKey(r.body)
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	rows.Close()
	if !any {
		return 0, 0, false, nil
	}

	for _, k := range order {
		grp := groups[k]
		hasQ := false
		minTS := grp[0].ts
		for _, r := range grp {
			hasQ = hasQ || r.hasQ
			if r.ts < minTS {
				minTS = r.ts
			}
		}
		if hasQ {
			skipped++
			continue
		}
		// listings since noQtyOptionsSince: every option, with or without a
		// bottle count; older ones keep the original rule (counted only)
		opts := textutil.ParseOptionsWithQty(grp[0].body)
		if minTS >= noQtyOptionsSince.Unix() {
			opts = textutil.ParseOptions(grp[0].body)
		}
		if len(opts) == 0 {
			skipped++
			continue
		}
		msgIDs := make([]string, len(grp))
		for i, r := range grp {
			msgIDs[i] = r.msgID
		}
		opts = textutil.WithCaseOptions(grp[0].body, opts)
		if _, err := s.CreateImportedQuotation(ctx, msgIDs, minTS, opts); err != nil {
			return created, skipped, true, err
		}
		created++
	}
	return created, skipped, true, nil
}

// noQtyOptionsSince: listings whose options have no bottle count ("A. Riesling
// a 14,50€ (CASSA INTERA a 78€)") get a quotation automatically only if posted
// from this date. Older ones were left out on purpose (decided with the
// user on 2026-10-01); Thanisch Riesling Secchi was created by hand.
var noQtyOptionsSince = time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)

// CreateImportedQuotation creates an order (ORDyynnnn) with these options and
// links the given listings to it. Returns the quotation number.
func (s *Service) CreateImportedQuotation(ctx context.Context, msgIDs []string, ts int64, opts []textutil.Option) (string, error) {
	var num string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		date := time.Unix(ts, 0).Format("2006-01-02")
		var err error
		if num, err = db.NextOrderNumber(ctx, tx, date); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO quotations (quotation_number, quotation_date) VALUES (?, ?)", num, date)
		if err != nil {
			return err
		}
		qid, _ := res.LastInsertId()
		// the delivery estimate set on the campaign in Inserzioni comes along
		if len(msgIDs) > 0 {
			ph := strings.TrimSuffix(strings.Repeat("?,", len(msgIDs)), ",")
			args := make([]any, 0, len(msgIDs)+1)
			for _, id := range msgIDs {
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE quotations SET consegna_stimata =
				(SELECT consegna_stimata FROM listings WHERE msg_id IN (`+ph+`) AND COALESCE(consegna_stimata,'') <> ''
				 ORDER BY timestamp DESC LIMIT 1) WHERE id = ?`, append(args, qid)...); err != nil {
				return err
			}
		}
		for _, o := range opts {
			if _, err := tx.ExecContext(ctx, "INSERT INTO quotation_items (quotation_id, option, wine_name, quantity, price) VALUES (?,?,?,?,?)",
				qid, o.Code(), o.WineName, o.Quantity, o.Price); err != nil {
				return err
			}
		}
		for _, id := range msgIDs {
			if _, err := tx.ExecContext(ctx, "UPDATE listings SET quotation_id = ? WHERE msg_id = ?", qid, id); err != nil {
				return err
			}
		}
		return nil
	})
	return num, err
}

func (s *Service) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ── summary list ─────────────────────────────────────────────────────────────

type Quotation struct {
	ID               int64
	Number           string
	Date             string
	ManualClientID   string
	ManualClientName string
}

func (s *Service) Quotations(ctx context.Context) ([]Quotation, error) {
	rows, err := s.db().QueryContext(ctx, `
		SELECT id, COALESCE(quotation_number,''), COALESCE(quotation_date,''),
		       COALESCE(manual_client_id,''), COALESCE(manual_client_name,'')
		FROM quotations ORDER BY quotation_date DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Quotation
	for rows.Next() {
		var q Quotation
		if err := rows.Scan(&q.ID, &q.Number, &q.Date, &q.ManualClientID, &q.ManualClientName); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Service) QuotationByNumber(ctx context.Context, num string) (*Quotation, error) {
	var q Quotation
	err := s.db().QueryRowContext(ctx, `
		SELECT id, COALESCE(quotation_number,''), COALESCE(quotation_date,''),
		       COALESCE(manual_client_id,''), COALESCE(manual_client_name,'')
		FROM quotations WHERE quotation_number = ?`, num).Scan(&q.ID, &q.Number, &q.Date, &q.ManualClientID, &q.ManualClientName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &q, err
}

type QuotationSummary struct {
	ID        int64
	Title     string // linked listing's title; for a manual order, the customer's name
	Manual    bool   // created by hand (no linked listing)
	Number    string
	Date      string
	Clienti   int
	Bottiglie int
	Totale    float64
	SavedOn   string // when every customer's order is confirmed: the latest order_date; "" otherwise
}

// sectionCustomers lists the customers that get a section on the order page
// (QuotationDetail), in order of first appearance: everyone with a quantity,
// plus the manual client of a manual order when it has lines.
func sectionCustomers(q Quotation, qsel []Selection, hasItems bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range qsel {
		if strings.TrimSpace(x.Utente) == "" || x.Qta <= 0 || seen[x.Utente] {
			continue
		}
		seen[x.Utente] = true
		out = append(out, x.Utente)
	}
	if m := q.ManualClientName; m != "" && !seen[m] && hasItems {
		out = append(out, m)
	}
	return out
}

func (s *Service) loadQuotationSummaries(ctx context.Context) ([]QuotationSummary, error) {
	quots, err := s.Quotations(ctx)
	if err != nil {
		return nil, err
	}
	sel, err := s.ComputeSelections(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.quotationItems(ctx, "")
	if err != nil {
		return nil, err
	}
	// saved orders per quotation, by customer id and by name (as SavedOrderDate)
	type savedKey struct{ num, who string }
	savedUID, savedName := map[savedKey]string{}, map[savedKey]string{}
	rows, err := s.db().QueryContext(ctx, "SELECT COALESCE(quotation_number,''), COALESCE(user_id,''), COALESCE(user_name,''), COALESCE(order_date,'') FROM orders ORDER BY id")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n, uid, name, d string
		if rows.Scan(&n, &uid, &name, &d) == nil {
			if _, ok := savedUID[savedKey{n, uid}]; !ok && uid != "" {
				savedUID[savedKey{n, uid}] = d
			}
			if _, ok := savedName[savedKey{n, name}]; !ok {
				savedName[savedKey{n, name}] = d
			}
		}
	}
	rows.Close()
	byName, _, err := s.ClientOptions(ctx)
	if err != nil {
		return nil, err
	}
	// allSaved: the latest confirmation date when every customer section of
	// the order page is confirmed, "" when one is not (or there is none)
	allSaved := func(q Quotation, qsel []Selection, hasItems bool) string {
		customers := sectionCustomers(q, qsel, hasItems)
		latest := ""
		for _, utente := range customers {
			cliente := s.DefaultCliente(&q, utente, len(customers) == 1)
			var d string
			var ok bool
			if uid := byName[cliente]; uid != "" {
				d, ok = savedUID[savedKey{q.Number, uid}]
			} else {
				d, ok = savedName[savedKey{q.Number, cliente}]
			}
			if !ok {
				return ""
			}
			if d > latest {
				latest = d
			}
		}
		return latest
	}

	// title of the listing (campaign) each quotation was created from
	titles := map[int64]string{}
	if lrows, err := s.ListingRows(ctx); err == nil {
		for _, c := range GroupCampaigns(lrows) {
			for _, qid := range c.QuotationIDs {
				if _, ok := titles[qid]; !ok {
					titles[qid] = c.DisplayTitle
				}
			}
		}
	}

	selBy := map[string][]Selection{}
	for _, x := range sel {
		selBy[x.Preventivo] = append(selBy[x.Preventivo], x)
	}
	itemsBy := map[int64][]QuotationItem{}
	for _, it := range items {
		itemsBy[it.QuotationID] = append(itemsBy[it.QuotationID], it)
	}

	var out []QuotationSummary
	for _, q := range quots {
		sum := QuotationSummary{ID: q.ID, Number: q.Number, Date: q.Date, Title: titles[q.ID],
			SavedOn: allSaved(q, selBy[q.Number], len(itemsBy[q.ID]) > 0)}
		if sum.Title == "" {
			sum.Manual = true
			sum.Title = textutil.StripEmoji(q.ManualClientName)
			if sum.Title == "" {
				sum.Title = "Ordine manuale"
			}
		}
		clienti := map[string]bool{}
		if q.ManualClientName != "" {
			clienti[q.ManualClientName] = true
		}
		if qs := selBy[q.Number]; len(qs) > 0 {
			for _, x := range qs {
				clienti[x.Utente] = true
				if x.Qta > 0 {
					sum.Bottiglie += x.Bottles()
					sum.Totale += float64(x.Qta) * x.Prezzo
				}
			}
		} else {
			for _, it := range itemsBy[q.ID] {
				sum.Bottiglie += it.Quantity
				sum.Totale += float64(it.Quantity) * it.Price
			}
		}
		sum.Clienti = len(clienti)
		sum.Totale = round2(sum.Totale)
		out = append(out, sum)
	}
	return out, nil
}

// QuotationMonth groups the orders of one month (by order date).
type QuotationMonth struct {
	Key       string // "2026-09"
	Year      int
	Month     int
	Rows      []QuotationSummary
	Bottiglie int
	Totale    float64
}

// QuotationMonths groups summaries (already newest first) by month.
func QuotationMonths(rows []QuotationSummary) []QuotationMonth {
	var out []QuotationMonth
	idx := map[string]int{}
	for _, r := range rows {
		key := ""
		if len(r.Date) >= 7 {
			key = r.Date[:7]
		}
		i, ok := idx[key]
		if !ok {
			i = len(out)
			idx[key] = i
			m := QuotationMonth{Key: key}
			if t, err := time.Parse("2006-01", key); err == nil {
				m.Year, m.Month = t.Year(), int(t.Month())
			}
			out = append(out, m)
		}
		out[i].Rows = append(out[i].Rows, r)
		if r.Clienti > 0 { // without customers the figures are the offer's stock, not orders
			out[i].Bottiglie += r.Bottiglie
			out[i].Totale = round2(out[i].Totale + r.Totale)
		}
	}
	return out
}

// SearchableSelections: the chat selections plus the lines of manual orders
// (customer set by hand, wines in quotation_items), as QuotationDetail shows
// them, so Ordini search finds orders created without a WhatsApp reply.
func (s *Service) SearchableSelections(ctx context.Context) ([]Selection, error) {
	sel, err := s.ComputeSelections(ctx)
	if err != nil {
		return nil, err
	}
	quots, err := s.Quotations(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.quotationItems(ctx, "")
	if err != nil {
		return nil, err
	}
	itemsBy := map[int64][]QuotationItem{}
	for _, it := range items {
		itemsBy[it.QuotationID] = append(itemsBy[it.QuotationID], it)
	}
	fromChat := map[string]bool{} // quotation|customer with chat selections
	for _, x := range sel {
		if x.Qta > 0 {
			fromChat[x.Preventivo+"|"+x.Utente] = true
		}
	}
	for _, q := range quots {
		m := q.ManualClientName
		if m == "" || fromChat[q.Number+"|"+m] {
			continue
		}
		for _, it := range itemsBy[q.ID] {
			sel = append(sel, Selection{Preventivo: q.Number, QuotationID: q.ID, DataPrev: q.Date, Utente: m,
				AuthorID: q.ManualClientID, Opzione: it.Option, Vino: it.WineName, Vintage: it.Vintage, Qta: it.Quantity, Prezzo: it.Price})
		}
	}
	return sel, nil
}

// SearchSelections filters selections by customer or wine, newest first.
// A query that is exactly a customer's name (e.g. "Ale", from the most
// active customers) keeps only that customer, not "Alessio" or wines.
func SearchSelections(sel []Selection, q string) []Selection {
	q = strings.ToLower(strings.TrimSpace(q))
	exact := false
	for _, x := range sel {
		if strings.EqualFold(strings.TrimSpace(x.Utente), q) {
			exact = true
			break
		}
	}
	var out []Selection
	for _, x := range sel {
		if exact {
			if strings.EqualFold(strings.TrimSpace(x.Utente), q) {
				out = append(out, x)
			}
		} else if strings.Contains(strings.ToLower(x.Utente), q) || strings.Contains(strings.ToLower(x.Vino), q) {
			out = append(out, x)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DataPrev > out[j].DataPrev })
	return out
}

// YearTotal: what one customer ordered in a year.
type YearTotal struct {
	Year      string
	Ordini    int
	Bottiglie int
	Totale    float64
}

// CustomerYearTotals: when the search results all belong to one customer,
// their name and their orders summed by year (newest first) plus the overall
// total; "" when the results name several customers (or none).
func CustomerYearTotals(rows []Selection) (string, []YearTotal, YearTotal) {
	name := ""
	for _, x := range rows {
		if name == "" {
			name = x.Utente
		} else if !strings.EqualFold(name, x.Utente) {
			return "", nil, YearTotal{}
		}
	}
	if name == "" {
		return "", nil, YearTotal{}
	}
	by := map[string]*YearTotal{}
	orders := map[string]bool{}
	all := YearTotal{Year: "Totale"}
	for _, x := range rows {
		if x.Qta <= 0 {
			continue
		}
		y := x.DataPrev
		if len(y) >= 4 {
			y = y[:4]
		}
		t := by[y]
		if t == nil {
			t = &YearTotal{Year: y}
			by[y] = t
		}
		if !orders[x.Preventivo] {
			orders[x.Preventivo] = true
			t.Ordini++
			all.Ordini++
		}
		b, v := x.Bottles(), float64(x.Qta)*x.Prezzo
		t.Bottiglie += b
		t.Totale += v
		all.Bottiglie += b
		all.Totale += v
	}
	out := make([]YearTotal, 0, len(by))
	for _, t := range by {
		t.Totale = round2(t.Totale)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Year > out[j].Year })
	all.Totale = round2(all.Totale)
	return name, out, all
}

// ── detail view ──────────────────────────────────────────────────────────────

type OrderRow struct {
	Opzione string
	Vino    string
	Qta     int
	Prezzo  float64
}

func (r OrderRow) Totale() float64 { return round2(float64(r.Qta) * r.Prezzo) }

type CustomerSection struct {
	Utente  string // name detected from WhatsApp replies
	Cliente string // effective customer (may be overridden)
	Options []string
	Rows    []OrderRow
	SavedOn string
	// manual order (no listing): each wine has its estimated delivery,
	// by option ("A" → "2026-11-15")
	Manual      bool
	QuotationID int64
	Consegne    map[string]ItemDelivery
	// the customer's place in Clienti ("" = not set), shown and filtered on
	Citta     string
	Provincia string
	// unix time of the customer's first reply read as an order (0 = unknown)
	OrderedAt int64
}

// Place is "Arzignano (Vicenza)", "Arzignano", "Vicenza" or "".
func (c CustomerSection) Place() string {
	switch {
	case c.Citta != "" && c.Provincia != "" && !strings.EqualFold(c.Citta, c.Provincia):
		return c.Citta + " (" + c.Provincia + ")"
	case c.Citta != "":
		return c.Citta
	}
	return c.Provincia
}

func (c CustomerSection) Total() float64 {
	t := 0.0
	for _, r := range c.Rows {
		t += r.Totale()
	}
	return t
}

type QuotationDetail struct {
	Quotation Quotation
	Date      string
	Sections  []CustomerSection
	Single    bool
	Stock     []OptionStock // per option: ordered, still available
	Linked    []LinkedLine  // wines of manual orders connected to this order's options
	// set by the page: the neighbours in the Ordini list and the listing
	Prev, Next   *QuotationSummary
	ListingURL   string // "" = manual order (no listing)
	ListingTitle string
}

// QuotationNeighbors returns the orders shown just before and after num in
// the Ordini list (same order as QuotationMonths); nil at either end.
func QuotationNeighbors(sums []QuotationSummary, num string) (prev, next *QuotationSummary) {
	var sorted []QuotationSummary
	for _, m := range QuotationMonths(sums) {
		sorted = append(sorted, m.Rows...)
	}
	for i := range sorted {
		if sorted[i].Number != num {
			continue
		}
		if i > 0 {
			prev = &sorted[i-1]
		}
		if i+1 < len(sorted) {
			next = &sorted[i+1]
		}
		break
	}
	return prev, next
}

// QuotationListing returns the campaign (listing) the quotation was created
// from, nil for a manual order.
func (s *Service) QuotationListing(ctx context.Context, quotationID int64) (*Campaign, error) {
	rows, err := s.ListingRows(ctx)
	if err != nil {
		return nil, err
	}
	// campaigns are grouped within one chat, as FindCampaign does
	byChat := map[string][]ListingRow{}
	for _, r := range rows {
		if r.QuotationID != nil && *r.QuotationID == quotationID {
			byChat[r.ChatID] = nil
		}
	}
	for _, r := range rows {
		if _, ok := byChat[r.ChatID]; ok {
			byChat[r.ChatID] = append(byChat[r.ChatID], r)
		}
	}
	for _, chatRows := range byChat {
		for _, c := range GroupCampaigns(chatRows) {
			for _, id := range c.QuotationIDs {
				if id == quotationID {
					c := c
					return &c, nil
				}
			}
		}
	}
	return nil, nil
}

// OptionStock is one wine option of the order: the bottles ordered and,
// when the listing gives a count, how many are left of it.
type OptionStock struct {
	Letter string
	Wine   string
	// the item edited by "Cassa" on the order page: the option's case
	// variant ("A-CASSA") if there is one, else the option itself
	Code     string
	CaseSize int     // bottles per case, 0 = sold by the bottle
	Price    float64 // that item's price (per case for a case)
	Ordered  int     // bottles ordered by everyone, at any time (cases count their bottles)
	HasCount bool    // the listing states how many bottles it offers
	Offered  int     // the initial quantity of the listing
}

// Remaining is what is left: offered minus ordered (negative = ordered
// beyond availability).
func (o OptionStock) Remaining() int { return o.Offered - o.Ordered }

// Excess is how many bottles were ordered beyond availability (0 if none).
func (o OptionStock) Excess() int {
	if !o.HasCount || o.Remaining() >= 0 {
		return 0
	}
	return -o.Remaining()
}

// optionStock sums, for each option, every bottle ordered (no matter when)
// and compares it with the initial quantity of the order's first listing
// (as first posted, or as corrected on the listing page).
func (s *Service) optionStock(ctx context.Context, quotationID int64, qsel []Selection) ([]OptionStock, error) {
	type stock struct {
		wine  string
		count int
	}
	first := map[string]stock{}
	ids, err := s.postIDs(ctx, "SELECT msg_id FROM listings WHERE quotation_id = ? ORDER BY created_at, id", quotationID)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		opts, err := s.initialOptions(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, o := range opts {
			if o.HasQty {
				first[o.Letter] = stock{o.Wine, o.Qty}
			}
		}
	}
	if len(first) == 0 {
		return nil, nil // no listing count to compare with (e.g. a manual order)
	}
	// the options of the order, named as in the order
	names := map[string]string{}
	letters := map[string]bool{}
	edit := map[string]QuotationItem{} // the case variant wins over the option
	plainCase := map[string]int{}      // options sold as whole cases ("30 x cassa da 12 …")
	items, err := s.quotationItems(ctx, "WHERE quotation_id = ?", quotationID)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		code := strings.ToUpper(it.Option)
		letter, isCase := strings.CutSuffix(code, textutil.CaseSuffix)
		if len(letter) != 1 {
			continue
		}
		letters[letter] = true
		if !isCase {
			names[letter] = wineWithVintage(it.WineName, it.Vintage)
			plainCase[letter] = textutil.CaseSizeFromName(it.WineName)
		}
		if _, has := edit[letter]; isCase || !has {
			edit[letter] = it
		}
	}
	for l := range first {
		letters[l] = true
	}
	total := map[string]int{}
	for _, x := range qsel {
		if x.Qta > 0 {
			letter, _ := strings.CutSuffix(x.Opzione, textutil.CaseSuffix)
			total[letter] += x.Bottles()
		}
	}
	// wines of manual orders connected to this order's options
	lrows, err := s.db().QueryContext(ctx, `SELECT UPPER(COALESCE(linked_option,'')), COALESCE(wine_name,''), quantity
		FROM quotation_items WHERE linked_quotation_id = ?`, quotationID)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var letter, wine string
		var qty int
		if lrows.Scan(&letter, &wine, &qty) == nil && qty > 0 {
			total[letter] += bottles(wine, qty)
			letters[letter] = true
		}
	}
	lrows.Close()
	keys := make([]string, 0, len(letters))
	for l := range letters {
		keys = append(keys, l)
	}
	sort.Strings(keys)
	var out []OptionStock
	for _, l := range keys {
		o := OptionStock{Letter: l, Wine: names[l], Ordered: total[l]}
		if it, ok := edit[l]; ok {
			o.Code, o.CaseSize, o.Price = strings.ToUpper(it.Option), textutil.CaseSizeFromName(it.WineName), it.Price
		}
		if st, ok := first[l]; ok {
			o.HasCount, o.Offered = true, st.count
			if n := plainCase[l]; n > 0 {
				o.Offered *= n // the listing counts cases, the orders bottles
			}
			if o.Wine == "" {
				o.Wine = st.wine
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// ClientOptions maps display name → user id for every non-group user.
func (s *Service) ClientOptions(ctx context.Context) (map[string]string, []string, error) {
	// customers only: no merged duplicates, no sellers, no groups
	rows, err := s.db().QueryContext(ctx, "SELECT id, COALESCE(name,'') FROM users WHERE id NOT LIKE '%@g.us' AND merged_into IS NULL AND COALESCE(is_seller,0) = 0 ORDER BY name")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byName := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, nil, err
		}
		name = strings.TrimSpace(textutil.StripEmoji(name))
		display := name
		if name == "" || strings.Contains(name, "@") {
			display = FmtName(id)
		}
		byName[display] = id
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return byName, names, rows.Err()
}

// SavedOrderDate returns the date of the saved order for this customer.
func (s *Service) SavedOrderDate(ctx context.Context, quotNum, clientName, clientUID string) string {
	var d string
	if clientUID != "" {
		s.db().QueryRowContext(ctx, "SELECT COALESCE(order_date,'') FROM orders WHERE quotation_number = ? AND user_id = ? ORDER BY id LIMIT 1", quotNum, clientUID).Scan(&d)
	} else {
		s.db().QueryRowContext(ctx, "SELECT COALESCE(order_date,'') FROM orders WHERE quotation_number = ? AND user_name = ? ORDER BY id LIMIT 1", quotNum, clientName).Scan(&d)
	}
	return d
}

func (s *Service) QuotationDetail(ctx context.Context, num string) (*QuotationDetail, error) {
	q, err := s.QuotationByNumber(ctx, num)
	if err != nil || q == nil {
		return nil, err
	}
	all, err := s.ComputeSelections(ctx)
	if err != nil {
		return nil, err
	}
	var qsel []Selection
	for _, x := range all {
		if x.Preventivo == num {
			qsel = append(qsel, x)
		}
	}
	d := &QuotationDetail{Quotation: *q, Date: q.Date}
	if len(qsel) > 0 && qsel[0].DataPrev != "" {
		d.Date = qsel[0].DataPrev
	}

	// rows per detected customer, in order of first appearance
	var order []string
	rowsBy := map[string][]OrderRow{}
	for _, x := range qsel {
		if strings.TrimSpace(x.Utente) == "" || x.Qta <= 0 {
			continue
		}
		if _, ok := rowsBy[x.Utente]; !ok {
			order = append(order, x.Utente)
		}
		rowsBy[x.Utente] = append(rowsBy[x.Utente], OrderRow{Opzione: x.Opzione, Vino: x.Vino, Qta: x.Qta, Prezzo: x.Prezzo})
	}
	if m := q.ManualClientName; m != "" {
		if _, ok := rowsBy[m]; !ok {
			items, err := s.quotationItems(ctx, "WHERE quotation_id = ?", q.ID)
			if err != nil {
				return nil, err
			}
			if len(items) > 0 {
				order = append(order, m)
				for _, it := range items {
					rowsBy[m] = append(rowsBy[m], OrderRow{Opzione: it.Option, Vino: it.WineName, Qta: it.Quantity, Prezzo: it.Price})
				}
			}
		}
	}

	// The manual client override is a single field on the quotation, so it
	// only stands in as default when there is exactly one customer section.
	if d.Stock, err = s.optionStock(ctx, q.ID, qsel); err != nil {
		return nil, err
	}
	if d.Linked, err = s.linkedLines(ctx, q.ID); err != nil {
		return nil, err
	}
	firstReply := map[string]int64{}
	for _, x := range qsel {
		if x.ReplyID > 0 && (firstReply[x.Utente] == 0 || x.ReplyID < firstReply[x.Utente]) {
			firstReply[x.Utente] = x.ReplyID
		}
	}
	replyTS, err := s.replyTimes(ctx, firstReply)
	if err != nil {
		return nil, err
	}
	d.Single = len(order) == 1
	for _, utente := range order {
		sec, err := s.BuildSection(ctx, q, utente, s.DefaultCliente(q, utente, d.Single), rowsBy[utente])
		if err != nil {
			return nil, err
		}
		sec.OrderedAt = replyTS[firstReply[utente]]
		if sec.OrderedAt == 0 && utente == q.ManualClientName {
			// an order entered by hand: no reply, the time it was created
			created, err := s.quotationTimes(ctx, []int64{q.ID})
			if err != nil {
				return nil, err
			}
			sec.OrderedAt = created[q.ID]
		}
		d.Sections = append(d.Sections, sec)
	}
	return d, nil
}

// replyTimes returns the timestamp of the given replies, by reply id.
func (s *Service) replyTimes(ctx context.Context, ids map[string]int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db().QueryContext(ctx, "SELECT id, COALESCE(timestamp,0) FROM replies WHERE id IN (?"+strings.Repeat(",?", len(args)-1)+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, t int64
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// quotationTimes returns when the given quotations were created (unix
// time, by id); created_at is stored in UTC by SQLite.
func (s *Service) quotationTimes(ctx context.Context, ids []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db().QueryContext(ctx, "SELECT id, COALESCE(created_at,'') FROM quotations WHERE id IN (?"+strings.Repeat(",?", len(args)-1)+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c string
		if err := rows.Scan(&id, &c); err != nil {
			return nil, err
		}
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", c, time.UTC); err == nil {
			out[id] = t.Unix()
		}
	}
	return out, rows.Err()
}

// LinkedLine: a wine of a manual order connected to an option of a
// listing's order (counted in that option's stock, delivered with it).
type LinkedLine struct {
	Order   string // the manual order, "IMP0553"
	Cliente string
	Opzione string // the listing order's option it is connected to
	Vino    string
	Qta     int
	Prezzo  float64
	At      int64 // unix time the manual order was created
}

func (l LinkedLine) Totale() float64 { return float64(l.Qta) * l.Prezzo }

// LinkedOrder: the linked wines of one manual order (one customer).
type LinkedOrder struct {
	Order   string
	Cliente string
	At      int64 // unix time the manual order was created (0 = unknown)
	Lines   []LinkedLine
}

func (o LinkedOrder) Totale() float64 {
	t := 0.0
	for _, l := range o.Lines {
		t += l.Totale()
	}
	return t
}

// GroupLinked groups linked wines by manual order, in order of appearance.
func GroupLinked(lines []LinkedLine) []LinkedOrder {
	var out []LinkedOrder
	idx := map[string]int{}
	for _, l := range lines {
		i, ok := idx[l.Order]
		if !ok {
			i = len(out)
			idx[l.Order] = i
			out = append(out, LinkedOrder{Order: l.Order, Cliente: l.Cliente, At: l.At})
		}
		out[i].Lines = append(out[i].Lines, l)
	}
	return out
}

// linkedLines returns the wines of manual orders connected to quotationID,
// by option then order, read from the same lines as Consegne.
func (s *Service) linkedLines(ctx context.Context, quotationID int64) ([]LinkedLine, error) {
	type itemKey struct {
		q   int64
		opt string
	}
	linkedOpt := map[itemKey]string{}
	rows, err := s.db().QueryContext(ctx, `SELECT quotation_id, UPPER(COALESCE(option,'')), UPPER(COALESCE(linked_option,''))
		FROM quotation_items WHERE linked_quotation_id = ?`, quotationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var q int64
		var opt, lopt string
		if rows.Scan(&q, &opt, &lopt) == nil {
			linkedOpt[itemKey{q, opt}] = lopt
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(linkedOpt) == 0 {
		return nil, nil
	}
	all, err := s.deliverySelections(ctx)
	if err != nil {
		return nil, err
	}
	qids := make([]int64, 0, len(linkedOpt))
	for k := range linkedOpt {
		qids = append(qids, k.q)
	}
	created, err := s.quotationTimes(ctx, qids)
	if err != nil {
		return nil, err
	}
	var out []LinkedLine
	for _, x := range all {
		lopt, ok := linkedOpt[itemKey{x.QuotationID, strings.ToUpper(x.Opzione)}]
		if !ok || x.Qta <= 0 || x.QuotationID == quotationID {
			continue
		}
		out = append(out, LinkedLine{Order: x.Preventivo, Cliente: x.Utente, Opzione: lopt,
			Vino: wineWithVintage(x.Vino, x.Vintage), Qta: x.Qta, Prezzo: x.Prezzo, At: created[x.QuotationID]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Opzione != out[j].Opzione {
			return out[i].Opzione < out[j].Opzione
		}
		return out[i].Order < out[j].Order
	})
	return out, nil
}

func (s *Service) DefaultCliente(q *Quotation, utente string, single bool) string {
	if single && q.ManualClientName != "" {
		return q.ManualClientName
	}
	return utente
}

// BuildSection assembles a customer section for an effective client name.
func (s *Service) BuildSection(ctx context.Context, q *Quotation, utente, cliente string, rows []OrderRow) (CustomerSection, error) {
	byName, names, err := s.ClientOptions(ctx)
	if err != nil {
		return CustomerSection{}, err
	}
	sec := CustomerSection{Utente: utente, Cliente: cliente, Rows: rows, QuotationID: q.ID}
	if sec.Manual, sec.Consegne, err = s.manualConsegne(ctx, q.ID); err != nil {
		return CustomerSection{}, err
	}
	seen := map[string]bool{}
	for _, c := range append([]string{utente, cliente}, names...) {
		if !seen[c] {
			seen[c] = true
			sec.Options = append(sec.Options, c)
		}
	}
	sec.SavedOn = s.SavedOrderDate(ctx, q.Number, cliente, byName[cliente])
	locs, err := s.customerLocations(ctx)
	if err != nil {
		return CustomerSection{}, err
	}
	loc, ok := locs.byID[byName[cliente]]
	if !ok {
		loc = locs.byName[cliente]
	}
	sec.Citta, sec.Provincia = loc.citta, loc.provincia
	return sec, nil
}

// SavedOrderRows returns the lines of this customer's saved order.
func (s *Service) SavedOrderRows(ctx context.Context, quotNum, clientName string) ([]OrderRow, error) {
	byName, _, err := s.ClientOptions(ctx)
	if err != nil {
		return nil, err
	}
	var oid int64
	if uid, ok := byName[clientName]; ok {
		err = s.db().QueryRowContext(ctx, "SELECT id FROM orders WHERE quotation_number = ? AND user_id = ? ORDER BY id LIMIT 1", quotNum, uid).Scan(&oid)
	} else {
		err = s.db().QueryRowContext(ctx, "SELECT id FROM orders WHERE quotation_number = ? AND user_name = ? ORDER BY id LIMIT 1", quotNum, clientName).Scan(&oid)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	rows, err := s.db().QueryContext(ctx, "SELECT COALESCE(option,''), COALESCE(wine_name,''), quantity, price FROM order_items WHERE order_id = ? ORDER BY id", oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrderRow
	for rows.Next() {
		var r OrderRow
		if err := rows.Scan(&r.Opzione, &r.Vino, &r.Qta, &r.Prezzo); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetManualClient persists the customer override chosen on a section.
func (s *Service) SetManualClient(ctx context.Context, quotNum, name string) error {
	byName, _, err := s.ClientOptions(ctx)
	if err != nil {
		return err
	}
	var uid any
	if id, ok := byName[name]; ok {
		uid = id
	}
	_, err = s.db().ExecContext(ctx, "UPDATE quotations SET manual_client_id = ?, manual_client_name = ? WHERE quotation_number = ?", uid, name, quotNum)
	return err
}

// SaveOrder replaces this customer's order for the quotation.
func (s *Service) SaveOrder(ctx context.Context, quotNum, clientName string, rows []OrderRow) error {
	byName, _, err := s.ClientOptions(ctx)
	if err != nil {
		return err
	}
	uid, hasUID := byName[clientName]
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var oldID int64
		var err error
		if hasUID {
			err = tx.QueryRowContext(ctx, "SELECT id FROM orders WHERE quotation_number = ? AND user_id = ?", quotNum, uid).Scan(&oldID)
		} else {
			err = tx.QueryRowContext(ctx, "SELECT id FROM orders WHERE quotation_number = ? AND user_name = ?", quotNum, clientName).Scan(&oldID)
		}
		if err == nil {
			if _, err := tx.ExecContext(ctx, "DELETE FROM order_items WHERE order_id = ?", oldID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM orders WHERE id = ?", oldID); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var uidArg any
		if hasUID {
			uidArg = uid
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO orders (quotation_number, user_name, user_id) VALUES (?, ?, ?)", quotNum, clientName, uidArg)
		if err != nil {
			return err
		}
		oid, _ := res.LastInsertId()
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO order_items (order_id, user_name, option, wine_name, quantity, price, total)
				VALUES (?,?,?,?,?,?,?)`, oid, clientName, r.Opzione, r.Vino, r.Qta, r.Prezzo, r.Totale()); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteSavedOrder removes a customer's confirmed order ("Riapri"): the
// section goes back to what the replies say, to be confirmed again.
func (s *Service) DeleteSavedOrder(ctx context.Context, quotNum, clientName string) error {
	byName, _, err := s.ClientOptions(ctx)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var ids []int64
		q, args := "SELECT id FROM orders WHERE quotation_number = ? AND user_name = ?", []any{quotNum, clientName}
		if uid, ok := byName[clientName]; ok {
			q, args = "SELECT id FROM orders WHERE quotation_number = ? AND (user_id = ? OR (user_id IS NULL AND user_name = ?))", []any{quotNum, uid, clientName}
		}
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 0 {
			return fmt.Errorf("nessun ordine confermato per %s", clientName)
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, "DELETE FROM order_items WHERE order_id = ?", id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM orders WHERE id = ?", id); err != nil {
				return err
			}
		}
		return nil
	})
}

// caseNameRe: the case part of an option's name, "(cassa da 6…)" or
// "(cassa intera)".
var caseNameRe = regexp.MustCompile(`(?i)\s*\((?:cassa da \d+[^)]*|cassa intera)\)`)

// SetOptionCase sets how an option of an order is sold: as a whole case of
// size bottles (its name says "(cassa da N)") at price per case, or by the
// bottle (size 0) at price per bottle. Replies already read count again
// with it ("2B" = 2 cases).
func (s *Service) SetOptionCase(ctx context.Context, quotNum, code string, size int, price float64) error {
	q, err := s.QuotationByNumber(ctx, quotNum)
	if err != nil {
		return err
	}
	if q == nil {
		return fmt.Errorf("ordine %q non trovato", quotNum)
	}
	if size < 0 || size > 99 || size == 1 {
		return fmt.Errorf("bottiglie per cassa non valide: %d (0 = a bottiglia)", size)
	}
	if price <= 0 {
		return fmt.Errorf("inserisci un prezzo valido")
	}
	var name string
	if err := s.db().QueryRowContext(ctx, "SELECT wine_name FROM quotation_items WHERE quotation_id = ? AND UPPER(option) = UPPER(?) ORDER BY id LIMIT 1", q.ID, code).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("opzione %s non trovata", code)
		}
		return err
	}
	switch {
	case size == 0:
		name = strings.TrimSpace(caseNameRe.ReplaceAllString(name, ""))
	case regexp.MustCompile(`(?i)\(cassa da \d+`).MatchString(name):
		name = regexp.MustCompile(`(?i)\(cassa da \d+`).ReplaceAllString(name, fmt.Sprintf("(cassa da %d", size))
	default:
		name = strings.TrimSpace(caseNameRe.ReplaceAllString(name, "")) + fmt.Sprintf(" (cassa da %d)", size)
	}
	_, err = s.db().ExecContext(ctx, "UPDATE quotation_items SET wine_name = ?, price = ? WHERE quotation_id = ? AND UPPER(option) = UPPER(?)",
		name, math.Round(price*100)/100, q.ID, code)
	return err
}

// ── new quotation ────────────────────────────────────────────────────────────

type CatalogEntry struct {
	Description string
	Winery      string
}

func (e CatalogEntry) Label() string {
	if e.Winery != "" {
		return e.Description + " — " + e.Winery
	}
	return e.Description
}

func (s *Service) Catalog(ctx context.Context) ([]CatalogEntry, error) {
	rows, err := s.db().QueryContext(ctx, "SELECT COALESCE(description,''), COALESCE(winery,'') FROM items WHERE deleted_at IS NULL AND COALESCE(description,'') != '' ORDER BY description")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatalogEntry
	for rows.Next() {
		var e CatalogEntry
		if rows.Scan(&e.Description, &e.Winery) == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

type NewQuotationRow struct {
	Option   string
	WineName string
	Quantity int
	Price    float64
	// a wine picked from a listing: connected to that option of the listing's
	// order (like "Collega"); Source "i" and Camp keep the form's choice.
	LinkQuotation int64
	LinkOption    string
	Source        string // "p" (Prodotti, default) or "i" (inserzione)
	Camp          string
}

// CreateQuotation inserts a manual quotation; wines not yet in the catalog
// are added to it (except the ones taken from a listing).
func (s *Service) CreateQuotation(ctx context.Context, date, clientName string, rows []NewQuotationRow) (string, error) {
	byName, _, err := s.ClientOptions(ctx)
	if err != nil {
		return "", err
	}
	var num string
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if num, err = db.NextOrderNumber(ctx, tx, date); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO quotations (quotation_number, quotation_date) VALUES (?, ?)", num, date)
		if err != nil {
			return err
		}
		qid, _ := res.LastInsertId()
		for _, r := range rows {
			wine := strings.TrimSpace(r.WineName)
			var lq, lo any
			if r.LinkQuotation != 0 {
				lq, lo = r.LinkQuotation, r.LinkOption
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO quotation_items (quotation_id, option, wine_name, quantity, price, linked_quotation_id, linked_option) VALUES (?,?,?,?,?,?,?)",
				qid, r.Option, wine, r.Quantity, r.Price, lq, lo); err != nil {
				return err
			}
			if r.LinkQuotation != 0 {
				continue
			}
			var exists int
			err := tx.QueryRowContext(ctx, "SELECT 1 FROM items WHERE LOWER(description) = LOWER(?) AND deleted_at IS NULL LIMIT 1", wine).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				code := nextItemCode(ctx, tx)
				if _, err := tx.ExecContext(ctx, "INSERT INTO items (itemCode, description, created_at) VALUES (?, ?, datetime('now'))", code, wine); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		}
		var uid any
		if id, ok := byName[clientName]; ok {
			uid = id
		}
		_, err = tx.ExecContext(ctx, "UPDATE quotations SET manual_client_id = ?, manual_client_name = ? WHERE id = ?", uid, clientName, qid)
		return err
	})
	return num, err
}

// InitialOption is an option of a listing with the bottles it offered when
// first posted (or as corrected by hand).
type InitialOption struct {
	Letter string
	Wine   string
	Qty    int
	HasQty bool // a quantity is known (from the listing or set by hand)
}

// initialOptions returns the options of a campaign's posts (msgIDs, oldest
// first) with their initial quantity: set by hand on the first post
// (initial_qty), else the first count the seller gave for that option, in the
// earliest post that has one (a listing may start without counts). Later
// counts, and edits of a post, are ignored.
func (s *Service) initialOptions(ctx context.Context, msgIDs []string) ([]InitialOption, error) {
	var out []InitialOption
	idx := map[string]int{}
	var manual map[string]int
	for i, id := range msgIDs {
		var body, saved string
		if err := s.db().QueryRowContext(ctx, `SELECT COALESCE(original_body, body, ''), COALESCE(initial_qty, '')
			FROM listings WHERE msg_id = ?`, id).Scan(&body, &saved); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		if i == 0 && saved != "" {
			json.Unmarshal([]byte(saved), &manual)
		}
		opts := textutil.ParseOptions(body)
		if len(opts) == 0 {
			opts, _ = textutil.ParseSingleWine(body)
		}
		for _, o := range opts {
			if o.Case {
				continue
			}
			k, ok := idx[o.Letter]
			if !ok {
				k = len(out)
				idx[o.Letter] = k
				out = append(out, InitialOption{Letter: o.Letter, Wine: o.WineName})
			}
			if !out[k].HasQty && o.Quantity > 0 {
				out[k].Qty, out[k].HasQty = o.Quantity, true
			}
		}
	}
	for k := range out {
		if n, ok := manual[out[k].Letter]; ok {
			out[k].Qty, out[k].HasQty = n, true
		}
	}
	return out, nil
}

// campaignPosts returns the campaign's posts, oldest first.
func (s *Service) campaignPosts(ctx context.Context, c *Campaign) ([]string, error) {
	if c == nil || len(c.MsgIDs) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(c.MsgIDs)), ",")
	args := make([]any, len(c.MsgIDs))
	for i, id := range c.MsgIDs {
		args[i] = id
	}
	return s.postIDs(ctx, "SELECT msg_id FROM listings WHERE msg_id IN ("+ph+") ORDER BY created_at, id", args...)
}

func (s *Service) postIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.db().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CampaignInitialStock returns the initial quantity of each option of the
// campaign (shown and editable on the listing page).
func (s *Service) CampaignInitialStock(ctx context.Context, c *Campaign) ([]InitialOption, error) {
	ids, err := s.campaignPosts(ctx, c)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return s.initialOptions(ctx, ids)
}

// SetInitialQty corrects by hand the initial quantity of one option of the
// campaign's listing; an empty value clears it.
func (s *Service) SetInitialQty(ctx context.Context, c *Campaign, letter, value string) error {
	ids, err := s.campaignPosts(ctx, c)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("inserzione non trovata")
	}
	id := ids[0]
	opts, err := s.initialOptions(ctx, ids)
	if err != nil {
		return err
	}
	qty := map[string]int{}
	found := false
	for _, o := range opts {
		if o.HasQty {
			qty[o.Letter] = o.Qty
		}
		found = found || o.Letter == letter
	}
	if !found {
		return fmt.Errorf("opzione %q non trovata nell'inserzione", letter)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		delete(qty, letter)
	} else {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 9999 {
			return fmt.Errorf("quantità non valida: %q", value)
		}
		qty[letter] = n
	}
	b, err := json.Marshal(qty)
	if err != nil {
		return err
	}
	_, err = s.db().ExecContext(ctx, "UPDATE listings SET initial_qty = ? WHERE msg_id = ?", string(b), id)
	return err
}

// ItemDelivery is the estimated delivery of a wine of a manual order: its
// own date, or, when connected to a listing's option, that listing's date.
type ItemDelivery struct {
	Date         string // "YYYY-MM-DD", "" = not set
	Linked       bool
	LinkedOption string
	LinkedTitle  string // the listing's title
	LinkedNumber string // the listing's number, "INS0731"
	LinkedURL    string // the listing's page
}

// manualConsegne: whether the quotation is a manual order (no listing) and,
// if so, the estimated delivery of each of its wines, by option.
func (s *Service) manualConsegne(ctx context.Context, quotationID int64) (bool, map[string]ItemDelivery, error) {
	var n int
	if err := s.db().QueryRowContext(ctx, "SELECT COUNT(*) FROM listings WHERE quotation_id = ?", quotationID).Scan(&n); err != nil {
		return false, nil, err
	}
	if n > 0 {
		return false, nil, nil
	}
	rows, err := s.db().QueryContext(ctx, `SELECT UPPER(COALESCE(qi.option,'')), COALESCE(qi.consegna_stimata,''),
		       COALESCE(qi.linked_quotation_id, 0), COALESCE(qi.linked_option,''), COALESCE(lq.consegna_stimata,'')
		FROM quotation_items qi LEFT JOIN quotations lq ON lq.id = qi.linked_quotation_id
		WHERE qi.quotation_id = ?`, quotationID)
	if err != nil {
		return false, nil, err
	}
	type row struct {
		opt, own, lopt, ldate string
		lq                    int64
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.opt, &r.own, &r.lq, &r.lopt, &r.ldate); err != nil {
			rows.Close()
			return false, nil, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, nil, err
	}
	out := map[string]ItemDelivery{}
	for _, r := range list {
		d := ItemDelivery{Date: r.own}
		if r.lq != 0 {
			d = ItemDelivery{Date: r.ldate, Linked: true, LinkedOption: r.lopt}
			if c, err := s.QuotationListing(ctx, r.lq); err == nil && c != nil {
				d.LinkedTitle, d.LinkedURL, d.LinkedNumber = c.DisplayTitle, CampaignURL(c.ChatID, c.Key), c.Number
			}
		}
		out[r.opt] = d
	}
	return true, out, nil
}

// LinkTarget is a wine option of a listing's order a manual wine can be
// connected to.
type LinkTarget struct {
	QuotationID int64
	Letter      string
	Wine        string
	Price       float64
}

// LinkCampaign is a listing with its wine options, for the "Collega" list.
type LinkCampaign struct {
	Title     string
	Published string
	ChatName  string
	Options   []LinkTarget
}

// LinkCampaigns returns the listings with an order and their wine options
// (bottles, not cases), newest first, published since `since` ("YYYY-MM-DD",
// "" = all).
func (s *Service) LinkCampaigns(ctx context.Context, since string) ([]LinkCampaign, error) {
	camps, err := s.ConsegneCampaigns(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.quotationItems(ctx, "")
	if err != nil {
		return nil, err
	}
	byQ := map[int64][]LinkTarget{}
	for _, it := range items {
		if o := strings.ToUpper(strings.TrimSpace(it.Option)); len(o) == 1 {
			byQ[it.QuotationID] = append(byQ[it.QuotationID], LinkTarget{QuotationID: it.QuotationID, Letter: o, Wine: wineWithVintage(it.WineName, it.Vintage), Price: it.Price})
		}
	}
	var out []LinkCampaign
	for _, c := range camps {
		if c.Manual || len(c.QuotationIDs) == 0 || (since != "" && c.Published < since) {
			continue
		}
		opts := byQ[c.QuotationIDs[0]]
		if len(opts) == 0 {
			continue
		}
		out = append(out, LinkCampaign{Title: c.Title, Published: c.Published, ChatName: c.ChatName, Options: opts})
	}
	return out, nil
}

// SetItemLink connects a wine of a manual order to an option of a listing's
// order (toQuotation 0 disconnects it).
func (s *Service) SetItemLink(ctx context.Context, quotationID int64, option string, toQuotation int64, toOption string) error {
	if manual, _, err := s.manualConsegne(ctx, quotationID); err != nil {
		return err
	} else if !manual {
		return errors.New("si possono collegare solo i vini degli ordini manuali")
	}
	var q, o any
	if toQuotation != 0 {
		toOption = strings.ToUpper(strings.TrimSpace(toOption))
		var n int
		if err := s.db().QueryRowContext(ctx, `SELECT COUNT(*) FROM quotation_items WHERE quotation_id = ? AND UPPER(option) = ?
			AND quotation_id IN (SELECT quotation_id FROM listings WHERE quotation_id IS NOT NULL)`, toQuotation, toOption).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return errors.New("opzione dell'inserzione non trovata")
		}
		q, o = toQuotation, toOption
	}
	res, err := s.db().ExecContext(ctx, "UPDATE quotation_items SET linked_quotation_id = ?, linked_option = ? WHERE quotation_id = ? AND UPPER(option) = UPPER(?)", q, o, quotationID, option)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("riga dell'ordine non trovata")
	}
	return nil
}

// IsManualOrder: an order created by hand (no listing behind it).
func (s *Service) IsManualOrder(ctx context.Context, quotationID int64) (bool, error) {
	manual, _, err := s.manualConsegne(ctx, quotationID)
	return manual, err
}

// ManualRows returns the wines of a manual order, as stored.
func (s *Service) ManualRows(ctx context.Context, quotationID int64) ([]OrderRow, error) {
	items, err := s.quotationItems(ctx, "WHERE quotation_id = ?", quotationID)
	if err != nil {
		return nil, err
	}
	var out []OrderRow
	for _, it := range items {
		out = append(out, OrderRow{Opzione: it.Option, Vino: it.WineName, Qta: it.Quantity, Prezzo: it.Price})
	}
	return out, nil
}

// AddManualItem saves a wine in a manual order under the next free letter
// (so it gets its delivery date and can be connected to an inserzione).
// linkQ/linkOpt (0/"" for none) connect it to a listing's option.
func (s *Service) AddManualItem(ctx context.Context, quotationID int64, wine string, qty int, price float64, linkQ int64, linkOpt string) (string, error) {
	if manual, err := s.IsManualOrder(ctx, quotationID); err != nil {
		return "", err
	} else if !manual {
		return "", errors.New("si possono aggiungere vini solo agli ordini manuali")
	}
	var letter string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT UPPER(COALESCE(option,'')) FROM quotation_items WHERE quotation_id = ?", quotationID)
		if err != nil {
			return err
		}
		used := map[string]bool{}
		for rows.Next() {
			var o string
			if err := rows.Scan(&o); err != nil {
				rows.Close()
				return err
			}
			used[o] = true
		}
		rows.Close()
		for c := 'A'; c <= 'Z'; c++ {
			if !used[string(c)] {
				letter = string(c)
				break
			}
		}
		if letter == "" {
			return errors.New("l'ordine ha già 26 vini")
		}
		var lq, lo any
		if linkQ != 0 {
			lq, lo = linkQ, linkOpt
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO quotation_items (quotation_id, option, wine_name, quantity, price, linked_quotation_id, linked_option) VALUES (?,?,?,?,?,?,?)",
			quotationID, letter, wine, qty, price, lq, lo)
		return err
	})
	return letter, err
}

// DeleteManualItem removes a wine from a manual order.
func (s *Service) DeleteManualItem(ctx context.Context, quotationID int64, option string) error {
	if manual, err := s.IsManualOrder(ctx, quotationID); err != nil {
		return err
	} else if !manual {
		return errors.New("si possono togliere vini solo dagli ordini manuali")
	}
	_, err := s.db().ExecContext(ctx, "DELETE FROM quotation_items WHERE quotation_id = ? AND UPPER(option) = UPPER(?)", quotationID, option)
	return err
}

// SetItemConsegna saves the estimated delivery of one wine of a manual
// order ("" clears it).
func (s *Service) SetItemConsegna(ctx context.Context, quotationID int64, option, date string) error {
	date = strings.TrimSpace(date)
	if date != "" {
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return fmt.Errorf("data non valida: %q", date)
		}
	}
	var v any
	if date != "" {
		v = date
	}
	res, err := s.db().ExecContext(ctx, "UPDATE quotation_items SET consegna_stimata = ? WHERE quotation_id = ? AND UPPER(option) = UPPER(?) AND linked_quotation_id IS NULL", v, quotationID, option)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("riga dell'ordine non trovata")
	}
	return nil
}

// MoveQuotation links the order to another inserzione (when it was linked
// to the wrong one): the posts of the old inserzione lose it, all the posts
// of the new one get it, so its customers become the new inserzione's
// repliers. An inserzione that already has another order is refused.
func (s *Service) MoveQuotation(ctx context.Context, quotationID int64, to *Campaign) error {
	if to == nil || len(to.MsgIDs) == 0 {
		return errors.New("inserzione non trovata")
	}
	for _, q := range to.QuotationIDs {
		if q != quotationID {
			var num string
			s.db().QueryRowContext(ctx, "SELECT COALESCE(quotation_number,'') FROM quotations WHERE id = ?", q).Scan(&num)
			return fmt.Errorf("questa inserzione ha già l'ordine %s", num)
		}
	}
	ids, err := s.campaignPosts(ctx, to)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE listings SET quotation_id = NULL WHERE quotation_id = ?", quotationID); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, "UPDATE listings SET quotation_id = ? WHERE msg_id = ?", quotationID, id); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE quotations SET msg_id = ? WHERE id = ?", ids[0], quotationID)
		return err
	})
}

// LinkWindowStart is the earliest publication date of the listings a manual
// order's wines can be connected to: 6 months before the order date.
func (s *Service) LinkWindowStart(ctx context.Context, quotationID int64) string {
	var d string
	s.db().QueryRowContext(ctx, "SELECT COALESCE(quotation_date,'') FROM quotations WHERE id = ?", quotationID).Scan(&d)
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return ""
	}
	return t.AddDate(0, -6, 0).Format("2006-01-02")
}
