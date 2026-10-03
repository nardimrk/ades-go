package collector

import (
	"context"
	"path/filepath"
	"testing"

	"adesgo/internal/config"
	"adesgo/internal/db"
	"adesgo/internal/llm"
)

const (
	chat  = "120363000000000001@g.us"
	owner = "111@lid"
)

func setup(t *testing.T) (*Collector, *db.Store) {
	t.Helper()
	st, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{ChannelIDs: []string{chat}, OwnerID: "39333@c.us"}
	return New(cfg, st, llm.New("", "")), st
}

func count(t *testing.T, st *db.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPipeline(t *testing.T) {
	c, st := setup(t)
	ctx := context.Background()
	listing := "Domaine Test disponibili\nA. 6x Pinot Noir 2020 a 25€\nB. 3x Chardonnay 2021 a 30€"
	msgs := []*Message{
		// owner identified through the phone-number alternative id
		{ID: "false_" + chat + "_L1_" + owner, ChatID: chat, AuthorID: owner, AuthorAlt: "39333@c.us", Author: "Ale", Body: listing, Timestamp: 1000},
		{ID: "false_" + chat + "_R1_222@lid", ChatID: chat, AuthorID: "222@lid", Author: "Mario", Body: "2A", Timestamp: 1010},
		// owner repost with only quantities changed → update, not a new listing
		{ID: "false_" + chat + "_L2_" + owner, ChatID: chat, AuthorID: owner, AuthorAlt: "39333@c.us", Author: "Ale",
			Body: "Domaine Test disponibili\nA. 4x Pinot Noir 2020 a 25€\nB. 3x Chardonnay 2021 a 30€", Timestamp: 2000},
		// a different chat is ignored
		{ID: "x", ChatID: "999@g.us", AuthorID: "222@lid", Body: "1A", Timestamp: 2001},
		// reply quoting the listing by raw stanza id
		{ID: "false_" + chat + "_R2_333@lid", ChatID: chat, AuthorID: "333@lid", Author: "Giulia", Body: "1B", Timestamp: 2010, QuotedID: "L1"},
		// same message delivered again (history sync) → no duplicate
		{ID: "false_" + chat + "_R1_222@lid", ChatID: chat, AuthorID: "222@lid", Author: "Mario", Body: "2A", Timestamp: 1010},
		// owner chit-chat without price → reply
		{ID: "false_" + chat + "_O1_" + owner, ChatID: chat, AuthorID: owner, AuthorAlt: "39333@c.us", Author: "Ale", Body: "Grazie a tutti!", Timestamp: 2020},
	}
	for _, m := range msgs {
		if err := c.Handle(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, st, "SELECT COUNT(*) FROM listings"); n != 1 {
		t.Errorf("listings = %d, want 1", n)
	}
	// the seller's edit is recorded, but the listing keeps its original text
	// and initial quantities
	if n := count(t, st, "SELECT COUNT(*) FROM listings WHERE body LIKE '%6x Pinot%' AND last_body LIKE '%4x Pinot%' AND timestamp = 2000"); n != 1 {
		t.Errorf("edit not recorded, or the original text was overwritten")
	}
	if n := count(t, st, `SELECT COUNT(*) FROM listings WHERE initial_qty LIKE '%"A":6%'`); n != 1 {
		t.Errorf("initial quantity of A not saved as 6")
	}
	if n := count(t, st, "SELECT COUNT(*) FROM replies"); n != 3 {
		t.Errorf("replies = %d, want 3", n)
	}
	if n := count(t, st, "SELECT COUNT(*) FROM replies WHERE listing_msg_id = ?", "false_"+chat+"_L1_"+owner); n != 3 {
		t.Errorf("replies linked to the listing = %d, want 3", n)
	}
	if n := count(t, st, "SELECT COUNT(*) FROM users WHERE id IN ('222@lid','333@lid')"); n != 2 {
		t.Errorf("users = %d, want 2", n)
	}
}

func TestLegacyQuotedID(t *testing.T) {
	c, st := setup(t)
	ctx := context.Background()
	// a listing stored by the old whatsapp-web.js bridge
	legacy := "false_" + chat + "_3ABC123_" + owner
	if err := st.InsertListing(ctx, db.Listing{MsgID: legacy, ChatID: chat, AuthorID: owner, Body: "Vino X a 10€", Timestamp: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertListing(ctx, db.Listing{MsgID: "newer", ChatID: chat, AuthorID: owner, Body: "Vino Y a 20€", Timestamp: 200}); err != nil {
		t.Fatal(err)
	}
	m := &Message{ID: "false_" + chat + "_R_444@lid", ChatID: chat, AuthorID: "444@lid", Body: "1A", Timestamp: 300, QuotedID: "3ABC123"}
	if err := c.Handle(ctx, m); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, "SELECT COUNT(*) FROM replies WHERE listing_msg_id = ?", legacy); n != 1 {
		t.Errorf("quoted reply not linked to the legacy listing")
	}
}
