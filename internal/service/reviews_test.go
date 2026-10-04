package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"adesgo/internal/config"
	"adesgo/internal/db"
)

func TestReplyOrderOverride(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)

	const chat = "1@g.us"
	listing := "Barolo e Barbaresco\nA. 6x Barolo 2019 a 40€\nB. 6x Barbaresco 2018 a 30€"
	if err := store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: listing, Timestamp: 1000}); err != nil {
		t.Fatal(err)
	}
	lid := "L1"
	for i, body := range []string{"3A", "e anche due dell'altro"} {
		if err := store.InsertReply(ctx, db.Reply{MsgID: "R" + itoa(i), ListingMsgID: &lid, ChatID: chat, AuthorID: "u1", Body: body, Timestamp: int64(1100 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := svc.ImportPreventivi(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.InserzioniList(ctx, "", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("campaigns = %v, %v", rows, err)
	}
	camp := &rows[0].Campaign

	state := func() (map[string]string, []ReplyView) {
		t.Helper()
		replies, err := svc.CampaignReplies(ctx, camp.MsgIDs)
		if err != nil {
			t.Fatal(err)
		}
		rv, err := svc.CampaignReview(ctx, camp)
		if err != nil {
			t.Fatal(err)
		}
		return rv.Current, replies
	}
	cur, replies := state()
	if cur["u1"] != "3A" || replies[0].Reading != "3A" || replies[1].Reading != "" {
		t.Fatalf("before: current %v, readings %q %q", cur, replies[0].Reading, replies[1].Reading)
	}

	if err := svc.SetReplyOrder(ctx, replies[1].ID, "2B"); err != nil {
		t.Fatal(err)
	}
	if cur, replies = state(); cur["u1"] != "3A, 2B" || replies[1].Reading != "2B" || replies[1].Override != "2B" {
		t.Errorf("after 2B: current %q, reading %q", cur["u1"], replies[1].Reading)
	}
	if err := svc.SetReplyOrder(ctx, replies[0].ID, "-"); err != nil {
		t.Fatal(err)
	}
	if cur, replies = state(); cur["u1"] != "2B" || replies[0].Ordine {
		t.Errorf("after not-an-order: current %q, ordine %v", cur["u1"], replies[0].Ordine)
	}
	if err := svc.SetReplyOrder(ctx, replies[1].ID, "0B"); err != nil {
		t.Fatal(err)
	}
	if cur, _ = state(); cur["u1"] != "" {
		t.Errorf("after 0B: current %q, want none", cur["u1"])
	}
	if err := svc.ResetReplyOrder(ctx, replies[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResetReplyOrder(ctx, replies[1].ID); err != nil {
		t.Fatal(err)
	}
	if cur, _ = state(); cur["u1"] != "3A" {
		t.Errorf("after reset: current %q, want 3A", cur["u1"])
	}

	// LLM findings: unknown options are dropped, settled replies left alone
	rv, _ := svc.CampaignReview(ctx, camp)
	_, replies = state()
	n, err := svc.saveChecks(ctx, rv, replies, 0, []reviewFinding{
		{N: 1, Tipo: "dubbia", Motivo: "uguale alla lettura", Proposta: "3A"},
		{N: 2, Tipo: "Ambigua", Motivo: "non dice quale", Proposta: "2B, 5C"},
		{N: 9, Tipo: "dubbia"},
	})
	if err != nil || n != 1 {
		t.Fatalf("saveChecks = %d, %v; want 1", n, err)
	}
	_, replies = state()
	if replies[0].Check != nil || !replies[1].Check.Open() || replies[1].Check.Proposal != "2B" || replies[1].Check.Kind != "ambigua" {
		t.Errorf("checks: %+v %+v", replies[0].Check, replies[1].Check)
	}
	if err := svc.DismissReplyCheck(ctx, replies[1].ID); err != nil {
		t.Fatal(err)
	}
	_, replies = state()
	if n, _ := svc.saveChecks(ctx, rv, replies, 0, []reviewFinding{{N: 2, Tipo: "ambigua"}}); n != 0 {
		t.Errorf("dismissed check flagged again (%d)", n)
	}

	p := reviewPrompt(camp, rv, replies, replies, 0)
	for _, want := range []string{"- A = Barolo, 40.00 euro", "[2] ", "lettura: nessuna", ": 3A"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func itoa(i int) string { return string(rune('0' + i)) }

// A listing with one wine and no letters ("30 x Langoa Barton a 47,50€",
// replies "4" and "2"): the check creates its order with option A.
func TestSingleWineReview(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	if err := store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner",
		Body: "Disponibili:\n\n30 x Château Langoa Barton 2022 a 47,50€", Timestamp: 1000}); err != nil {
		t.Fatal(err)
	}
	lid := "L1"
	for i, body := range []string{"4", "2"} {
		if err := store.InsertReply(ctx, db.Reply{MsgID: "R" + itoa(i), ListingMsgID: &lid, ChatID: chat,
			AuthorID: "u" + itoa(i), Body: body, Timestamp: int64(1100 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := svc.ImportPreventivi(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := svc.InserzioniList(ctx, "", "")
	camp := &rows[0].Campaign
	if len(camp.QuotationIDs) != 0 {
		t.Fatal("the automatic import must leave single-wine listings out")
	}
	if ok, err := svc.EnsureSingleWineQuotation(ctx, camp); !ok || err != nil {
		t.Fatalf("EnsureSingleWineQuotation = %v, %v", ok, err)
	}
	camp, _ = svc.FindCampaign(ctx, chat, camp.Key)
	if ok, _ := svc.EnsureSingleWineQuotation(ctx, camp); ok {
		t.Error("created twice")
	}
	rv, err := svc.CampaignReview(ctx, camp)
	if err != nil || len(rv.Options) != 1 || rv.Options[0].Wine != "Château Langoa Barton 2022" {
		t.Fatalf("options = %+v, %v", rv.Options, err)
	}
	replies, _ := svc.CampaignReplies(ctx, camp.MsgIDs)
	if p := reviewPrompt(camp, rv, replies, replies, 0); !strings.Contains(p, "UNA SOLA opzione (A)") {
		t.Errorf("prompt lacks the single-option rule:\n%s", p)
	}
	if n, err := svc.saveChecks(ctx, rv, replies, 0, []reviewFinding{{N: 1, Tipo: "dubbia", Proposta: "4A"}, {N: 2, Tipo: "dubbia", Proposta: "2"}}); n != 2 || err != nil {
		t.Errorf("saveChecks = %d, %v; want 2", n, err)
	}
	if replies, _ = svc.CampaignReplies(ctx, camp.MsgIDs); replies[1].Check == nil || replies[1].Check.Proposal != "2A" {
		t.Errorf("a bare \"2\" from the model should become 2A: %+v", replies[1].Check)
	}
	if err := svc.SetReplyOrder(ctx, replies[0].ID, "4A"); err != nil {
		t.Fatal(err)
	}
	if rv, _ = svc.CampaignReview(ctx, camp); rv.Current["u0"] != "4A" {
		t.Errorf("current = %v", rv.Current)
	}
}

func TestGuessQty(t *testing.T) {
	one := &CampaignReview{Options: []OrderOption{{Key: "A", Letter: "A"}, {Key: "A-CASSA", Letter: "A", Case: true}}}
	two := &CampaignReview{Options: []OrderOption{{Key: "A", Letter: "A"}, {Key: "B", Letter: "B"}}}
	for _, c := range []struct {
		rv   *CampaignReview
		body string
		want int // quantity of A, 0 = no prefill
	}{
		{one, "4", 4}, {one, "ne prendo 4 grazie", 4}, {one, "2 o 3", 0}, {one, "grazie", 0}, {two, "4", 0},
		// IMP0531: the order has only A, the customer asked for B
		{one, "3 B grazie", 0}, {one, "Ciao 3B grazie", 0}, {one, "2A", 2},
	} {
		if got := c.rv.GuessQty(c.body)["A"]; got != c.want {
			t.Errorf("GuessQty(%q) = %d; want %d", c.body, got, c.want)
		}
	}
	if got := two.GuessQty("3B grazie"); got["B"] != 3 || got["A"] != 0 {
		t.Errorf("GuessQty(3B) on A+B = %v", got)
	}
	if got := one.GuessQty("3 B grazie"); len(got) != 0 {
		t.Errorf("GuessQty(3 B) on A only = %v; want nothing", got)
	}
}

func TestAddManualReply(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)

	const chat = "1@g.us"
	listing := "Barolo\nA. 6x Barolo 2019 a 40€"
	if err := store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: listing, Timestamp: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertUser(ctx, "u1", "Alessio"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.ImportPreventivi(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.InserzioniList(ctx, "", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("campaigns = %v, %v", rows, err)
	}
	camp := &rows[0].Campaign

	if err := svc.AddManualReply(ctx, camp, "u1", "3A", 1200); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddManualReply(ctx, camp, "u1", "3A", 1200); err != ErrDuplicateReply {
		t.Fatalf("second insert: %v, want ErrDuplicateReply", err)
	}
	// a time before the listing (wrong clock, typo) still files it under the campaign
	if err := svc.AddManualReply(ctx, camp, "u1", "e 1A", 900); err != nil {
		t.Fatal(err)
	}
	replies, err := svc.CampaignReplies(ctx, camp.MsgIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 2 || !replies[0].Manual || replies[1].Utente != "Alessio" || replies[1].Reading != "3A" {
		t.Fatalf("replies = %+v", replies)
	}
}

// IMP0531: the order holds the case as 6 bottles at 119/6 and lacks option
// B; the review must say so.
func TestListingMismatch(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	body := "Thanisch Riesling Mosella disponibili:\n\nA. CASSA intera Ürziger Würzgarten Auslese 2003 a 119€\n\nB. 20 x Ürziger Würzgarten Auslese 2003 a 24,50€ (bottiglia sfusa)"
	if err := store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: body, Timestamp: 1000}); err != nil {
		t.Fatal(err)
	}
	opts := []OrderOption{{Key: "A", Letter: "A", Wine: "Ürziger Würzgarten Auslese 2003", Price: 19.83}}
	got, missing, err := svc.listingMismatch(ctx, []string{"L1"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.Contains(got[0], "opzione A") || !strings.Contains(got[0], "119.00") || !strings.Contains(got[1], "manca l'opzione B") {
		t.Fatalf("mismatch = %q", got)
	}
	if len(missing) != 1 || missing[0].Letter != "B" || missing[0].Price != 24.5 {
		t.Fatalf("missing = %+v", missing)
	}
	opts = []OrderOption{{Key: "A", Letter: "A", Price: 119}, {Key: "B", Letter: "B", Price: 24.5}}
	if got, _, _ := svc.listingMismatch(ctx, []string{"L1"}, opts); len(got) != 0 {
		t.Fatalf("matching order flagged: %q", got)
	}
}

// Ordini list: "Confermato il …" only once every customer's order is saved.
func TestSummarySavedOnAllCustomers(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	if err := store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: "Barolo\nA. 6x Barolo 2019 a 40€", Timestamp: 1000}); err != nil {
		t.Fatal(err)
	}
	lid := "L1"
	for i, u := range []string{"u1", "u2"} {
		if err := store.UpsertUser(ctx, u, "Cliente "+u); err != nil {
			t.Fatal(err)
		}
		if err := store.InsertReply(ctx, db.Reply{MsgID: "R" + itoa(i), ListingMsgID: &lid, ChatID: chat, AuthorID: u, Body: "2A", Timestamp: int64(1100 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := svc.ImportPreventivi(ctx); err != nil {
		t.Fatal(err)
	}
	savedOn := func() string {
		t.Helper()
		sums, err := svc.QuotationSummaries(ctx)
		if err != nil || len(sums) != 1 {
			t.Fatalf("summaries = %v, %v", sums, err)
		}
		return sums[0].SavedOn
	}
	num := ""
	if sums, _ := svc.QuotationSummaries(ctx); len(sums) == 1 {
		num = sums[0].Number
	}
	rows := []OrderRow{{Opzione: "A", Vino: "Barolo", Qta: 2, Prezzo: 40}}
	if got := savedOn(); got != "" {
		t.Fatalf("nothing saved: SavedOn = %q", got)
	}
	if err := svc.SaveOrder(ctx, num, "Cliente u1", rows); err != nil {
		t.Fatal(err)
	}
	if got := savedOn(); got != "" {
		t.Fatalf("1 of 2 saved: SavedOn = %q, want none", got)
	}
	if err := svc.SaveOrder(ctx, num, "Cliente u2", rows); err != nil {
		t.Fatal(err)
	}
	if got := savedOn(); got == "" {
		t.Fatal("2 of 2 saved: no SavedOn")
	}
}

// Confirm modal: a missing option is added to the quotation together with
// the reply's order; adding an option that exists fails and changes nothing.
func TestSetReplyOrderAddingOptions(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	if err := store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: "Matallana\nA. 11x Matallana 2023 a 49,95€\nB. 6x Yjar 2022 a 120€", Timestamp: 1000}); err != nil {
		t.Fatal(err)
	}
	lid := "L1"
	if err := store.InsertReply(ctx, db.Reply{MsgID: "R1", ListingMsgID: &lid, ChatID: chat, AuthorID: "u1", Body: "Una cassa B per me", Timestamp: 1100}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.ImportPreventivi(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := svc.InserzioniList(ctx, "", "")
	camp := &rows[0].Campaign
	qid := camp.QuotationIDs[0]
	// simulate the legacy order without B
	if _, err := store.DB.ExecContext(ctx, "DELETE FROM quotation_items WHERE quotation_id = ? AND option = 'B'", qid); err != nil {
		t.Fatal(err)
	}
	replies, _ := svc.CampaignReplies(ctx, camp.MsgIDs)
	add := []NewOption{{Letter: "B", Wine: "Yjar 2022", Price: 120}}
	if err := svc.SetReplyOrderAddingOptions(ctx, replies[0].ID, qid, add, "6B"); err != nil {
		t.Fatal(err)
	}
	rv, _ := svc.CampaignReview(ctx, camp)
	if len(rv.Missing) != 0 || len(rv.Options) != 2 || rv.Current["u1"] != "6B" {
		t.Fatalf("after add: missing %v, options %v, current %v", rv.Missing, rv.Options, rv.Current)
	}
	if err := svc.SetReplyOrderAddingOptions(ctx, replies[0].ID, qid, add, "1B"); err == nil {
		t.Fatal("adding B twice: no error")
	}
	if rv, _ := svc.CampaignReview(ctx, camp); rv.Current["u1"] != "6B" {
		t.Fatalf("failed add changed the order: %v", rv.Current)
	}
}

// "Sposta": the reply goes under the other campaign and its correction is dropped.
func TestMoveReply(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	for _, l := range []db.Listing{
		{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: "Barolo\nA. 6x Barolo 2019 a 40€", Timestamp: 1000},
		{MsgID: "L2", ChatID: chat, AuthorID: "owner", Body: "Champagne\nA. 6x Brut a 30€", Timestamp: 2000},
	} {
		if err := store.InsertListing(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	lid := "L2"
	if err := store.InsertReply(ctx, db.Reply{MsgID: "R1", ListingMsgID: &lid, ChatID: chat, AuthorID: "u1", Body: "2A", Timestamp: 2100}); err != nil {
		t.Fatal(err)
	}
	var rid int64
	store.DB.QueryRowContext(ctx, "SELECT id FROM replies WHERE msg_id = 'R1'").Scan(&rid)
	if err := svc.SetReplyOrder(ctx, rid, "1A"); err != nil {
		t.Fatal(err)
	}
	rows, _ := svc.InserzioniList(ctx, "", "")
	var barolo *Campaign
	for i := range rows {
		if rows[i].MsgIDs[0] == "L1" {
			barolo = &rows[i].Campaign
		}
	}
	if barolo == nil {
		t.Fatal("Barolo campaign not found")
	}
	if err := svc.MoveReply(ctx, rid, barolo); err != nil {
		t.Fatal(err)
	}
	var listing, override string
	store.DB.QueryRowContext(ctx, "SELECT listing_msg_id, COALESCE(order_override,'') FROM replies WHERE id = ?", rid).Scan(&listing, &override)
	if listing != "L1" || override != "" {
		t.Fatalf("after move: listing %q, override %q", listing, override)
	}
}

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"+39 349 286 9246": "+393492869246", "349 2869246": "+393492869246", "0039 349.286.9246": "+393492869246",
		"+41 79 123 45 67": "+41791234567", "": "",
	} {
		if got, err := NormalizePhone(in); err != nil || got != want {
			t.Errorf("NormalizePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"123", "abc 349", "+39 349 286 9246 9999 99"} {
		if _, err := NormalizePhone(bad); err == nil {
			t.Errorf("NormalizePhone(%q): no error", bad)
		}
	}
}

// Phones from WhatsApp fill only empty numbers: LID map at start, the
// sender's number with a message; a number typed in Clienti stays.
func TestFillPhones(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	if _, err := store.DB.ExecContext(ctx, "CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT)"); err != nil {
		t.Fatal(err)
	}
	store.DB.ExecContext(ctx, "INSERT INTO whatsmeow_lid_map VALUES ('111', '393492869246'), ('222', '393350000000')")
	for _, u := range []string{"111@lid", "222@lid", "333@lid", "393471112222@c.us"} {
		store.UpsertUser(ctx, u, "x")
	}
	if _, err := svc.SetUserField(ctx, "222@lid", "telefono", "+39 333 999 8888"); err != nil {
		t.Fatal(err)
	}
	n, err := store.FillPhones(ctx)
	if err != nil || n != 2 {
		t.Fatalf("FillPhones = %d, %v; want 2", n, err)
	}
	if err := store.SetPhoneIfEmpty(ctx, "333@lid", "393400000001"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPhoneIfEmpty(ctx, "111@lid", "390000000000"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"111@lid": "+393492869246", "222@lid": "+393339998888", "333@lid": "+393400000001", "393471112222@c.us": "+393471112222"}
	users, _ := svc.Users(ctx)
	for _, u := range users {
		if w, ok := want[u.ID]; ok && u.Telefono != w {
			t.Errorf("%s: telefono %q, want %q", u.ID, u.Telefono, w)
		}
	}
}

// Per option: everything ordered (no matter when) against the quantity of
// the listing as first posted.
func TestOptionStock(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	if err := store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: "Barolo\nA. 3x Barolo 2019 a 40€\nB. 6x Barbaresco 2018 a 30€", Timestamp: 1000}); err != nil {
		t.Fatal(err)
	}
	lid := "L1"
	for i, r := range []struct {
		body string
		ts   int64
	}{{"5A", 900}, {"2A", 1100}, {"2A 1B", 1200}} { // the first one came before the post
		if err := store.InsertReply(ctx, db.Reply{MsgID: "R" + itoa(i), ListingMsgID: &lid, ChatID: chat, AuthorID: "u" + itoa(i), Body: r.body, Timestamp: r.ts}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := svc.ImportPreventivi(ctx); err != nil {
		t.Fatal(err)
	}
	sums, _ := svc.QuotationSummaries(ctx)
	d, err := svc.QuotationDetail(ctx, sums[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	// every reply counts, whenever it was sent: A 9 ordered of 3 offered → 6
	// too many; B 1 of 6. An edit of the listing doesn't change the offer.
	if len(d.Stock) != 2 {
		t.Fatalf("stock = %+v", d.Stock)
	}
	a, b := d.Stock[0], d.Stock[1]
	if a.Letter != "A" || a.Ordered != 9 || a.Offered != 3 || a.Excess() != 6 {
		t.Fatalf("A = %+v", a)
	}
	if b.Letter != "B" || b.Ordered != 1 || b.Remaining() != 5 || b.Excess() != 0 {
		t.Fatalf("B = %+v", b)
	}
	if err := store.UpdateListing(ctx, "L1", db.Listing{Body: "Barolo\nA. 1x Barolo 2019 a 40€\nB. 6x Barbaresco 2018 a 30€", Timestamp: 2000}); err != nil {
		t.Fatal(err)
	}
	if d, _ = svc.QuotationDetail(ctx, sums[0].Number); d.Stock[0].Offered != 3 {
		t.Fatalf("after an edit, offered = %d; want the original 3", d.Stock[0].Offered)
	}
}

// Initial quantity: the first count the seller gave for each option (the
// first post may have none), later counts ignored; a value set by hand wins.
func TestInitialStock(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	for i, body := range []string{
		"Disponibili:\nA. Champagne Theophile a 33€\nB. 12x Brut a 30€",
		"Disponibili:\nA. 174 x Champagne Theophile a 33€\nB. 12x Brut a 30€",
		"Disponibili:\nA. 71 x Champagne Theophile a 33€\nB. 3x Brut a 30€",
	} {
		if err := store.InsertListing(ctx, db.Listing{MsgID: "L" + itoa(i), ChatID: chat, AuthorID: "owner", Body: body, Timestamp: int64(1000 + i*100)}); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := svc.InserzioniList(ctx, "", "")
	if len(rows) != 1 {
		t.Fatalf("campaigns = %d, want 1", len(rows))
	}
	camp := &rows[0].Campaign
	got, err := svc.CampaignInitialStock(ctx, camp)
	if err != nil || len(got) != 2 || got[0].Qty != 174 || got[1].Qty != 12 {
		t.Fatalf("initial = %+v, %v", got, err)
	}
	if err := svc.SetInitialQty(ctx, camp, "A", "200"); err != nil {
		t.Fatal(err)
	}
	if got, _ = svc.CampaignInitialStock(ctx, camp); got[0].Qty != 200 || got[1].Qty != 12 {
		t.Fatalf("after setting A=200: %+v", got)
	}
}

// A manual wine connected to a listing's option takes that listing's
// delivery date and counts in its stock; disconnected, it is on its own again.
func TestManualLink(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	if err := store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: "Langoa\nA. 36x Langoa Barton 2022 a 47,50€", Timestamp: 1000}); err != nil {
		t.Fatal(err)
	}
	lid := "L1"
	store.InsertReply(ctx, db.Reply{MsgID: "R1", ListingMsgID: &lid, ChatID: chat, AuthorID: "u1", Body: "4A", Timestamp: 1100})
	if _, _, _, err := svc.ImportPreventivi(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := svc.InserzioniList(ctx, "", "")
	camp := &rows[0].Campaign
	if err := svc.SetCampaignDelivery(ctx, camp.MsgIDs, "2026-11-20"); err != nil {
		t.Fatal(err)
	}
	listQ := camp.QuotationIDs[0]
	num, err := svc.CreateQuotation(ctx, "2026-10-01", "Mario", []NewQuotationRow{{Option: "A", WineName: "Langoa Barton 2022", Quantity: 6, Price: 47.5}})
	if err != nil {
		t.Fatal(err)
	}
	mq, _ := svc.QuotationByNumber(ctx, num)
	if err := svc.SetItemLink(ctx, mq.ID, "A", listQ, "A"); err != nil {
		t.Fatal(err)
	}
	_, del, _ := svc.manualConsegne(ctx, mq.ID)
	if !del["A"].Linked || del["A"].Date != "2026-11-20" {
		t.Fatalf("linked delivery = %+v", del["A"])
	}
	stock, _ := svc.optionStock(ctx, listQ, nil)
	if len(stock) != 1 || stock[0].Ordered != 6 || stock[0].Remaining() != 30 {
		t.Fatalf("stock with the manual wine = %+v", stock)
	}
	if err := svc.SetItemLink(ctx, mq.ID, "A", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, del, _ = svc.manualConsegne(ctx, mq.ID); del["A"].Linked {
		t.Fatalf("still linked: %+v", del["A"])
	}
}

// Cleanup: a message stored twice keeps one copy; reposts within 15 days are
// merged into the first post (replies follow it); the same wine sold again
// months later stays a separate inserzione; numbers are by year, INSyy0001….
func TestCleanupListings(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	day := int64(86400)
	ins := func(id, body string, ts int64) {
		if err := store.InsertListing(ctx, db.Listing{MsgID: id, ChatID: chat, AuthorID: "owner", Body: body, Timestamp: ts}); err != nil {
			t.Fatal(err)
		}
	}
	ins("true_"+chat+"_S1_a@c.us", "Albert Grivault Disponibili:\nA. 30 x Meursault 2023 a 67,50€", 1000)
	ins("false_"+chat+"_S1_b@lid", "Albert Grivault Disponibili:\nA. 30 x Meursault 2023 a 67,50€", 1001)      // same message
	ins("false_"+chat+"_S2_b@lid", "Albert Grivault Disponibili:\nA. 18 x Meursault 2023 a 67,50€", 2000)      // repost
	ins("false_"+chat+"_S3_b@lid", "Albert Grivault Disponibili:\nA. 24 x Meursault 2025 a 70€", 1000+200*day) // sold again
	lid := "false_" + chat + "_S2_b@lid"
	store.InsertReply(ctx, db.Reply{MsgID: "R1", ListingMsgID: &lid, ChatID: chat, AuthorID: "u1", Body: "2A", Timestamp: 2100})

	rep, err := svc.CleanupListings(ctx, false)
	if err != nil || rep.Duplicates != 1 || rep.MergedGroups != 1 || rep.MergedPosts != 1 {
		t.Fatalf("dry run = %+v, %v", rep, err)
	}
	if n := countRows(t, store, "SELECT COUNT(*) FROM listings"); n != 4 {
		t.Fatalf("dry run wrote: %d listings", n)
	}
	if _, err := svc.CleanupListings(ctx, true); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, store, "SELECT COUNT(*) FROM listings"); n != 2 {
		t.Fatalf("listings after cleanup = %d, want 2", n)
	}
	first := db.Code(db.ListingPrefix, time.Unix(1000, 0).Year(), 1)
	if n := countRows(t, store, `SELECT COUNT(*) FROM listings WHERE listing_number = '`+first+`' AND body LIKE '%30 x%' AND last_body LIKE '%18 x%' AND initial_qty LIKE '%"A":30%'`); n != 1 {
		t.Fatal("first post not kept with its original text, latest edit and initial quantity")
	}
	if n := countRows(t, store, `SELECT COUNT(*) FROM replies r JOIN listings l ON l.msg_id = r.listing_msg_id WHERE l.listing_number = '`+first+`'`); n != 1 {
		t.Fatal("the reply did not follow the merged post")
	}
}

func countRows(t *testing.T, st *db.Store, q string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Customers: an imported name merged into the WhatsApp id counts as one
// customer in the orders; the main record takes the missing data; undo works.
func TestMergeCustomers(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: "Barolo\nA. 12x Barolo 2019 a 40€\nB. 6x Barbaresco 2018 a 30€", Timestamp: 1000})
	lid := "L1"
	store.UpsertUser(ctx, "import:Edoardo Vino Gravina", "Edoardo Vino Gravina")
	store.UpsertUser(ctx, "111@lid", "Edoardo Gravina")
	store.DB.ExecContext(ctx, `UPDATE users SET "città" = 'Arzignano' WHERE id = 'import:Edoardo Vino Gravina'`)
	store.InsertReply(ctx, db.Reply{MsgID: "R1", ListingMsgID: &lid, ChatID: chat, AuthorID: "import:Edoardo Vino Gravina", Body: "2A", Timestamp: 1100})
	store.InsertReply(ctx, db.Reply{MsgID: "R2", ListingMsgID: &lid, ChatID: chat, AuthorID: "111@lid", Body: "1B", Timestamp: 1200})
	if _, _, _, err := svc.ImportPreventivi(ctx); err != nil {
		t.Fatal(err)
	}
	sugg, _ := svc.MergeSuggestions(ctx)
	if len(sugg) == 0 || sugg[0].Main.ID != "111@lid" || sugg[0].Alias.ID != "import:Edoardo Vino Gravina" {
		t.Fatalf("suggestions = %+v", sugg)
	}
	if err := svc.MergeCustomers(ctx, "import:Edoardo Vino Gravina", "111@lid"); err != nil {
		t.Fatal(err)
	}
	sums, _ := svc.QuotationSummaries(ctx)
	if sums[0].Clienti != 1 || sums[0].Bottiglie != 3 {
		t.Fatalf("after merge: %d clienti, %d bott.; want 1, 3", sums[0].Clienti, sums[0].Bottiglie)
	}
	users, _ := svc.Users(ctx)
	if len(users) != 1 || users[0].Citta != "Arzignano" || len(users[0].Aliases) != 1 || users[0].Replies != 2 {
		t.Fatalf("customers after merge = %+v", users)
	}
	if err := svc.UnmergeCustomer(ctx, "import:Edoardo Vino Gravina"); err != nil {
		t.Fatal(err)
	}
	if sums, _ = svc.QuotationSummaries(ctx); sums[0].Clienti != 2 {
		t.Fatalf("after undo: %d clienti; want 2", sums[0].Clienti)
	}
}

// "Nuovo cliente" from an order: created with its data and a code; a name
// that's already a customer's is refused (orders pick customers by name).
func TestCreateCustomer(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	u, err := svc.CreateCustomer(ctx, User{Name: " Enoteca Rossi ", Telefono: "349 286 9246", Citta: "Vicenza"})
	if err != nil || u.Name != "Enoteca Rossi" || u.Telefono != "+393492869246" {
		t.Fatalf("created = %+v, %v", u, err)
	}
	users, _ := svc.Users(ctx)
	if len(users) != 1 || users[0].Code == "" || users[0].Citta != "Vicenza" || !IsManualUser(users[0].ID) {
		t.Fatalf("customers = %+v", users)
	}
	if _, err := svc.CreateCustomer(ctx, User{Name: "enoteca rossi"}); err == nil {
		t.Fatal("same name accepted")
	}
	if _, err := svc.CreateCustomer(ctx, User{Name: "  "}); err == nil {
		t.Fatal("empty name accepted")
	}
}

// The same wine sold again months later is a separate inserzione, with its
// own order; reposts within days stay one.
func TestCampaignChains(t *testing.T) {
	q1, q2 := int64(1), int64(2)
	rows := []ListingRow{
		{MsgID: "a", ChatID: "c", Campaign: "Roederer", Data: "2025-05-31 11:00:00", Number: "INS0497", QuotationID: &q1},
		{MsgID: "b", ChatID: "c", Campaign: "Roederer", Data: "2025-06-02 11:00:00", Number: "INS0498"},
		{MsgID: "c", ChatID: "c", Campaign: "Roederer", Data: "2026-09-23 09:00:00", Number: "INS0725", QuotationID: &q2},
	}
	camps := GroupCampaigns(rows)
	if len(camps) != 2 {
		t.Fatalf("campaigns = %d, want 2", len(camps))
	}
	for _, c := range camps {
		switch c.Number {
		case "INS0497":
			if len(c.MsgIDs) != 2 || c.Key != "Roederer" {
				t.Errorf("2025 sale = %+v", c)
			}
		case "INS0725":
			if len(c.MsgIDs) != 1 || c.Key != "Roederer · INS0725" || c.DisplayTitle == "" || len(c.QuotationIDs) != 1 || c.QuotationIDs[0] != 2 {
				t.Errorf("2026 sale = %+v", c)
			}
		default:
			t.Errorf("unexpected campaign %+v", c)
		}
	}
}

// Moving an order: its customers become the new inserzione's repliers; an
// inserzione with another order is refused.
func TestMoveQuotation(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	const chat = "1@g.us"
	store.InsertListing(ctx, db.Listing{MsgID: "L1", ChatID: chat, AuthorID: "owner", Body: "Barolo\nA. 6x Barolo 2019 a 40€", Timestamp: 1000})
	store.InsertListing(ctx, db.Listing{MsgID: "L2", ChatID: chat, AuthorID: "owner", Body: "Champagne\nA. 6x Brut a 30€", Timestamp: 2000})
	l2 := "L2"
	store.InsertReply(ctx, db.Reply{MsgID: "R1", ListingMsgID: &l2, ChatID: chat, AuthorID: "u1", Body: "2A", Timestamp: 2100})
	if _, _, _, err := svc.ImportPreventivi(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := svc.InserzioniList(ctx, "", "")
	var barolo, champ *Campaign
	for i := range rows {
		if rows[i].MsgIDs[0] == "L1" {
			barolo = &rows[i].Campaign
		} else {
			champ = &rows[i].Campaign
		}
	}
	if err := svc.MoveQuotation(ctx, barolo.QuotationIDs[0], champ); err == nil {
		t.Fatal("moved onto an inserzione that has its own order")
	}
	// free the Champagne inserzione, then move the Barolo order onto it
	store.DB.ExecContext(ctx, "UPDATE listings SET quotation_id = NULL WHERE msg_id = 'L2'")
	champ.QuotationIDs = nil
	q := barolo.QuotationIDs[0]
	if err := svc.MoveQuotation(ctx, q, champ); err != nil {
		t.Fatal(err)
	}
	var on string
	store.DB.QueryRow("SELECT GROUP_CONCAT(msg_id) FROM listings WHERE quotation_id = ?", q).Scan(&on)
	if on != "L2" {
		t.Fatalf("order now on %q, want L2", on)
	}
}

// "Nuovo prodotto" from an order: created with the next ITM code; a name
// that's already a product's is refused.
func TestCreateProduct(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	it, err := svc.CreateProduct(ctx, Item{Description: " Nellino  2023 ", Winery: "Cantina X", Area: "cha"})
	if err != nil || it.Description != "Nellino 2023" || it.Code == "" {
		t.Fatalf("created = %+v, %v", it, err)
	}
	cat, _ := svc.Catalog(ctx)
	if len(cat) != 1 || cat[0].Label() != "Nellino 2023 — Cantina X" {
		t.Fatalf("catalog = %+v", cat)
	}
	if _, err := svc.CreateProduct(ctx, Item{Description: "nellino 2023"}); err == nil {
		t.Fatal("same name accepted")
	}
}
