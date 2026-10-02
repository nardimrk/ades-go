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
