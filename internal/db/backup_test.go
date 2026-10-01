package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotDropsSessionTables(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.DB.Exec("CREATE TABLE whatsmeow_device (jid TEXT); INSERT INTO whatsmeow_device VALUES ('secret')"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertUser(ctx, "1@lid", "Mario"); err != nil {
		t.Fatal(err)
	}
	data, err := st.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "snap.db")
	os.WriteFile(p, data, 0o600)
	snap, _ := sql.Open("sqlite", "file:"+p)
	defer snap.Close()
	var n int
	snap.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'whatsmeow%'").Scan(&n)
	if n != 0 {
		t.Errorf("session tables still in snapshot")
	}
	snap.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	if n != 1 {
		t.Errorf("users in snapshot = %d", n)
	}
}
