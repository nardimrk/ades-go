package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"adesgo/internal/llm"
	"adesgo/internal/textutil"
)

// "Controlla risposte" (Inserzioni): the LLM reads a campaign's replies next
// to what the order rules made of them, and flags the dubious or ambiguous
// ones with a proposed order. The user edits and confirms the proposal,
// which is stored on the reply (replies.order_override) and read by the
// order parser instead of the body, so Ordini and Consegne follow.

// ReplyCheck is the LLM's verdict on one reply (table reply_checks).
type ReplyCheck struct {
	ReplyID  int64
	Kind     string // "dubbia" | "ambigua"
	Reason   string
	Proposal string // order codes ("3A, 1 cassa B"), "" = none
	Status   string // "open" | "applied" | "dismissed"
	Stale    bool   // the reply changed since it was checked
}

func (c *ReplyCheck) Open() bool { return c != nil && c.Status == "open" }

// OrderOption is one option of the campaign's quotation, as the proposal
// editor shows it.
type OrderOption struct {
	Key    string // quotation_items.option: "A", "A-CASSA"
	Letter string
	Case   bool
	Wine   string
	Price  float64
}

// Code is the order code for qty of this option ("3A", "1 cassa A").
func (o OrderOption) Code(qty int) textutil.OrderCode {
	return textutil.OrderCode{Qty: qty, Option: o.Letter, Case: o.Case}
}

// Label names the option as orders do: "A", or "cassa A" for a full case.
func (o OrderOption) Label() string {
	if o.Case {
		return "cassa " + o.Letter
	}
	return o.Letter
}

// CampaignReview is what the conversation needs to show checks and
// proposals: the quotation's options and each customer's current order.
type CampaignReview struct {
	QuotationID     int64
	QuotationNumber string
	Options         []OrderOption
	Current         map[string]string // author id → current order in this quotation ("3A, 2B")
	LastRun         string            // "02/10/2026 14:32", "" = never checked
	// Orderable: replies can be confirmed as orders (the quotation exists, or
	// EnsureQuotation can create it from the listing)
	Orderable bool
	SellerIDs map[string]bool // authors of the campaign's listings: their replies aren't orders
	// Mismatch: where the quotation differs from the options of the listing
	// (an option missing, a different price), e.g. "manca B: Riesling a € 24.50".
	// Orders confirmed against it would be wrong, so the page warns.
	Mismatch []string
	// Missing: the listing's options that the quotation lacks; the confirm
	// modal offers them as new options to add to the order
	Missing []textutil.Option
}

// NewOption is an option added to a quotation from the confirm modal.
type NewOption struct {
	Letter string
	Case   bool
	Wine   string // one of the Prodotti
	Price  float64
}

// Key is the quotation_items.option of the new option ("B", "B-CASSA").
func (o NewOption) Key() string {
	if o.Case {
		return o.Letter + textutil.CaseSuffix
	}
	return o.Letter
}

func reviewMetaKey(chatID, key string) string { return "review:" + chatID + "|" + key }

// selectionCode turns a selection back into its order code.
func selectionCode(x Selection) textutil.OrderCode {
	letter, isCase := strings.CutSuffix(x.Opzione, textutil.CaseSuffix)
	return textutil.OrderCode{Qty: x.Qta, Option: letter, Case: isCase}
}

