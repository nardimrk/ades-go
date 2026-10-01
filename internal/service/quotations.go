package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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
}

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
	rows, err := s.db().QueryContext(ctx, `
		SELECT r.author_id, `+userNameSQL+`, COALESCE(r.msg_id,''), COALESCE(r.body,''), COALESCE(r.timestamp,0),
		       q.quotation_number, q.id, COALESCE(q.quotation_date,'')
		FROM replies r
		LEFT JOIN users u      ON u.id     = r.author_id
		LEFT JOIN listings l   ON l.msg_id = r.listing_msg_id
		LEFT JOIN quotations q ON q.id     = l.quotation_id
		WHERE r.listing_msg_id IS NOT NULL AND q.id IS NOT NULL
		ORDER BY q.id, r.timestamp, r.id`)
	if err != nil {
		return nil, err
	}
	type rep struct {
		authorID, userName, msgID, body string
		ts                              int64
		qnum                            string
		qid                             int64
		qdate                           string
		matches                         int
	}
	var reps []rep
	for rows.Next() {
		var r rep
		var author, name, qnum sql.NullString
		if err := rows.Scan(&author, &name, &r.msgID, &r.body, &r.ts, &qnum, &r.qid, &r.qdate); err != nil {
			rows.Close()
			return nil, err
		}
		r.authorID, r.userName, r.qnum = nullStr(author), nullStr(name), nullStr(qnum)
		r.matches = len(textutil.ParseOrders(r.body))
		reps = append(reps, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The same physical message can be stored twice (real id + synthetic id,
	// possibly with slightly re-formatted text after a WhatsApp edit). Two
	// distinct messages from one customer in the same second are effectively
	// impossible, so keep one per (quotation, author, second): the version that
	// parses into the most option codes, and on a tie the one stored under its
	// real WhatsApp id (the synthetic one is the early, possibly pre-edit copy).
	type key struct {
		qid    int64
		author string
		ts     int64
	}
	best := map[key]int{}
	for i, r := range reps {
		k := key{r.qid, r.authorID, r.ts}
		if j, ok := best[k]; !ok || r.matches > reps[j].matches ||
			(r.matches == reps[j].matches && realID(r.msgID) && !realID(reps[j].msgID)) {
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
		if textutil.MentionsQuotationNumber(r.body) {
			continue
		}
		for _, c := range textutil.ParseOrders(r.body) {
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
			})
		}
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
	return out, nil
}

// realID reports a serialized WhatsApp id ("false_<chat>_<id>_<author>")
// as opposed to a synthetic fallback id.
func realID(msgID string) bool {
	return strings.HasPrefix(msgID, "false_") || strings.HasPrefix(msgID, "true_")
}

// ── import quotations from listings ──────────────────────────────────────────

func (s *Service) nextIMPNumber(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) string {
	var last string
	n := 1
	if err := q.QueryRowContext(ctx,
		"SELECT quotation_number FROM quotations WHERE quotation_number LIKE 'IMP%' ORDER BY quotation_number DESC LIMIT 1").Scan(&last); err == nil {
		if v, err := strconv.Atoi(strings.TrimPrefix(last, "IMP")); err == nil {
			n = v + 1
		}
	}
	return fmt.Sprintf("IMP%04d", n)
}

func (s *Service) NextIMPNumber(ctx context.Context) string { return s.nextIMPNumber(ctx, s.db()) }

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
		opts := textutil.ParseOptionsWithQty(grp[0].body)
		if len(opts) == 0 && minTS >= noQtyOptionsSince.Unix() {
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

// CreateImportedQuotation creates an IMPnnnn quotation with these options and
// links the given listings to it. Returns the quotation number.
func (s *Service) CreateImportedQuotation(ctx context.Context, msgIDs []string, ts int64, opts []textutil.Option) (string, error) {
	var num string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		num = s.nextIMPNumber(ctx, tx)
		res, err := tx.ExecContext(ctx, "INSERT INTO quotations (quotation_number, quotation_date) VALUES (?, ?)",
			num, time.Unix(ts, 0).Format("2006-01-02"))
		if err != nil {
			return err
		}
		qid, _ := res.LastInsertId()
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
	Title     string // linked listing's title, or "Ordine manuale"
	Number    string
	Date      string
	Clienti   int
	Bottiglie int
	Totale    float64
	SavedOn   string // order_date of a saved order, "" = none
}

func (s *Service) QuotationSummaries(ctx context.Context) ([]QuotationSummary, error) {
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
	saved := map[string]string{}
	rows, err := s.db().QueryContext(ctx, "SELECT COALESCE(quotation_number,''), COALESCE(order_date,'') FROM orders ORDER BY id")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n, d string
		if rows.Scan(&n, &d) == nil {
			if _, ok := saved[n]; !ok {
				saved[n] = d
			}
		}
	}
	rows.Close()

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
		sum := QuotationSummary{ID: q.ID, Number: q.Number, Date: q.Date, SavedOn: saved[q.Number], Title: titles[q.ID]}
		if sum.Title == "" {
			sum.Title = "Ordine manuale"
			if q.ManualClientName != "" {
				sum.Title += " · " + textutil.StripEmoji(q.ManualClientName)
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
					sum.Bottiglie += x.Qta
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

// SearchSelections filters selections by customer or wine, newest first.
func SearchSelections(sel []Selection, q string) []Selection {
	q = strings.ToLower(strings.TrimSpace(q))
	var out []Selection
	for _, x := range sel {
		if strings.Contains(strings.ToLower(x.Utente), q) || strings.Contains(strings.ToLower(x.Vino), q) {
			out = append(out, x)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DataPrev > out[j].DataPrev })
	return out
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
}

// ClientOptions maps display name → user id for every non-group user.
func (s *Service) ClientOptions(ctx context.Context) (map[string]string, []string, error) {
	rows, err := s.db().QueryContext(ctx, "SELECT id, COALESCE(name,'') FROM users WHERE id NOT LIKE '%@g.us' ORDER BY name")
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
	d.Single = len(order) == 1
	for _, utente := range order {
		sec, err := s.BuildSection(ctx, q, utente, s.DefaultCliente(q, utente, d.Single), rowsBy[utente])
		if err != nil {
			return nil, err
		}
		d.Sections = append(d.Sections, sec)
	}
	return d, nil
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
	sec := CustomerSection{Utente: utente, Cliente: cliente, Rows: rows}
	seen := map[string]bool{}
	for _, c := range append([]string{utente, cliente}, names...) {
		if !seen[c] {
			seen[c] = true
			sec.Options = append(sec.Options, c)
		}
	}
	sec.SavedOn = s.SavedOrderDate(ctx, q.Number, cliente, byName[cliente])
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
}

// CreateQuotation inserts a manual quotation; wines not yet in the catalog
// are added to it.
func (s *Service) CreateQuotation(ctx context.Context, date, clientName string, rows []NewQuotationRow) (string, error) {
	byName, _, err := s.ClientOptions(ctx)
	if err != nil {
		return "", err
	}
	var num string
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		num = s.nextIMPNumber(ctx, tx)
		res, err := tx.ExecContext(ctx, "INSERT INTO quotations (quotation_number, quotation_date) VALUES (?, ?)", num, date)
		if err != nil {
			return err
		}
		qid, _ := res.LastInsertId()
		for _, r := range rows {
			wine := strings.TrimSpace(r.WineName)
			if _, err := tx.ExecContext(ctx, "INSERT INTO quotation_items (quotation_id, option, wine_name, quantity, price) VALUES (?,?,?,?,?)",
				qid, r.Option, wine, r.Quantity, r.Price); err != nil {
				return err
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
