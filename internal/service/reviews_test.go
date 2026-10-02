package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
	if err := svc.SetUserField(ctx, "222@lid", "telefono", "+39 333 999 8888"); err != nil {
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
