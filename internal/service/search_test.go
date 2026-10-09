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

func TestSearchUsesConfirmedOrders(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)

	num, err := svc.CreateQuotation(ctx, "2026-10-05", "Mazzucato", []NewQuotationRow{
		{Option: "A", WineName: "Langoa Barton 2022", Quantity: 6, Price: 45},
	})
	if err != nil {
		t.Fatal(err)
	}
	// confirmed with a wine added on the order's page and a new quantity
	if err := svc.SaveOrder(ctx, num, "Mazzucato", []OrderRow{
		{Vino: "Champagne Julien Prelat Presle", Qta: 12, Prezzo: 39.95},
		{Vino: "Langoa Barton 2022", Qta: 12, Prezzo: 45},
	}); err != nil {
		t.Fatal(err)
	}
	sel, err := svc.SearchableSelections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := SearchSelections(sel, "presle"); len(got) != 1 || got[0].Preventivo != num || got[0].Utente != "Mazzucato" || got[0].Qta != 12 {
		t.Errorf("presle: %+v; want the confirmed line of %s", got, num)
	}
	if got := SearchSelections(sel, "mazzucato"); len(got) != 2 || got[0].DataPrev != "2026-10-05" {
		t.Errorf("mazzucato: %+v; want the 2 confirmed lines, not the draft ones", got)
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

func TestSearchSelectionsExactCustomerWordOrder(t *testing.T) {
	sel := []Selection{
		{Preventivo: "P1", DataPrev: "2026-09-23", Utente: "Laura De Toni", Vino: "Roederer", Qta: 1, Prezzo: 250},
		{Preventivo: "P2", DataPrev: "2026-10-06", Utente: "De Toni Laura", Vino: "Chablis", Qta: 6, Prezzo: 10},
		{Preventivo: "P3", DataPrev: "2026-10-01", Utente: "Laura", Vino: "Toni De Rosso", Qta: 1, Prezzo: 9},
	}
	got := SearchSelections(sel, "laura de toni")
	if len(got) != 2 || got[0].Preventivo != "P2" || got[1].Preventivo != "P1" {
		t.Fatalf("word order: got %+v", got)
	}
	if name, _, all := CustomerYearTotals(got); name == "" || all.Ordini != 2 || all.Totale != 310 {
		t.Fatalf("totals: %q %+v", name, all)
	}
}

func TestSearchSelectionsMatching(t *testing.T) {
	sel := []Selection{
		{Preventivo: "ORD260121", DataPrev: "2026-10-05", Utente: "Mazzucato Tommaso", Vino: "Champagne Julien Prelat - Presle", Qta: 12, Citta: "Treviso", Provincia: "Treviso"},
		{Preventivo: "ORD260114", DataPrev: "2026-10-02", Utente: "A.P.", Vino: "Château Langoa Barton 2022", Qta: 4, Citta: "Arzignano", Provincia: "Vicenza"},
		{Preventivo: "ORD260127", DataPrev: "2026-10-07", Utente: "Matteo", Vino: "L’Imprudence", Qta: 1},
		{Preventivo: "ORD260090", DataPrev: "2026-09-01", Utente: "Moët Fan", Vino: "Côte de Beaune", Qta: 2},
	}
	for q, want := range map[string]string{
		"chateau langoa":   "ORD260114",
		"CHÂTEAU":          "ORD260114",
		"presle prelat":    "ORD260121",
		"  presle  ":       "ORD260121",
		"l'imprudence":     "ORD260127",
		"ord260121":        "ORD260121",
		"260127":           "ORD260127",
		"treviso":          "ORD260121",
		"vicenza":          "ORD260114",
		"arzignano barton": "ORD260114",
		"cote beaune":      "ORD260090",
		"moet":             "ORD260090",
	} {
		got := SearchSelections(sel, q)
		if len(got) != 1 || got[0].Preventivo != want {
			t.Errorf("SearchSelections(%q) = %d rows %+v; want %s", q, len(got), got, want)
		}
	}
	if got := SearchSelections(sel, "presle vicenza"); len(got) != 0 {
		t.Errorf("every word must match: got %+v", got)
	}
}

func TestDeleteManualOrder(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := New(store, &config.Config{}, nil)
	num, err := svc.CreateQuotation(ctx, "2026-10-05", "De Toni Laura", []NewQuotationRow{
		{Option: "A", WineName: "Presle", Quantity: 6, Price: 37.95},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveOrder(ctx, num, "De Toni Laura", []OrderRow{{Vino: "Presle", Qta: 6, Prezzo: 37.95}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteManualOrder(ctx, num); err != nil {
		t.Fatal(err)
	}
	for table, where := range map[string]string{
		"quotations": "quotation_number = '" + num + "'", "quotation_items": "1=1",
		"orders": "1=1", "order_items": "1=1",
	} {
		var n int
		if err := store.DB.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE " + where).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows left (%v)", table, n, err)
		}
	}
	if err := svc.DeleteManualOrder(ctx, num); err == nil {
		t.Error("deleting it again: want an error")
	}
}
