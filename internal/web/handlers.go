package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"adesgo/internal/service"
	"adesgo/internal/textutil"
	"adesgo/internal/web/views"
)

// ── Inserzioni ───────────────────────────────────────────────────────────────

func (s *Server) inserzioniData(r *http.Request) (views.InserzioniData, error) {
	ctx := r.Context()
	qs := r.URL.Query()
	d := views.InserzioniData{Search: qs.Get("q"), Group: qs.Get("g"), GroupIDs: s.cfg.ChannelIDs, Groups: s.svc.GroupNames(ctx),
		TitleJob: s.titles.Status(), LLMEnabled: s.llm.Enabled()}
	rows, err := s.svc.InserzioniList(ctx, d.Search, d.Group)
	d.Rows = rows
	return d, err
}

func (s *Server) loadSelection(r *http.Request, d *views.InserzioniData) error {
	camp, err := s.svc.FindCampaign(r.Context(), d.SelChat, d.SelKey)
	if err != nil || camp == nil {
		return err
	}
	d.Selected = camp
	if d.Replies, err = s.svc.CampaignReplies(r.Context(), camp.MsgIDs); err != nil {
		return err
	}
	if _, d.Clients, err = s.svc.ClientOptions(r.Context()); err != nil {
		return err
	}
	if d.Initial, err = s.svc.CampaignInitialStock(r.Context(), camp); err != nil {
		return err
	}
	d.LLMEnabled = s.reviews.Enabled()
	d.ReviewJob = s.reviews.Status()
	d.Review, err = s.svc.CampaignReview(r.Context(), camp)
	return err
}

// inserzioni shows the campaign table, or a campaign's conversation when
// chat + c are given (links from the table and from Consegne).
func (s *Server) inserzioni(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	if qs.Get("chat") != "" {
		d := views.InserzioniData{SelChat: qs.Get("chat"), SelKey: qs.Get("c"), Search: qs.Get("q"), Group: qs.Get("g")}
		if err := s.loadSelection(r, &d); err != nil {
			s.fail(w, r, err)
			return
		}
		// previous / next inserzione of the list, with the same filters
		rows, err := s.svc.InserzioniList(r.Context(), d.Search, d.Group)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		d.Prev, d.Next = service.CampaignNeighbors(rows, d.SelChat, d.SelKey)
		render(w, r, views.InserzioneDetail(d))
		return
	}
	d, err := s.inserzioniData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.Inserzioni(d))
}

func (s *Server) inserzioniList(w http.ResponseWriter, r *http.Request) {
	d, err := s.inserzioniData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// keep the filters in the address bar, so back/reload show the same list
	w.Header().Set("HX-Replace-Url", "/inserzioni?"+url.Values{"q": {d.Search}, "g": {d.Group}}.Encode())
	render(w, r, views.InserzioniList(d))
}

func (s *Server) titleJobStatus(w http.ResponseWriter, r *http.Request) {
	st := s.titles.Status()
	if !st.Running && st.Done > 0 {
		// only the progress poll calls this: when it sees the job finish,
		// reload the page so the list shows the new titles
		w.Header().Set("HX-Refresh", "true")
	}
	render(w, r, views.TitleJobStatus(st, s.llm.Enabled()))
}

func (s *Server) titleJobStart(w http.ResponseWriter, r *http.Request) {
	s.titles.Start(r.Context())
	render(w, r, views.TitleJobStatus(s.titles.Status(), s.llm.Enabled()))
}

