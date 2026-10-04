package service

import (
	"context"
	"path/filepath"
	"testing"

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