// CampaignReview loads the options and current orders of the campaign's
// quotation (none when the campaign has no quotation).
func (s *Service) CampaignReview(ctx context.Context, c *Campaign) (*CampaignReview, error) {
	rv := &CampaignReview{Current: map[string]string{}, SellerIDs: map[string]bool{}}
	if len(c.MsgIDs) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(c.MsgIDs)), ",")
		args := make([]any, len(c.MsgIDs))
		for i, id := range c.MsgIDs {
			args[i] = id
		}
		rows, err := s.db().QueryContext(ctx, "SELECT DISTINCT COALESCE(author_id,'') FROM listings WHERE msg_id IN ("+ph+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil && id != "" {
				rv.SellerIDs[id] = true
			}
		}
		rows.Close()
	}
	if ts := s.Store.Meta(ctx, reviewMetaKey(c.ChatID, c.Key)); ts != "" {
		if v, err := strconv.ParseInt(ts, 10, 64); err == nil {
			rv.LastRun = FmtTS(v)
		}
	}
	if len(c.QuotationIDs) == 0 {
		_, single := textutil.ParseSingleWine(c.Testo)
		rv.Orderable = single || len(textutil.ParseOptions(c.Testo)) > 0
		return rv, nil
	}
	rv.QuotationID = c.QuotationIDs[0]
	if err := s.db().QueryRowContext(ctx, "SELECT COALESCE(quotation_number,'') FROM quotations WHERE id = ?", rv.QuotationID).
		Scan(&rv.QuotationNumber); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	items, err := s.quotationItems(ctx, "WHERE quotation_id = ?", rv.QuotationID)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		key := strings.ToUpper(strings.TrimSpace(it.Option))
		letter, isCase := strings.CutSuffix(key, textutil.CaseSuffix)
		if len(letter) != 1 || letter < "A" || letter > "J" {
			continue
		}
		wine := it.WineName
		if v := strconv.Itoa(it.Vintage); it.Vintage != 0 && !strings.Contains(wine, v) {
			wine += " " + v
		}
		rv.Options = append(rv.Options, OrderOption{Key: key, Letter: letter, Case: isCase, Wine: wine, Price: it.Price})
	}
	// bottles before cases, then by letter: A, B, A-CASSA, B-CASSA
	sort.SliceStable(rv.Options, func(i, j int) bool {
		a, b := rv.Options[i], rv.Options[j]
		if a.Case != b.Case {
			return !a.Case
		}
		return a.Letter < b.Letter
	})
	rv.Orderable = len(rv.Options) > 0
	if rv.Mismatch, rv.Missing, err = s.listingMismatch(ctx, c.MsgIDs, rv.Options); err != nil {
		return nil, err
	}
	sel, err := s.ComputeSelections(ctx)
	if err != nil {
		return nil, err
	}
	codes := map[string][]textutil.OrderCode{}
	for _, x := range sel {
		if x.QuotationID == rv.QuotationID && x.Qta > 0 {
			codes[x.AuthorID] = append(codes[x.AuthorID], selectionCode(x))
		}
	}
	for author, cs := range codes {
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].Case != cs[j].Case {
				return !cs[i].Case
			}
			return cs[i].Option < cs[j].Option
		})
		rv.Current[author] = textutil.FormatOrderCodes(cs)
	}
	return rv, nil
}

// listingMismatch compares the quotation's options with the options of the
// campaign's posts (every repost: an option may appear in only some of them,
// the latest price wins). A listing the rules can't read gives no warning.
func (s *Service) listingMismatch(ctx context.Context, msgIDs []string, opts []OrderOption) (msgs []string, missing []textutil.Option, err error) {
	if len(msgIDs) == 0 {
		return nil, nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(msgIDs)), ",")
	args := make([]any, len(msgIDs))
	for i, id := range msgIDs {
		args[i] = id
	}
	rows, err := s.db().QueryContext(ctx, "SELECT COALESCE(body,'') FROM listings WHERE msg_id IN ("+ph+") ORDER BY timestamp, id", args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	want := map[string]textutil.Option{}
	var keys []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, nil, err
		}
		parsed := textutil.ParseOptions(body)
		if len(parsed) == 0 {
			parsed, _ = textutil.ParseSingleWine(body)
		}
		for _, o := range textutil.WithCaseOptions(body, parsed) {
			k := o.Letter
			if o.Case {
				k += textutil.CaseSuffix
			}
			if _, seen := want[k]; !seen {
				keys = append(keys, k)
			}
			want[k] = o
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	have := map[string]OrderOption{}
	for _, o := range opts {
		have[o.Key] = o
	}
	sort.Strings(keys)
	for _, k := range keys {
		w := want[k]
		label := strings.Replace(k, textutil.CaseSuffix, " (cassa)", 1)
		h, ok := have[k]
		switch {
		case !ok:
			msgs = append(msgs, fmt.Sprintf("manca l'opzione %s: %s a € %.2f", label, w.WineName, w.Price))
			missing = append(missing, w)
		case math.Abs(h.Price-w.Price) > 0.005:
			msgs = append(msgs, fmt.Sprintf("opzione %s: nell'ordine € %.2f, nell'inserzione € %.2f", label, h.Price, w.Price))
		}
	}
	return msgs, missing, nil
}

