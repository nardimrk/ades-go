package db

import (
	"context"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Snapshot writes a consistent copy of the database (VACUUM INTO works while
// other connections keep writing) and returns its bytes.
func (s *Store) Snapshot(ctx context.Context) ([]byte, error) {
	dir, err := os.MkdirTemp("", "adesgo-backup-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "snapshot.db")
	if _, err := s.DB.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return nil, err
	}
	if err := stripSessionTables(ctx, path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// stripSessionTables drops the whatsmeow_* tables from a snapshot: they hold
// the linked device's keys, which must never leave the server by email.
func stripSessionTables(ctx context.Context, path string) error {
	snap, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer snap.Close()
	rows, err := snap.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'whatsmeow\\_%' ESCAPE '\\'")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			tables = append(tables, n)
		}
	}
	rows.Close()
	for _, t := range tables {
		if _, err := snap.ExecContext(ctx, `DROP TABLE "`+t+`"`); err != nil {
			return err
		}
	}
	_, err = snap.ExecContext(ctx, "VACUUM")
	return err
}

// BackupSender delivers a snapshot (implemented with Mailgun in main).
type BackupSender func(ctx context.Context, filename string, data []byte) error

const backupMetaKey = "last_backup_at"

// RunWeeklyBackup sends a snapshot whenever the last one is older than a
// week, checking hourly. The last send time is stored in app_meta so
// restarts don't trigger extra backups.
func (s *Store) RunWeeklyBackup(ctx context.Context, send BackupSender) {
	check := func() {
		last, _ := time.Parse(time.RFC3339, s.Meta(ctx, backupMetaKey))
		if time.Since(last) < 7*24*time.Hour {
			return
		}
		data, err := s.Snapshot(ctx)
		if err != nil {
			log.Printf("[backup] snapshot failed: %v", err)
			return
		}
		name := "wine_backup_" + time.Now().Format("20060102") + ".db"
		if err := send(ctx, name, data); err != nil {
			log.Printf("[backup] send failed: %v", err)
			return
		}
		_ = s.SetMeta(ctx, backupMetaKey, time.Now().Format(time.RFC3339))
		log.Printf("[backup] sent %s (%d KB)", name, len(data)/1024)
	}
	check()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
