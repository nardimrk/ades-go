package service

import (
	"context"
	"path/filepath"
	"testing"

	"adesgo/internal/config"
	"adesgo/internal/db"
)

func TestSearchFindsManualOrders(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)

	num, err := svc.CreateQuotation(ctx, "2026-10-01", "Meneghina", []NewQuotationRow{
		{Option: "A", WineName: "Barolo Riserva", Quantity: 6, Price: 40},
	})
	if err != nil {
		t.Fatal(err)
	}
	sel, err := svc.SearchableSelections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"meneghina", "MENEGHINA", "barolo"} {
		got := SearchSelections(sel, q)
		if len(got) != 1 || got[0].Preventivo != num || got[0].Utente != "Meneghina" || got[0].Qta != 6 {
			t.Errorf("SearchSelections(%q) = %+v; want the manual order %s", q, got, num)
		}
	}
}

func TestSearchSelectionsExactCustomer(t *testing.T) {
	sel := []Selection{
		{Preventivo: "P1", DataPrev: "2026-03-01", Utente: "Ale", Vino: "Barolo", Qta: 2, Prezzo: 10},
		{Preventivo: "P2", DataPrev: "2025-05-01", Utente: "Ale", Vino: "Barbera", Qta: 1, Prezzo: 20},
		{Preventivo: "P2", DataPrev: "2025-05-01", Utente: "Ale", Vino: "Nebbiolo", Qta: 3, Prezzo: 5},
		{Preventivo: "P3", DataPrev: "2026-04-01", Utente: "Alessio", Vino: "Pale Ale", Qta: 1, Prezzo: 9},
	}
	if got := SearchSelections(sel, "ale"); len(got) != 3 {
		t.Fatalf("exact name: got %d rows, want 3", len(got))
	}
	if got := SearchSelections(sel, "ales"); len(got) != 1 {
		t.Fatalf("substring: got %d rows, want 1", len(got))
	}
	name, years, all := CustomerYearTotals(SearchSelections(sel, "Ale"))
	if name != "Ale" || len(years) != 2 || years[0].Year != "2026" || years[0].Totale != 20 ||
		years[1].Ordini != 1 || years[1].Totale != 35 || all.Ordini != 2 || all.Totale != 55 {
		t.Fatalf("totals: %q %+v %+v", name, years, all)
	}
	if name, _, _ := CustomerYearTotals(sel); name != "" {
		t.Fatalf("several customers: got %q, want none", name)
	}
}