// Option returns the option an order code refers to. A case order that
// names no option ("1 cassa") is the only case option, if there is one.
func (rv *CampaignReview) Option(c textutil.OrderCode) (OrderOption, bool) {
	var cases []OrderOption
	for _, o := range rv.Options {
		if o.Case == c.Case && o.Letter == c.Option {
			return o, true
		}
		if o.Case {
			cases = append(cases, o)
		}
	}
	if c.Case && c.Option == "" && len(cases) == 1 {
		return cases[0], true
	}
	return OrderOption{}, false
}

// ProposalQty maps option key → proposed quantity, for the editor.
func (rv *CampaignReview) ProposalQty(codes string) map[string]int {
	out := map[string]int{}
	for _, c := range textutil.ParseOrders(codes) {
		if o, ok := rv.Option(c); ok {
			out[o.Key] = c.Qty
		}
	}
	return out
}

var bareQtyRe = regexp.MustCompile(`\b(\d{1,3})\b`)

// GuessQty prefills the "Conferma come ordine" editor. A message naming
// options ("3 B grazie") fills those options, and nothing at all when one of
// them is not in the order: "3 B" must never become 3 of option A. Otherwise,
// with one bottle option, the only number in the message ("4", "ne prendo 4").
func (rv *CampaignReview) GuessQty(body string) map[string]int {
	if codes := textutil.ParseOrders(body); len(codes) > 0 {
		out := map[string]int{}
		for _, c := range codes {
			o, ok := rv.Option(c)
			if !ok {
				return map[string]int{}
			}
			out[o.Key] = c.Qty
		}
		return out
	}
	var bottles []OrderOption
	for _, o := range rv.Options {
		if !o.Case {
			bottles = append(bottles, o)
		}
	}
	nums := bareQtyRe.FindAllString(body, -1)
	if len(bottles) != 1 || len(nums) != 1 {
		return map[string]int{}
	}
	n, _ := strconv.Atoi(nums[0])
	if n < 1 {
		return map[string]int{}
	}
	return map[string]int{bottles[0].Key: n}
}

// cleanProposal keeps the codes of existing options, in canonical form.
func (rv *CampaignReview) cleanProposal(codes string) string {
	// with one bottle option a bare quantity ("4") is an order of it
	if n, err := strconv.Atoi(strings.TrimSpace(codes)); err == nil {
		var bottles []OrderOption
		for _, o := range rv.Options {
			if !o.Case {
				bottles = append(bottles, o)
			}
		}
		if len(bottles) == 1 && n >= 0 && n <= 999 {
			return textutil.FormatOrderCodes([]textutil.OrderCode{bottles[0].Code(n)})
		}
		return ""
	}
	var out []textutil.OrderCode
	seen := map[string]int{}
	for _, c := range textutil.ParseOrders(codes) {
		o, ok := rv.Option(c)
		if !ok || c.Qty < 0 || c.Qty > 999 {
			continue
		}
		if i, dup := seen[o.Key]; dup {
			out[i] = o.Code(c.Qty) // a later mention replaces the earlier one
			continue
		}
		seen[o.Key] = len(out)
		out = append(out, o.Code(c.Qty))
	}
	return textutil.FormatOrderCodes(out)
}

// EnsureQuotation creates the campaign's quotation when it has none: from
// lettered options (the automatic import, as on Ordini), else from a single
// wine. Returns whether one was created.
func (s *Service) EnsureQuotation(ctx context.Context, c *Campaign) (bool, error) {
	if len(c.QuotationIDs) > 0 {
		return false, nil
	}
	created, _, _, err := s.ImportPreventivi(ctx)
	if err != nil {
		return false, err
	}
	if created > 0 {
		if fresh, err := s.FindCampaign(ctx, c.ChatID, c.Key); err == nil && fresh != nil && len(fresh.QuotationIDs) > 0 {
			return true, nil
		}
	}
	return s.EnsureSingleWineQuotation(ctx, c)
}

