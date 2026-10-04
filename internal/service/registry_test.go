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

func TestPaginate(t *testing.T) {
	for _, c := range []struct{ total, num, wantNum, wantPages, lo, hi int }{
		{280, 1, 1, 12, 0, 25}, {280, 12, 12, 12, 275, 280}, {280, 99, 12, 12, 275, 280},
		{280, 0, 1, 12, 0, 25}, {0, 1, 1, 1, 0, 0}, {25, 2, 1, 1, 0, 25},
	} {
		p, lo, hi := Paginate(c.total, c.num, 25)
		if p.Num != c.wantNum || p.Pages != c.wantPages || lo != c.lo || hi != c.hi {
			t.Errorf("Paginate(%d, %d) = %+v, %d, %d", c.total, c.num, p, lo, hi)
		}
	}
}

func TestFilterUsersAndCache(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	for _, u := range [][3]string{{"a@lid", "Mario Rossi", "Città di Castello"}, {"b@lid", "Bruno", "Vicenza"}} {
		if _, err := store.DB.Exec("INSERT INTO users (id, name, città) VALUES (?, ?, ?)", u[0], u[1], u[2]); err != nil {
			t.Fatal(err)
		}
	}
	users, err := svc.UsersCached(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("users = %+v, %v", users, err)
	}
	if got := FilterUsers(users, "citta di", nil); len(got) != 1 || got[0].Name != "Mario Rossi" {
		t.Errorf("search = %+v", got)
	}
	if got := FilterUsers(users, "", map[string]string{"citta": "vic", "nome": "bru"}); len(got) != 1 || got[0].Name != "Bruno" {
		t.Errorf("columns = %+v", got)
	}
	// a write anywhere refreshes the cached list
	if _, err := svc.SetUserField(ctx, "b@lid", "name", "Bruno Bianchi"); err != nil {
		t.Fatal(err)
	}
	if users, _ = svc.UsersCached(ctx); FilterUsers(users, "bianchi", nil) == nil {
		t.Error("cache not refreshed after a change")
	}
}

func TestSplitRecent(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	var months []InserzioniMonth
	for _, ym := range [][2]int{{2026, 10}, {2026, 5}, {2026, 4}, {2026, 1}, {2025, 12}, {2025, 3}, {0, 0}} {
		months = append(months, InserzioniMonth{Year: ym[0], Month: ym[1], Rows: make([]CampaignRow, 2), Risposte: 3})
	}
	recent, years := SplitRecent(months, now)
	if len(recent) != 2 || recent[1].Month != 5 {
		t.Fatalf("recent = %+v", recent)
	}
	if len(years) != 3 || years[0].Year != 2026 || len(years[0].Months) != 2 || years[1].Year != 2025 || years[1].Count != 4 || years[1].Risposte != 6 || years[2].Year != 0 {
		t.Fatalf("years = %+v", years)
	}
}
