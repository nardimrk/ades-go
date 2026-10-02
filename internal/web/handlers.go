package web

import (
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
		codes, err := proposalCodes(r, d.Review)
		if err != nil {
			if modal {
				// the error shows inside the modal, which stays open
				w.Header().Set("HX-Retarget", "#modal")
				w.Header().Set("HX-Reswap", "innerHTML")
				msg := err.Error()
				if errors.Is(err, errNoQuantity) {
					msg = "Indica la quantità di almeno un vino."
				}
				render(w, r, views.ReplyOrderModal(d, *reply, formQty(r, d.Review), msg))
				return
			}
			s.fail(w, r, err)
			return
		}
		err = s.svc.SetReplyOrder(ctx, id, codes)
		msg = "Ordine aggiornato: " + who + " · " + codes
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
	for _, x := range d.Replies {
		if x.ID == id {
			render(w, r, views.ReplyOrderModal(d, x, d.Review.GuessQty(x.Testo), ""))
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

var errNoQuantity = errors.New(`indica almeno una quantità, oppure scegli "Non è un ordine"`)

// proposalCodes reads the quantities of the proposal editor (qty_<option>):
// empty = option not touched, 0 = cancelled.
func proposalCodes(r *http.Request, rv *service.CampaignReview) (string, error) {
	if rv == nil || len(rv.Options) == 0 {
		return "", fmt.Errorf("l'inserzione non ha opzioni ordinabili")
	}
	var codes []textutil.OrderCode
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
	render(w, r, views.ConsegnePage(service.ConsegneMonths(camps), sel, res, ids))
}

func (s *Server) consegneResult(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query()["sel"]
	res, err := s.svc.Consegne(r.Context(), ids)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Replace-Url", "/consegne?"+url.Values{"sel": ids}.Encode())
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
func (s *Server) clienteField(w http.ResponseWriter, r *http.Request) {
	autosaveResult(w, r, s.svc.SetUserField(r.Context(), r.FormValue("id"), r.FormValue("field"), r.FormValue("value")))
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

func (s *Server) clienteDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteManualUser(r.Context(), r.FormValue("id")); err != nil {
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