// EnsureSingleWineQuotation creates the quotation of a campaign that offers
// a single wine without option letters (see textutil.ParseSingleWine), if it
// has none. Only "Controlla risposte" calls it, so such listings get an order
// only when the user asks: the automatic import leaves them out.
func (s *Service) EnsureSingleWineQuotation(ctx context.Context, c *Campaign) (bool, error) {
	if len(c.QuotationIDs) > 0 || len(c.MsgIDs) == 0 {
		return false, nil
	}
	opts, ok := textutil.ParseSingleWine(c.Testo)
	if !ok {
		return false, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(c.MsgIDs)), ",")
	args := make([]any, len(c.MsgIDs))
	for i, id := range c.MsgIDs {
		args[i] = id
	}
	var ts int64
	if err := s.db().QueryRowContext(ctx, "SELECT COALESCE(MIN(timestamp),0) FROM listings WHERE msg_id IN ("+ph+")", args...).Scan(&ts); err != nil {
		return false, err
	}
	num, err := s.CreateImportedQuotation(ctx, c.MsgIDs, ts, opts)
	if err != nil {
		return false, err
	}
	log.Printf("[review] %q: single-wine quotation %s created", c.Key, num)
	return true, nil
}

// ── actions on a reply ───────────────────────────────────────────────────────

// SetReplyOrder stores the order codes confirmed by hand on a reply ("-" =
// not an order) and closes its check.
func (s *Service) SetReplyOrder(ctx context.Context, replyID int64, codes string) error {
	return s.SetReplyOrderAddingOptions(ctx, replyID, 0, nil, codes)
}

// SetReplyOrderAddingOptions first adds new options to the quotation (the
// ones the order lacked, typed in the confirm modal), then stores the reply's
// order — all or nothing.
func (s *Service) SetReplyOrderAddingOptions(ctx context.Context, replyID, quotationID int64, add []NewOption, codes string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, o := range add {
			var exists int
			err := tx.QueryRowContext(ctx, "SELECT 1 FROM quotation_items WHERE quotation_id = ? AND UPPER(option) = ?", quotationID, o.Key()).Scan(&exists)
			if err == nil {
				return fmt.Errorf("l'opzione %s c'è già nell'ordine", o.Key())
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO quotation_items (quotation_id, option, wine_name, quantity, price) VALUES (?, ?, ?, 0, ?)",
				quotationID, o.Key(), o.Wine, o.Price); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, "UPDATE replies SET order_override = ? WHERE id = ?", codes, replyID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("messaggio non trovato")
		}
		_, err = tx.ExecContext(ctx, "UPDATE reply_checks SET status = 'applied' WHERE reply_id = ?", replyID)
		return err
	})
}

// ResetReplyOrder drops the codes set by hand: the rules read the body
// again, and the check (if any) is open again.
func (s *Service) ResetReplyOrder(ctx context.Context, replyID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE replies SET order_override = NULL WHERE id = ?", replyID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE reply_checks SET status = 'open' WHERE reply_id = ?", replyID)
		return err
	})
}

// DismissReplyCheck hides a check: the automatic reading is fine.
func (s *Service) DismissReplyCheck(ctx context.Context, replyID int64) error {
	_, err := s.db().ExecContext(ctx, "UPDATE reply_checks SET status = 'dismissed' WHERE reply_id = ?", replyID)
	return err
}

// ── the LLM run ──────────────────────────────────────────────────────────────

// ReviewJob runs one campaign check at a time in the background: a long
// conversation takes several requests, paced for the free tier.
type ReviewJob struct {
	svc *Service
	llm *llm.Client

	mu     sync.Mutex
	status ReviewStatus
}

type ReviewStatus struct {
	Running bool
	ChatID  string
	Key     string
	Done    int // batches done
	Total   int // batches
	Flagged int
	Message string // outcome of the last run
	Error   bool
}

// For reports whether the status is about this campaign.
func (st ReviewStatus) For(chatID, key string) bool { return st.ChatID == chatID && st.Key == key }

