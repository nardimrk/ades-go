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
