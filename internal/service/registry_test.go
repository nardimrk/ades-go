package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"adesgo/internal/config"
	"adesgo/internal/db"
)

func TestSetItemFieldERPCodeLength(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)

	it, err := svc.CreateItem(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetItemField(ctx, it.ID, "alias", " ABCDEFGHIJKL "); err != nil {
		t.Fatalf("12 characters: %v", err)
	}
	if err := svc.SetItemField(ctx, it.ID, "alias", "ÀBCDEFGHIJKL"); err != nil {
		t.Fatalf("12 characters with an accent: %v", err)
	}
	if err := svc.SetItemField(ctx, it.ID, "alias", "ABCDEFGHIJKLM"); err == nil {
		t.Fatal("13 characters accepted")
	}
	if err := svc.SetItemField(ctx, it.ID, "alias", ""); err != nil {
		t.Fatalf("empty: %v", err)
	}
}

func TestNormalizePartitaIVA(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"01234567897":       "01234567897",
		" IT 123 456 78903": "12345678903",
		"it12345678903":     "12345678903",
		"DE 123456789":      "DE123456789",
	} {
		if got, err := NormalizePartitaIVA(in); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"01234567890", "1234567890", "123456789012", "D1234", "DE12/34"} {
		if got, err := NormalizePartitaIVA(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestNormalizePlaces(t *testing.T) {
	for in, want := range map[string]string{"": "", "vi": "Vicenza", " VICENZA ": "Vicenza", "forli cesena": "Forlì-Cesena", "l'aquila": "L'Aquila", "MB": "Monza e della Brianza"} {
		if got, err := NormalizeProvincia(in); err != nil || got != want {
			t.Errorf("provincia %q = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{"veneto": "Veneto", "Friuli Venezia Giulia": "Friuli-Venezia Giulia", "trentino alto adige": "Trentino-Alto Adige"} {
		if got, err := NormalizeRegione(in); err != nil || got != want {
			t.Errorf("regione %q = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeProvincia("Vicenz"); err == nil {
		t.Error("unknown provincia accepted")
	}
	if _, err := NormalizeRegione("Vicenza"); err == nil {
		t.Error("a provincia accepted as regione")
	}
	if len(Province()) != 107 || len(Regioni()) != 20 {
		t.Errorf("%d province, %d regioni", len(Province()), len(Regioni()))
	}
}

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{"": "", " Mario.Rossi@Example.IT ": "mario.rossi@example.it", "a+b@x.co": "a+b@x.co"} {
		if got, err := NormalizeEmail(in); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"mario", "mario@", "@x.it", "mario@x", "Mario <m@x.it>", "m@x.it.", "a b@x.it"} {
		if got, err := NormalizeEmail(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestActiveUsersSkipSellers(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	now := time.Now().Unix()
	for _, u := range [][2]string{{"a@lid", "Alessandro"}, {"b@lid", "Bruno"}, {"import:Alessandro", "Alessandro"}} {
		if _, err := store.DB.Exec("INSERT INTO users (id, name) VALUES (?, ?)", u[0], u[1]); err != nil {
			t.Fatal(err)
		}
	}
	for i, a := range []string{"a@lid", "a@lid", "b@lid"} {
		if err := store.InsertReply(ctx, db.Reply{MsgID: "R" + itoa(i), ChatID: "1@g.us", AuthorID: a, Body: "x", Timestamp: now - 3600}); err != nil {
			t.Fatal(err)
		}
	}
	// an old message doesn't count in 3 months
	if err := store.InsertReply(ctx, db.Reply{MsgID: "OLD", ChatID: "1@g.us", AuthorID: "b@lid", Body: "x", Timestamp: now - 200*24*3600}); err != nil {
		t.Fatal(err)
	}
	users, err := svc.ActiveUsers(ctx, 3)
	if err != nil || len(users) != 2 || users[0].Utente != "Alessandro" || users[1].Risposte != 1 {
		t.Fatalf("before: %+v, %v", users, err)
	}
	for _, id := range []string{"a@lid", "import:Alessandro"} {
		if err := svc.SetSeller(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	if users, _ = svc.ActiveUsers(ctx, 3); len(users) != 1 || users[0].Utente != "Bruno" {
		t.Fatalf("after: %+v", users)
	}
	sellers, err := svc.Sellers(ctx)
	if err != nil || len(sellers) != 1 || len(sellers[0].IDs) != 2 {
		t.Fatalf("sellers: %+v, %v", sellers, err)
	}
	if err := svc.SetSeller(ctx, "a@lid", false); err != nil {
		t.Fatal(err)
	}
	if users, _ = svc.ActiveUsers(ctx, 3); len(users) != 2 {
		t.Fatalf("restored: %+v", users)
	}
}