func NewReviewJob(svc *Service, client *llm.Client) *ReviewJob {
	return &ReviewJob{svc: svc, llm: client}
}

func (j *ReviewJob) Enabled() bool { return j.llm.Enabled() }

func (j *ReviewJob) Status() ReviewStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

func (j *ReviewJob) set(f func(*ReviewStatus)) {
	j.mu.Lock()
	f(&j.status)
	j.mu.Unlock()
}

// Start checks this campaign unless a check is already running (here or on
// another campaign: they share the free quota). Returns false if busy.
func (j *ReviewJob) Start(ctx context.Context, chatID, key string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status.Running {
		return false
	}
	j.status = ReviewStatus{Running: true, ChatID: chatID, Key: key}
	go j.run(context.WithoutCancel(ctx), chatID, key)
	return true
}

const (
	reviewBatch  = 35 // replies per request
	reviewPace   = 3500 * time.Millisecond
	reviewMaxLen = 500 // runes of a reply sent to the LLM
)

func (j *ReviewJob) run(ctx context.Context, chatID, key string) {
	finish := func(msg string, isErr bool) {
		j.set(func(s *ReviewStatus) { s.Running, s.Message, s.Error = false, msg, isErr })
	}
	defer func() {
		if v := recover(); v != nil {
			log.Printf("[review] panic: %v", v)
			finish("Errore interno durante il controllo.", true)
		}
	}()
	if !j.llm.Enabled() {
		finish("OPENROUTER_API_KEY non configurata.", true)
		return
	}
	// a campaign with lettered options gets its quotation here, as on Ordini
	if _, _, _, err := j.svc.ImportPreventivi(ctx); err != nil {
		log.Printf("[review] import quotations: %v", err)
	}
	camp, err := j.svc.FindCampaign(ctx, chatID, key)
	if err != nil || camp == nil {
		finish("Inserzione non trovata.", true)
		return
	}
	// one wine without letters: its quotation (option A) is created now, so
	// replies like "4" can be added to it
	if created, err := j.svc.EnsureSingleWineQuotation(ctx, camp); err != nil {
		finish("Errore: "+err.Error(), true)
		return
	} else if created {
		if camp, err = j.svc.FindCampaign(ctx, chatID, key); err != nil || camp == nil {
			finish("Inserzione non trovata.", true)
			return
		}
	}
	replies, err := j.svc.CampaignReplies(ctx, camp.MsgIDs)
	if err != nil {
		finish("Errore: "+err.Error(), true)
		return
	}
	rv, err := j.svc.CampaignReview(ctx, camp)
	if err != nil {
		finish("Errore: "+err.Error(), true)
		return
	}
	if len(replies) == 0 {
		finish("Nessuna risposta da controllare.", false)
		return
	}

	total := (len(replies) + reviewBatch - 1) / reviewBatch
	j.set(func(s *ReviewStatus) { s.Total = total })
	failed := 0
	for b := 0; b < total; b++ {
		if b > 0 {
			time.Sleep(reviewPace)
		}
		batch := replies[b*reviewBatch : min((b+1)*reviewBatch, len(replies))]
		prompt := reviewPrompt(camp, rv, replies, batch, b*reviewBatch)
		text, err := j.llm.Complete(ctx, prompt, 3000)
		if errors.Is(err, llm.ErrDailyCap) {
			if b > 0 {
				j.saveRun(ctx, camp)
			}
			finish(fmt.Sprintf("Quota giornaliera OpenRouter esaurita dopo %d di %d blocchi: riprova dopo il reset.", b, total), true)
			return
		}
		var found []reviewFinding
		if err == nil {
			err = json.Unmarshal([]byte(extractJSON(text)), &found)
		}
		if err != nil {
			failed++
			log.Printf("[review] %q batch %d: %v", key, b+1, err)
			j.set(func(s *ReviewStatus) { s.Done++ })
			continue
		}
		n, err := j.svc.saveChecks(ctx, rv, batch, b*reviewBatch, found)
		if err != nil {
			finish("Errore: "+err.Error(), true)
			return
		}
		j.set(func(s *ReviewStatus) { s.Done++; s.Flagged += n })
	}
	j.saveRun(ctx, camp)
	st := j.Status()
	var msg string
	switch {
	case failed == total:
		finish("Il modello non ha risposto in modo valido: riprova tra qualche minuto.", true)
		return
	case st.Flagged == 0:
		msg = "Controllo completato: nessuna risposta dubbia."
	default:
		msg = "Controllo completato: " + plural(st.Flagged, "risposta da verificare", "risposte da verificare") + "."
	}
	if failed > 0 {
		msg += fmt.Sprintf(" %d blocchi su %d non riusciti: riprova per controllarli.", failed, total)
	}
	finish(msg, false)
	log.Printf("[review] %q: %d flagged, %d/%d batches failed", key, st.Flagged, failed, total)
}