func (s *Server) inserzioniRename(w http.ResponseWriter, r *http.Request) {
	d := views.InserzioniData{SelChat: r.FormValue("chat"), SelKey: r.FormValue("c")}
	if err := s.loadSelection(r, &d); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Selected != nil {
		if err := s.svc.RenameCampaign(r.Context(), d.Selected.MsgIDs, r.FormValue("title")); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	// the tree labels changed too: reload the page on the same selection
	w.Header().Set("HX-Redirect", "/inserzioni?"+url.Values{"chat": {d.SelChat}, "c": {d.SelKey}}.Encode())
}

// inserzioniConsegna autosaves the campaign's estimated delivery date.
func (s *Server) inserzioniConsegna(w http.ResponseWriter, r *http.Request) {
	d := views.InserzioniData{SelChat: r.FormValue("chat"), SelKey: r.FormValue("c")}
	if err := s.loadSelection(r, &d); err != nil {
		autosaveResult(w, r, err)
		return
	}
	if d.Selected == nil {
		autosaveResult(w, r, fmt.Errorf("inserzione non trovata"))
		return
	}
	autosaveResult(w, r, s.svc.SetCampaignDelivery(r.Context(), d.Selected.MsgIDs, r.FormValue("value")))
}

func (s *Server) deleteReply(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.svc.DeleteReply(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	toast(w, r, "success", "Messaggio eliminato.")
}

// addReply stores a reply typed in by hand (a message WhatsApp never
// delivered to the app) and redraws the conversation.
func (s *Server) addReply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := views.InserzioniData{SelChat: r.FormValue("chat"), SelKey: r.FormValue("c")}
	if err := s.loadSelection(r, &d); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Selected == nil {
		s.fail(w, r, fmt.Errorf("inserzione non trovata"))
		return
	}
	byName, _, err := s.svc.ClientOptions(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := strings.TrimSpace(r.FormValue("cliente"))
	authorID, known := byName[name]
	body := strings.TrimSpace(r.FormValue("testo"))
	at, timeErr := time.ParseInLocation("2006-01-02T15:04", r.FormValue("ora"), time.Local)
	warn := ""
	switch {
	case !known:
		warn = "Cliente non trovato: " + name + ". Sceglilo dall'elenco (un cliente nuovo si aggiunge da Clienti)."
	case timeErr != nil:
		warn = "Data e ora non valide."
	case body == "":
		warn = "Scrivi il testo del messaggio."
	}
	if warn == "" {
		if err := s.svc.AddManualReply(ctx, d.Selected, authorID, body, at.Unix()); errors.Is(err, service.ErrDuplicateReply) {
			warn = "Questo messaggio di " + name + " c'è già."
		} else if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if warn != "" {
		// the form keeps what was typed
		w.Header().Set("HX-Reswap", "none")
		toast(w, r, "warning", warn)
		return
	}
	if err := s.loadSelection(r, &d); err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.Conversation(d))
	toast(w, r, "success", "Risposta aggiunta: "+name+" · "+at.Format("02/01/2006 15:04"))
}

// orderItemConsegna autosaves the estimated delivery of a wine of a manual order.
func (s *Server) orderItemConsegna(w http.ResponseWriter, r *http.Request) {
	qid, err := strconv.ParseInt(r.FormValue("q"), 10, 64)
	if err == nil {
		err = s.svc.SetItemConsegna(r.Context(), qid, r.FormValue("option"), r.FormValue("value"))
	}
	autosaveResult(w, r, err)
}

// moveOrderForm opens the list of inserzioni to move an order to.
func (s *Server) moveOrderForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	num := r.URL.Query().Get("n")
	q, err := s.svc.QuotationByNumber(ctx, num)
	if err != nil || q == nil {
		s.fail(w, r, fmt.Errorf("ordine %q non trovato", num))
		return
	}
	rows, err := s.svc.InserzioniList(ctx, "", "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sums, err := s.svc.QuotationSummaries(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	quotNum := map[int64]string{}
	for _, x := range sums {
		quotNum[x.ID] = x.Number
	}
	current := "nessuna inserzione"
	if c, err := s.svc.QuotationListing(ctx, q.ID); err == nil && c != nil {
		current = c.DisplayTitle
		if c.Number != "" {
			current = c.Number + " · " + current
		}
	}
	render(w, r, views.MoveOrderModal(num, current, rows, quotNum))
}

// moveOrder links the order to the chosen inserzione and reloads it.
func (s *Server) moveOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	num := r.FormValue("n")
	q, err := s.svc.QuotationByNumber(ctx, num)
	if err != nil || q == nil {
		s.fail(w, r, fmt.Errorf("ordine %q non trovato", num))
		return
	}
	to, err := url.ParseQuery(r.FormValue("to"))
	if err != nil || to.Get("chat") == "" {
		s.fail(w, r, fmt.Errorf("scegli un'inserzione"))
		return
	}
	camp, err := s.svc.FindCampaign(ctx, to.Get("chat"), to.Get("c"))
	if err != nil || camp == nil {
		s.fail(w, r, fmt.Errorf("inserzione non trovata"))
		return
	}
	if err := s.svc.MoveQuotation(ctx, q.ID, camp); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Redirect", "/ordini/view?"+url.Values{"n": {num}}.Encode())
}

// orderLinkForm opens the list of listings to connect a manual wine to.
func (s *Server) orderLinkForm(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	qid, err := strconv.ParseInt(qs.Get("q"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// inserzioni published from 6 months before the order date
	camps, err := s.svc.LinkCampaigns(r.Context(), s.svc.LinkWindowStart(r.Context(), qid))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.LinkModal(qid, qs.Get("option"), qs.Get("vino"), camps))
}

// orderLink connects (or, with to_q=0, disconnects) a manual wine to a
// listing's option, then reloads the order.
func (s *Server) orderLink(w http.ResponseWriter, r *http.Request) {
	qid, err := strconv.ParseInt(r.FormValue("q"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	to, _ := strconv.ParseInt(r.FormValue("to_q"), 10, 64)
	if err := s.svc.SetItemLink(r.Context(), qid, r.FormValue("option"), to, r.FormValue("to_opt")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

// initialStock saves the initial quantity of one option of the listing.
func (s *Server) initialStock(w http.ResponseWriter, r *http.Request) {
	camp, err := s.svc.FindCampaign(r.Context(), r.FormValue("chat"), r.FormValue("c"))
	if err == nil && camp == nil {
		err = fmt.Errorf("inserzione non trovata")
	}
	if err == nil {
		err = s.svc.SetInitialQty(r.Context(), camp, r.FormValue("letter"), r.FormValue("value"))
	}
	autosaveResult(w, r, err)
}

// moveReplyForm opens the "Sposta in un'altra inserzione" modal.
func (s *Server) moveReplyForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	qs := r.URL.Query()
	d := views.InserzioniData{SelChat: qs.Get("chat"), SelKey: qs.Get("c")}
	if err := s.loadSelection(r, &d); err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.svc.InserzioniList(r.Context(), "", "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, x := range d.Replies {
		if x.ID == id {
			render(w, r, views.MoveReplyModal(d, x, rows))
			return
		}
	}
	s.fail(w, r, fmt.Errorf("messaggio non trovato in questa inserzione"))
}

// moveReply files the reply under the chosen campaign and opens it there.
func (s *Server) moveReply(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	to, err := url.ParseQuery(r.FormValue("to"))
	if err != nil || to.Get("chat") == "" {
		s.fail(w, r, fmt.Errorf("scegli un'inserzione"))
		return
	}
	camp, err := s.svc.FindCampaign(r.Context(), to.Get("chat"), to.Get("c"))
	if err != nil || camp == nil {
		s.fail(w, r, fmt.Errorf("inserzione non trovata"))
		return
	}
	if err := s.svc.MoveReply(r.Context(), id, camp); err != nil {
		s.fail(w, r, err)
		return
	}
	target := "/inserzioni?" + url.Values{"chat": {camp.ChatID}, "c": {camp.Key}}.Encode()
	w.Header().Set("HX-Redirect", target+"#reply-"+strconv.FormatInt(id, 10))
}

// reviewStart launches the LLM check of a campaign's replies.
func (s *Server) reviewStart(w http.ResponseWriter, r *http.Request) {
	d := views.InserzioniData{SelChat: r.FormValue("chat"), SelKey: r.FormValue("c")}
	if !s.reviews.Start(r.Context(), d.SelChat, d.SelKey) {
		toast(w, r, "warning", "È già in corso un controllo: attendi che finisca.")
	}
	if err := s.loadSelection(r, &d); err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.ReviewBar(d))
}

// reviewStatus is polled while a check runs; when it is over, the whole
// conversation is redrawn with the flagged replies.
func (s *Server) reviewStatus(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	d := views.InserzioniData{SelChat: qs.Get("chat"), SelKey: qs.Get("c")}
	if err := s.loadSelection(r, &d); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.ReviewJob.Running {
		render(w, r, views.ReviewBar(d))
		return
	}
	w.Header().Set("HX-Retarget", "#conv")
	w.Header().Set("HX-Reswap", "innerHTML")
	render(w, r, views.Conversation(d))
}

// replyOrder acts on a reply's check: apply the (edited) proposed order,
// mark it as not an order, dismiss the check, or undo a correction. The
// customer's replies, the counters and the filter are refreshed out-of-band.
func (s *Server) replyOrder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	d := views.InserzioniData{SelChat: r.FormValue("chat"), SelKey: r.FormValue("c")}
	if err := s.loadSelection(r, &d); err != nil {
		s.fail(w, r, err)
		return
	}
	var reply *service.ReplyView
	for i := range d.Replies {
		if d.Replies[i].ID == id {
			reply = &d.Replies[i]
		}
	}
	if d.Selected == nil || reply == nil {
		s.fail(w, r, fmt.Errorf("messaggio non trovato in questa inserzione"))
		return
	}
	who := reply.Utente
	modal := r.FormValue("modal") == "1"
	var msg string
	switch r.FormValue("op") {
	case "apply":
		var catalog []service.CatalogEntry
		if modal {
			if catalog, err = s.svc.Catalog(ctx); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		add, extra, rows, err := newOptionRows(r, d.Review, catalog)
		var codes string
		if err == nil {
			codes, err = proposalCodes(r, d.Review, extra)
		}
		if err != nil {
			if modal {
				// the error shows inside the modal, which stays open
				w.Header().Set("HX-Retarget", "#modal")
				w.Header().Set("HX-Reswap", "innerHTML")
				msg := err.Error()
				if errors.Is(err, errNoQuantity) {
					msg = "Indica la quantità di almeno un vino."
				}
				d.Catalog = catalog
				render(w, r, views.ReplyOrderModal(d, *reply, formQty(r, d.Review), rows, msg))
				return
			}
			s.fail(w, r, err)
			return
		}
		err = s.svc.SetReplyOrderAddingOptions(ctx, id, d.Review.QuotationID, add, codes)
		msg = "Ordine aggiornato: " + who + " · " + codes
		if err == nil && len(add) > 0 {
			// new options change how other replies read and the warning:
			// redraw the whole conversation
			var keys []string
			for _, o := range add {
				keys = append(keys, o.Key())
			}
			if err := s.loadSelection(r, &d); err != nil {
				s.fail(w, r, err)
				return
			}
			w.Header().Set("HX-Retarget", "#conv")
			w.Header().Set("HX-Reswap", "innerHTML")
			render(w, r, views.Conversation(d))
			render(w, r, views.ModalSlot(true))
			toast(w, r, "success", msg+" · aggiunte all'ordine le opzioni "+strings.Join(keys, ", "))
			return
		}
	case "notorder":
		err = s.svc.SetReplyOrder(ctx, id, "-")
		msg = "Non è un ordine: " + who
	case "dismiss":
		err = s.svc.DismissReplyCheck(ctx, id)
		msg = "Segnalazione ignorata: " + who
	case "reset":
		err = s.svc.ResetReplyOrder(ctx, id)
		msg = "Correzione annullata: " + who
	default:
		err = fmt.Errorf("azione sconosciuta")
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.loadSelection(r, &d); err != nil {
		s.fail(w, r, err)
		return
	}
	// the customer's current order shows in each of their flagged replies
	for _, x := range d.Replies {
		if x.ID == id || (x.AuthorID == reply.AuthorID && x.Check.Open()) {
			render(w, r, views.ReplyItem(d, x, true))
		}
	}
	render(w, r, views.ReplyCounts(d, true))
	render(w, r, views.ReviewFilter(d, true))
	if modal {
		render(w, r, views.ModalSlot(true))
	}
	toast(w, r, "success", msg)
}

// replyOrderForm opens the "Conferma come ordine" modal of a reply the rules
// didn't read as an order. A campaign without a quotation gets it first.
func (s *Server) replyOrderForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	qs := r.URL.Query()
	d := views.InserzioniData{SelChat: qs.Get("chat"), SelKey: qs.Get("c")}
	if err := s.loadSelection(r, &d); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Selected != nil && d.Review != nil && d.Review.QuotationID == 0 && d.Review.Orderable {
		if _, err := s.svc.EnsureQuotation(r.Context(), d.Selected); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.loadSelection(r, &d); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if d.Catalog, err = s.svc.Catalog(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	for _, x := range d.Replies {
		if x.ID == id {
			render(w, r, views.ReplyOrderModal(d, x, d.Review.GuessQty(x.Testo), missingOptionRows(d.Review, x.Testo), ""))
			return
		}
	}
	s.fail(w, r, fmt.Errorf("messaggio non trovato in questa inserzione"))
}

// formQty reads back the quantities typed in the editor (to redraw it).
func formQty(r *http.Request, rv *service.CampaignReview) map[string]int {
	out := map[string]int{}
	if rv == nil {
		return out
	}
	for _, o := range rv.Options {
		if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("qty_" + o.Key))); err == nil {
			out[o.Key] = n
		}
	}
	return out
}

// missingOptionRows: a row for each option of the listing that the order
// lacks, with the listing's price and, when the reply names that option
// ("una cassa B"), its quantity. The wine is left to pick from Prodotti.
func missingOptionRows(rv *service.CampaignReview, body string) []views.NewOptionRow {
	if rv == nil {
		return nil
	}
	named := map[string]int{}
	for _, c := range textutil.ParseOrders(body) {
		k := c.Option
		if c.Case {
			k += textutil.CaseSuffix
		}
		named[k] = c.Qty
	}
	var out []views.NewOptionRow
	for _, o := range rv.Missing {
		row := views.NewOptionRow{Letter: o.Letter, Case: o.Case, Price: strconv.FormatFloat(o.Price, 'f', 2, 64),
			Hint: fmt.Sprintf("%s a € %.2f", o.WineName, o.Price)}
		k := o.Letter
		if o.Case {
			k += textutil.CaseSuffix
		}
		if n, ok := named[k]; ok && n > 0 {
			row.Qty = strconv.Itoa(n)
		}
		out = append(out, row)
	}
	return out
}

// newOptionRows reads the options typed in the confirm modal (new_letter,
// new_case, new_wine, new_price, new_qty). A row left empty is ignored; a
// filled one needs a wine from Prodotti, a price and a quantity, and a letter
// the order doesn't use yet. Returns the options to add, their order codes
// and the rows as typed (to redraw the modal on errors).
func newOptionRows(r *http.Request, rv *service.CampaignReview, catalog []service.CatalogEntry) (add []service.NewOption, codes []textutil.OrderCode, rows []views.NewOptionRow, err error) {
	if err := r.ParseForm(); err != nil {
		return nil, nil, nil, err
	}
	at := func(vals []string, i int) string {
		if i < len(vals) {
			return strings.TrimSpace(vals[i])
		}
		return ""
	}
	letters := r.Form["new_letter"]
	used := map[string]bool{}
	if rv != nil {
		for _, o := range rv.Options {
			used[o.Key] = true
		}
	}
	for i := range letters {
		row := views.NewOptionRow{Letter: strings.ToUpper(at(letters, i)), Case: at(r.Form["new_case"], i) == "1",
			Wine: at(r.Form["new_wine"], i), Price: at(r.Form["new_price"], i), Qty: at(r.Form["new_qty"], i), Hint: at(r.Form["new_hint"], i)}
		rows = append(rows, row)
		if err != nil || (row.Wine == "" && (row.Qty == "" || row.Qty == "0")) {
			continue // untouched row, or already failed
		}
		o := service.NewOption{Letter: row.Letter, Case: row.Case}
		label := row.Letter
		if row.Case {
			label = "cassa " + row.Letter
		}
		if len(o.Letter) != 1 || o.Letter < "A" || o.Letter > "J" {
			err = fmt.Errorf("lettera dell'opzione non valida: %q", row.Letter)
			continue
		}
		if used[o.Key()] {
			err = fmt.Errorf("l'opzione %s c'è già nell'ordine", label)
			continue
		}
		name, _, _ := strings.Cut(row.Wine, " — ")
		for _, e := range catalog {
			if strings.EqualFold(e.Description, strings.TrimSpace(name)) {
				o.Wine = e.Description
				break
			}
		}
		price, perr := strconv.ParseFloat(strings.ReplaceAll(row.Price, ",", "."), 64)
		qty, qerr := strconv.Atoi(row.Qty)
		switch {
		case row.Wine == "":
			err = fmt.Errorf("scegli il vino dell'opzione %s dai Prodotti", label)
		case o.Wine == "":
			err = fmt.Errorf("“%s” non è tra i Prodotti: sceglilo dall'elenco (o aggiungilo prima nella pagina Prodotti)", row.Wine)
		case perr != nil || price <= 0:
			err = fmt.Errorf("indica il prezzo dell'opzione %s", label)
		case qerr != nil || qty < 1 || qty > 999:
			err = fmt.Errorf("indica la quantità dell'opzione %s", label)
		default:
			o.Price = price
			used[o.Key()] = true
			add = append(add, o)
			codes = append(codes, textutil.OrderCode{Qty: qty, Option: o.Letter, Case: o.Case})
		}
	}
	if err != nil {
		return nil, nil, rows, err
	}
	return add, codes, rows, nil
}

var errNoQuantity = errors.New(`indica almeno una quantità, oppure scegli "Non è un ordine"`)

// proposalCodes reads the quantities of the proposal editor (qty_<option>):
// empty = option not touched, 0 = cancelled.
func proposalCodes(r *http.Request, rv *service.CampaignReview, extra []textutil.OrderCode) (string, error) {
	if rv == nil || len(rv.Options)+len(extra) == 0 {
		return "", fmt.Errorf("l'inserzione non ha opzioni ordinabili")
	}
	codes := append([]textutil.OrderCode(nil), extra...)
	for _, o := range rv.Options {
		v := strings.TrimSpace(r.FormValue("qty_" + o.Key))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 999 {
			return "", fmt.Errorf("quantità non valida per %s: %q", o.Label(), v)
		}
		codes = append(codes, o.Code(n))
	}
	if len(codes) == 0 {
		return "", errNoQuantity
	}
	return textutil.FormatOrderCodes(codes), nil
}

// ── Preventivi ───────────────────────────────────────────────────────────────

func (s *Server) preventivi(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Entering the page imports new quotations automatically, like the
	// Streamlit page did.
	flash := ""
	if created, _, _, err := s.svc.ImportPreventivi(ctx); err != nil {
		s.fail(w, r, err)
		return
	} else if created > 0 {
		flash = fmt.Sprintf("Importazione automatica: creati %d ordini", created)
	}
	q := r.URL.Query().Get("q")
	list, err := s.preventiviListComponent(r, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.PreventiviPage(q, flash, list))
}

func (s *Server) preventiviListComponent(r *http.Request, q string) (templ.Component, error) {
	ctx := r.Context()
	if strings.TrimSpace(q) != "" {
		sel, err := s.svc.SearchableSelections(ctx)
		if err != nil {
			return nil, err
		}
		return views.PreventiviSearch(service.SearchSelections(sel, q)), nil
	}
	rows, err := s.svc.QuotationSummaries(ctx)
	if err != nil {
		return nil, err
	}
	return views.PreventiviSummary(service.QuotationMonths(rows)), nil
}

func (s *Server) preventiviList(w http.ResponseWriter, r *http.Request) {
	c, err := s.preventiviListComponent(r, r.URL.Query().Get("q"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, c)
}

func (s *Server) preventiviImport(w http.ResponseWriter, r *http.Request) {
	created, skipped, ok, err := s.svc.ImportPreventivi(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.preventiviListComponent(r, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, c)
	if !ok {
		toast(w, r, "warning", "Nessuna inserzione disponibile da importare.")
		return
	}
	toast(w, r, "success", fmt.Sprintf("Creati %d ordini · %d già presenti o senza opzioni", created, skipped))
}

func (s *Server) preventivoView(w http.ResponseWriter, r *http.Request) {
	d, err := s.svc.QuotationDetail(r.Context(), r.URL.Query().Get("n"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if d == nil {
		http.Redirect(w, r, "/ordini", http.StatusSeeOther)
		return
	}
	for i := range d.Sections {
		s.useSavedRows(r, d.Quotation.Number, &d.Sections[i])
	}
	// the listing it comes from and the orders next to it in the list
	if c, err := s.svc.QuotationListing(r.Context(), d.Quotation.ID); err != nil {
		s.fail(w, r, err)
		return
	} else if c != nil {
		d.ListingURL, d.ListingTitle = service.CampaignURL(c.ChatID, c.Key), c.DisplayTitle
	}
	sums, err := s.svc.QuotationSummaries(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d.Prev, d.Next = service.QuotationNeighbors(sums, d.Quotation.Number)
	catalog, err := s.svc.Catalog(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.PreventivoDetail(d, catalog))
}

// useSavedRows shows the saved order lines (not the live selections) once a
// customer's order has been created.
func (s *Server) useSavedRows(r *http.Request, quotNum string, sec *service.CustomerSection) {
	if sec.SavedOn == "" {
		return
	}
	if rows, err := s.svc.SavedOrderRows(r.Context(), quotNum, sec.Cliente); err == nil && len(rows) > 0 {
		sec.Rows = rows
	}
}

// preventivoSection handles every action of a customer section. The draft
// rows travel with the form (hidden inputs), so the server stays stateless.
func (s *Server) preventivoSection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	num, utente, cliente := r.FormValue("n"), r.FormValue("utente"), r.FormValue("cliente")
	q, err := s.svc.QuotationByNumber(ctx, num)
	if err != nil || q == nil {
		s.fail(w, r, fmt.Errorf("ordine %q non trovato", num))
		return
	}
	rows := parseOrderRows(r)
	op := r.FormValue("op")
	msg, errMsg, clientErr := "", "", ""
	switch {
	case op == "cliente":
		// the new customer must be one of the choices (or the detected name)
		next := strings.TrimSpace(r.FormValue("new_cliente"))
		byName, _, err := s.svc.ClientOptions(ctx)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if _, known := byName[next]; next == "" || (!known && next != utente) {
			clientErr = "Cliente non trovato: " + next + ". Sceglilo dall'elenco."
			if next == "" {
				clientErr = "Scegli un cliente dall'elenco."
			}
			break
		}
		if err := s.svc.SetManualClient(ctx, num, next); err != nil {
			s.fail(w, r, err)
			return
		}
		cliente = next
		msg = "Cliente aggiornato: " + next
	case op == "add":
		// the wine must be one of the Prodotti (datalist labels may carry " — winery")
		name, _, _ := strings.Cut(strings.TrimSpace(r.FormValue("new_vino")), " — ")
		name = strings.TrimSpace(name)
		qty, _ := strconv.Atoi(r.FormValue("new_qta"))
		priceStr := strings.ReplaceAll(strings.TrimSpace(r.FormValue("new_prezzo")), ",", ".")
		price, perr := strconv.ParseFloat(priceStr, 64)
		wine := ""
		if catalog, err := s.svc.Catalog(ctx); err == nil {
			for _, e := range catalog {
				if strings.EqualFold(e.Description, name) {
					wine = e.Description
				}
			}
		}
		switch {
		case name == "":
			errMsg = "Scegli un vino: scrivi per cercarlo tra i Prodotti."
		case wine == "":
			errMsg = "“" + name + "” non è tra i Prodotti: aggiungilo prima nella pagina Prodotti."
		case priceStr == "" || perr != nil || price < 0:
			errMsg = "Inserisci un prezzo valido."
		default:
			if qty < 1 {
				qty = 1
			}
			rows = append(rows, service.OrderRow{Vino: wine, Qta: qty, Prezzo: price})
		}
	case strings.HasPrefix(op, "del:"):
		if i, err := strconv.Atoi(strings.TrimPrefix(op, "del:")); err == nil && i >= 0 && i < len(rows) {
			rows = append(rows[:i], rows[i+1:]...)
		}
	case op == "save":
		if err := s.svc.SaveOrder(ctx, num, cliente, rows); err != nil {
			s.fail(w, r, err)
			return
		}
		msg = "Ordine per " + cliente + " salvato!"
	}
	sec, err := s.svc.BuildSection(ctx, q, utente, cliente, rows)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.useSavedRows(r, num, &sec)
	render(w, r, views.CustomerSection(num, sec, msg, errMsg, clientErr))
}

func parseOrderRows(r *http.Request) []service.OrderRow {
	opz, vino, qta, prezzo := r.Form["opz"], r.Form["vino"], r.Form["qta"], r.Form["prezzo"]
	var rows []service.OrderRow
	for i := range vino {
		row := service.OrderRow{Vino: vino[i]}
		if i < len(opz) {
			row.Opzione = opz[i]
		}
		if i < len(qta) {
			row.Qta, _ = strconv.Atoi(qta[i])
		}
		if i < len(prezzo) {
			row.Prezzo, _ = strconv.ParseFloat(prezzo[i], 64)
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) nuovoData(r *http.Request) (views.NuovoPreventivoData, error) {
	ctx := r.Context()
	_, clients, err := s.svc.ClientOptions(ctx)
	if err != nil {
		return views.NuovoPreventivoData{}, err
	}
	catalog, err := s.svc.Catalog(ctx)
	if err != nil {
		return views.NuovoPreventivoData{}, err
	}
	return views.NuovoPreventivoData{
		Number: s.svc.NextIMPNumber(ctx), Date: time.Now().Format("2006-01-02"),
		Clients: clients, Catalog: catalog,
		Rows: []service.NewQuotationRow{{Option: "A", Quantity: 1}},
	}, nil
}

func (s *Server) nuovoPreventivo(w http.ResponseWriter, r *http.Request) {
	d, err := s.nuovoData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.NuovoPreventivo(d))
}

func (s *Server) nuovoPreventivoSubmit(w http.ResponseWriter, r *http.Request) {
	d, err := s.nuovoData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	r.ParseForm()
	d.Date = r.FormValue("date")
	d.Cliente = strings.TrimSpace(r.FormValue("cliente"))
	vino, qta, prezzo := r.Form["vino"], r.Form["qta"], r.Form["prezzo"]
	letters := "ABCDEFGHIJ"
	d.Rows = nil
	var valid []service.NewQuotationRow
	for i := 0; i < len(vino) && i < len(letters); i++ {
		// catalog labels are "description — winery"
		name, _, _ := strings.Cut(vino[i], " — ")
		row := service.NewQuotationRow{Option: string(letters[i]), WineName: strings.TrimSpace(name), Quantity: 1}
		if i < len(qta) {
			if v, err := strconv.Atoi(qta[i]); err == nil && v >= 1 {
				row.Quantity = v
			}
		}
		if i < len(prezzo) {
			row.Price, _ = strconv.ParseFloat(strings.ReplaceAll(prezzo[i], ",", "."), 64)
		}
		d.Rows = append(d.Rows, row)
		if row.WineName != "" && row.Price > 0 {
			valid = append(valid, row)
		}
	}
	if _, err := time.Parse("2006-01-02", d.Date); err != nil {
		d.Error = "Data non valida."
	} else if d.Cliente == "" {
		d.Error = "Seleziona un cliente per poter creare l'ordine."
	} else if !slices.Contains(d.Clients, d.Cliente) {
		d.Error = "Cliente non trovato: " + d.Cliente + ". Sceglilo dall'elenco."
	} else if len(valid) == 0 {
		d.Error = "Inserisci almeno un vino con un prezzo maggiore di zero."
	}
	if d.Error != "" {
		if len(d.Rows) == 0 {
			d.Rows = []service.NewQuotationRow{{Option: "A", Quantity: 1}}
		}
		render(w, r, views.NuovoPreventivo(d))
		return
	}
	num, err := s.svc.CreateQuotation(r.Context(), d.Date, d.Cliente, valid)
	if err != nil {
		d.Error = "Errore salvataggio: " + err.Error()
		render(w, r, views.NuovoPreventivo(d))
		return
	}
	http.Redirect(w, r, "/ordini/view?n="+num, http.StatusSeeOther)
}

// ── Consegne ─────────────────────────────────────────────────────────────────

func (s *Server) consegne(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	camps, err := s.svc.ConsegneCampaigns(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids := r.URL.Query()["sel"]
	res, err := s.svc.Consegne(ctx, ids)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sel := map[string]bool{}
	for _, id := range ids {
		sel[id] = true
	}
	// two tabs: the listings (default) and the manual orders
	manual := r.URL.Query().Get("vista") == "ordini"
	var shown, hidden []service.ConsegneCampaign
	for _, c := range camps {
		if c.Manual == manual {
			shown = append(shown, c)
		} else if sel[c.ID] {
			hidden = append(hidden, c) // selected in the other tab: kept in the selection
		}
	}
	render(w, r, views.ConsegnePage(service.ConsegneMonths(shown), sel, res, ids, manual, hidden))
}

func (s *Server) consegneResult(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query()["sel"]
	res, err := s.svc.Consegne(r.Context(), ids)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := url.Values{"sel": ids}
	if r.URL.Query().Get("vista") == "ordini" {
		q.Set("vista", "ordini")
	}
	w.Header().Set("HX-Replace-Url", "/consegne?"+q.Encode())
	render(w, r, views.ConsegneSelBar(res, ids))
}

func (s *Server) consegneShow(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query()["sel"]
	res, err := s.svc.Consegne(r.Context(), ids)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.ConsegneShowPage(res, ids))
}

func (s *Server) consegneExcel(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Consegne(r.Context(), r.URL.Query()["sel"])
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data, err := service.ConsegneExcel(res.ExportRows)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="consegne_%s.xlsx"`, time.Now().Format("2006-01-02")))
	w.Write(data)
}

// ── Clienti / Prodotti / Statistiche ─────────────────────────────────────────

func (s *Server) clienti(w http.ResponseWriter, r *http.Request) {
	users, err := s.svc.Users(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.ClientiPage(users))
}

// autosaveResult answers an autosave request: 204 when saved; on error a 200
// with X-Autosave-Error (htmx ignores error statuses) and a toast saying why.
func autosaveResult(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		w.Header().Set("X-Autosave-Error", "1")
		w.Header().Set("HX-Reswap", "none")
		toast(w, r, "error", "Non salvato: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clienteField saves one field of one customer (autosave from the Clienti page).
// A value saved differently from how it was typed ("pd" → "Padova") is sent
// back in X-Autosave-Value, and app.js shows it in the field.
func (s *Server) clienteField(w http.ResponseWriter, r *http.Request) {
	saved, err := s.svc.SetUserField(r.Context(), r.FormValue("id"), r.FormValue("field"), r.FormValue("value"))
	if err == nil && saved != r.FormValue("value") {
		w.Header().Set("X-Autosave-Value", url.PathEscape(saved))
	}
	autosaveResult(w, r, err)
}

// clienteNew creates an empty customer and returns its row (prepended to the
// table, cursor in Nome).
func (s *Server) clienteNew(w http.ResponseWriter, r *http.Request) {
	u, err := s.svc.CreateManualUser(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.ClienteRow(u, true))
	toast(w, r, "success", "Nuovo cliente aggiunto: compila i campi, si salvano da soli.")
}

// customerCreate adds a customer from the "Nuovo cliente" modal; the page
// picks it up (event clienteCreato) and selects it.
func (s *Server) customerCreate(w http.ResponseWriter, r *http.Request) {
	u, err := s.svc.CreateCustomer(r.Context(), service.User{
		Name: r.FormValue("name"), Telefono: r.FormValue("telefono"), Indirizzo: r.FormValue("indirizzo"),
		Citta: r.FormValue("citta"), Provincia: r.FormValue("provincia"), CAP: r.FormValue("cap"),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ev, _ := json.Marshal(map[string]any{"clienteCreato": map[string]string{"name": u.Name}})
	w.Header().Set("HX-Trigger", string(ev))
	toast(w, r, "success", "Nuovo cliente aggiunto: "+u.Name)
}

// productCreate adds a product from the "Nuovo prodotto" modal; the page
// picks it up (event prodottoCreato) and puts it in the wine field.
func (s *Server) productCreate(w http.ResponseWriter, r *http.Request) {
	it, err := s.svc.CreateProduct(r.Context(), service.Item{
		Description: r.FormValue("description"), Winery: r.FormValue("winery"), Area: r.FormValue("area"),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	label := service.CatalogEntry{Description: it.Description, Winery: it.Winery}.Label()
	ev, _ := json.Marshal(map[string]any{"prodottoCreato": map[string]string{"name": label}})
	w.Header().Set("HX-Trigger", string(ev))
	toast(w, r, "success", "Nuovo prodotto aggiunto: "+it.Description+" ("+it.Code+")")
}

// mergePage shows the possible duplicates among the customers.
func (s *Server) mergePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sugg, err := s.svc.MergeSuggestions(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	merged, err := s.svc.MergedCustomers(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	_, names, err := s.svc.ClientOptions(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.MergePage(sugg, merged, names))
}

// mergeCustomers merges one customer into another (by id, from a
// suggestion, or by name, from the free form) and reloads the page.
func (s *Server) mergeCustomers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	alias, main := r.FormValue("alias_id"), r.FormValue("main_id")
	if alias == "" || main == "" {
		byName, _, err := s.svc.ClientOptions(ctx)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		var ok1, ok2 bool
		alias, ok1 = byName[strings.TrimSpace(r.FormValue("alias"))]
		main, ok2 = byName[strings.TrimSpace(r.FormValue("main"))]
		if !ok1 || !ok2 {
			s.fail(w, r, fmt.Errorf("scegli entrambi i clienti dall'elenco"))
			return
		}
	}
	if err := s.svc.MergeCustomers(ctx, alias, main); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

// unmergeCustomer makes a merged id a customer of its own again.
func (s *Server) unmergeCustomer(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.UnmergeCustomer(r.Context(), r.FormValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) clienteDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteUser(r.Context(), r.FormValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	toast(w, r, "success", "Cliente eliminato.")
}

func (s *Server) prodotti(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.Items(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.ProdottiPage(items))
}

// prodottoField saves one field of one product (autosave from Prodotti).
func (s *Server) prodottoField(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		autosaveResult(w, r, fmt.Errorf("prodotto non valido"))
		return
	}
	autosaveResult(w, r, s.svc.SetItemField(r.Context(), id, r.FormValue("field"), r.FormValue("value")))
}

func (s *Server) prodottoNew(w http.ResponseWriter, r *http.Request) {
	it, err := s.svc.CreateItem(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.ProdottoRow(it, true))
	toast(w, r, "success", "Nuovo prodotto "+it.Code+": scrivi la descrizione, si salva da sola.")
}

func (s *Server) prodottoDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err == nil {
		err = s.svc.DeleteItem(r.Context(), id)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	toast(w, r, "success", "Prodotto eliminato.")
}

func (s *Server) statistiche(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.Stats(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.StatistichePage(st))
}