func (j *ReviewJob) saveRun(ctx context.Context, camp *Campaign) {
	_ = j.svc.Store.SetMeta(ctx, reviewMetaKey(camp.ChatID, camp.Key), strconv.FormatInt(time.Now().Unix(), 10))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

type reviewFinding struct {
	N        int    `json:"n"`
	Tipo     string `json:"tipo"`
	Motivo   string `json:"motivo"`
	Proposta string `json:"proposta"`
}

// saveChecks replaces the open checks of a batch with the new findings.
// Replies the user already settled (check applied/dismissed on the same text,
// or codes set by hand) are left alone. Returns the open checks saved.
func (s *Service) saveChecks(ctx context.Context, rv *CampaignReview, batch []ReplyView, offset int, found []reviewFinding) (int, error) {
	settled := func(r ReplyView) bool {
		return r.Override != "" || (r.Check != nil && r.Check.Status != "open" && !r.Check.Stale)
	}
	saved := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, r := range batch {
			if !settled(r) {
				if _, err := tx.ExecContext(ctx, "DELETE FROM reply_checks WHERE reply_id = ?", r.ID); err != nil {
					return err
				}
			}
		}
		done := map[int64]bool{}
		for _, f := range found {
			i := f.N - 1 - offset
			if i < 0 || i >= len(batch) {
				continue
			}
			r := batch[i]
			kind := strings.ToLower(strings.TrimSpace(f.Tipo))
			if (kind != "dubbia" && kind != "ambigua") || settled(r) || done[r.ID] {
				continue
			}
			done[r.ID] = true
			reason := strings.TrimSpace(textutil.StripEmoji(f.Motivo))
			if rr := []rune(reason); len(rr) > 300 {
				reason = string(rr[:300]) + "…"
			}
			proposal := rv.cleanProposal(f.Proposta)
			if kind == "dubbia" && proposal != "" && proposal == r.Reading {
				continue // the model agrees with the rules after all
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO reply_checks (reply_id, kind, reason, proposal, body, status, checked_at)
				VALUES (?, ?, ?, ?, ?, 'open', datetime('now'))`, r.ID, kind, reason, proposal, r.Testo); err != nil {
				return err
			}
			saved++
		}
		return nil
	})
	return saved, err
}

const reviewPromptHead = `Sei un assistente che controlla gli ordini ricevuti come risposte WhatsApp a un'inserzione di vendita vini.
Un programma ha già letto gli ordini con regole semplici. Per ogni messaggio trovi la "lettura" del programma:
codici NUMERO+LETTERA ("3A" = 3 bottiglie dell'opzione A, "1 cassa B" = una cassa intera dell'opzione B),
oppure "nessuna" se il messaggio non è stato letto come ordine. Se lo stesso cliente cita di nuovo un'opzione
in un messaggio successivo, la nuova quantità sostituisce la precedente.

Annuncio:
---
%s
---

Opzioni ordinabili:
%s
Ordine attuale di ogni cliente (risultato di tutte le letture):
%s
Messaggi, in ordine di tempo:
%s
Segnala SOLO i messaggi che richiedono il controllo di una persona:
- "dubbia": la lettura del programma è probabilmente sbagliata o incompleta. Esempi: un prezzo, un'annata o
  un orario letti come quantità; una domanda ("c'è ancora la B?", "3A quanto costa?") letta come ordine; un
  ordine scritto a parole o col nome del vino non letto; una modifica o un annullamento ("metti 2 invece di 3",
  "annullo", "tolgo la B") non applicati; un ordine fatto per un'altra persona.
- "ambigua": il messaggio sembra un ordine ma non si capisce con certezza quale opzione o quantità
  ("ne prendo 2", "anche per me", "come sopra", "una cassa" con più opzioni).
NON segnalare: saluti, ringraziamenti, domande generiche, messaggi del venditore, ordini letti correttamente.
%s

Per ogni messaggio segnalato scrivi "proposta": i codici che quel messaggio dovrebbe dare, stesso formato
(es. "3A" oppure "3A, 1 cassa B"), con la quantità risultante per ogni opzione che il messaggio tocca
(es. ordine attuale "3A" e messaggio "aggiungine 2" = "5A"; annullamento dell'opzione A = "0A").
Lascia "proposta" vuota se il messaggio non è un ordine o se non si può dedurre.
"motivo": una frase breve in italiano che spiega il dubbio.

Rispondi SOLO con un array JSON, vuoto [] se non c'è nulla da segnalare. Nessun altro testo:
[{"n": 3, "tipo": "dubbia", "motivo": "...", "proposta": "3A"}]`

// singleOptionRule: with one wine on offer, a bare quantity is an order.
func singleOptionRule(rv *CampaignReview) string {
	letters := map[string]bool{}
	for _, o := range rv.Options {
		letters[o.Letter] = true
	}
	if len(letters) != 1 {
		return ""
	}
	l := rv.Options[0].Letter
	return `
C'è UNA SOLA opzione (` + l + `): i clienti spesso scrivono solo la quantità. Un messaggio che indica solo
un numero di bottiglie ("4", "2 grazie", "ne prendo due", "per me 3", "6 bottiglie") è un ordine di ` + l + `:
se la lettura è "nessuna" segnalalo come "dubbia" e proponi la quantità con la lettera (es. "4` + l + `").
Una cassa ("una cassa", "1 cassa") è "1 cassa ` + l + `" se l'opzione cassa esiste, altrimenti chiedi conferma ("ambigua").
`
}

// reviewPrompt asks about batch (replies[offset:offset+len(batch)]); the
// messages keep their number in the whole conversation.
func reviewPrompt(camp *Campaign, rv *CampaignReview, all, batch []ReplyView, offset int) string {
	listing := []rune(textutil.StripEmoji(camp.Testo))
	if len(listing) > 2500 {
		listing = listing[:2500]
	}
	var opts strings.Builder
	if len(rv.Options) == 0 {
		opts.WriteString("(nessuna opzione con lettera: proposta sempre vuota)\n")
	}
	for _, o := range rv.Options {
		fmt.Fprintf(&opts, "- %s = %s, %s euro\n", o.Label(), o.Wine, Money(o.Price))
	}
	names := map[string]string{}
	for _, r := range all {
		names[r.AuthorID] = r.Utente
	}
	var cur strings.Builder
	authors := make([]string, 0, len(rv.Current))
	for a := range rv.Current {
		authors = append(authors, a)
	}
	sort.Strings(authors)
	for _, a := range authors {
		name := names[a]
		if name == "" {
			name = FmtName(a)
		}
		fmt.Fprintf(&cur, "- %s: %s\n", name, rv.Current[a])
	}
	if cur.Len() == 0 {
		cur.WriteString("(nessun ordine letto)\n")
	}
	var msgs strings.Builder
	for i, r := range batch {
		body := []rune(strings.TrimSpace(textutil.StripEmoji(r.Testo)))
		if len(body) > reviewMaxLen {
			body = append(body[:reviewMaxLen], '…')
		}
		reading := r.Reading
		if reading == "" {
			reading = "nessuna"
		}
		if r.Override != "" {
			reading += " (corretta a mano)"
		}
		fmt.Fprintf(&msgs, "[%d] %s, %s: %s\n    lettura: %s\n", offset+i+1, r.Utente, r.Ora,
			strings.ReplaceAll(string(body), "\n", " / "), reading)
	}
	return fmt.Sprintf(reviewPromptHead, string(listing), opts.String(), cur.String(), msgs.String(), singleOptionRule(rv))
}
