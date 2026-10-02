// Package db is the SQLite layer. The schema is the same one used by the
// Python version (ades/db/wine.db), so an existing database can be reused
// as-is: missing tables/columns are added on startup, nothing is dropped.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

type Store struct {
	DB *sql.DB
}

// DSN builds a modernc.org/sqlite connection string with the pragmas we need:
// WAL so the web UI can read while the WhatsApp worker writes, a busy timeout
// instead of immediate SQLITE_BUSY errors, and IMMEDIATE transactions so two
// writers never deadlock upgrading a read lock.
func DSN(path string, foreignKeys bool) string {
	fk := 0
	if foreignKeys {
		fk = 1
	}
	return fmt.Sprintf("file:%s?_pragma=foreign_keys(%d)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate", path, fk)
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// Foreign keys stay off for the app tables, exactly like the Python app
	// (sqlite3 never enabled them): legacy rows may not satisfy them.
	conn, err := sql.Open("sqlite", DSN(path, false))
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(4)
	s := &Store{DB: conn}
	if err := s.migrate(context.Background()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS users (
    id         TEXT PRIMARY KEY,
    name       TEXT,
    updated_at TEXT DEFAULT (datetime('now')),
    indirizzo  TEXT, città TEXT, provincia TEXT, regione TEXT, cap TEXT
);
CREATE TABLE IF NOT EXISTS quotations (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    quotation_number   TEXT UNIQUE,
    quotation_date     TEXT,
    chat_id            TEXT,
    msg_id             TEXT,
    created_at         TEXT DEFAULT (datetime('now')),
    consegna_stimata   TEXT,
    manual_client_id   TEXT,
    manual_client_name TEXT
);
CREATE TABLE IF NOT EXISTS quotation_items (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    quotation_id INTEGER NOT NULL REFERENCES quotations(id),
    option       TEXT    NOT NULL DEFAULT '',
    wine_name    TEXT    NOT NULL,
    quantity     INTEGER NOT NULL,
    price        REAL    NOT NULL
);
CREATE TABLE IF NOT EXISTS listings (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    msg_id       TEXT UNIQUE,
    chat_id      TEXT,
    chat_name    TEXT,
    author_id    TEXT,
    author_name  TEXT,
    body         TEXT,
    wine_name    TEXT,
    price        REAL,
    vintage      INTEGER,
    media_url    TEXT,
    timestamp    INTEGER,
    created_at   TEXT,
    quotation_id INTEGER REFERENCES quotations(id),
    message      TEXT,
    title        TEXT
);
CREATE TABLE IF NOT EXISTS replies (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    msg_id         TEXT UNIQUE,
    listing_msg_id TEXT,
    chat_id        TEXT,
    author_id      TEXT REFERENCES users(id),
    body           TEXT,
    timestamp      INTEGER,
    created_at     TEXT,
    FOREIGN KEY (listing_msg_id) REFERENCES listings(msg_id)
);
CREATE INDEX IF NOT EXISTS idx_replies_listing ON replies(listing_msg_id);
CREATE INDEX IF NOT EXISTS idx_replies_author  ON replies(author_id);
CREATE TABLE IF NOT EXISTS auth_tokens (
    token      TEXT PRIMARY KEY,
    email      TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS orders (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    quotation_number TEXT,
    order_date       TEXT DEFAULT (date('now')),
    created_at       TEXT DEFAULT (datetime('now')),
    user_name        TEXT,
    user_id          TEXT
);
CREATE TABLE IF NOT EXISTS order_items (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id  INTEGER NOT NULL REFERENCES orders(id),
    user_name TEXT,
    option    TEXT,
    wine_name TEXT,
    quantity  INTEGER NOT NULL,
    price     REAL    NOT NULL,
    total     REAL    NOT NULL
);
CREATE TABLE IF NOT EXISTS items (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    itemCode      TEXT,
    description   TEXT,
    winery        TEXT,
    area          TEXT,
    created_at    TEXT DEFAULT (datetime('now')),
    updated_at    TEXT,
    deleted_at    TEXT,
    itemCodeAlias TEXT
);
CREATE TABLE IF NOT EXISTS app_meta (
    key   TEXT PRIMARY KEY,
    value TEXT
);
`

// Columns added over time by the Python app's ad-hoc ALTER TABLEs. Re-applied
// here (ignoring "duplicate column") so an older database is brought up to date.
var addColumns = []string{
	"ALTER TABLE quotation_items ADD COLUMN option TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE orders ADD COLUMN user_name TEXT",
	"ALTER TABLE orders ADD COLUMN user_id TEXT",
	"ALTER TABLE listings ADD COLUMN quotation_id INTEGER REFERENCES quotations(id)",
	"ALTER TABLE listings ADD COLUMN message TEXT",
	"ALTER TABLE listings ADD COLUMN title TEXT",
	"ALTER TABLE quotations ADD COLUMN consegna_stimata TEXT",
	"ALTER TABLE quotations ADD COLUMN manual_client_id TEXT",
	"ALTER TABLE quotations ADD COLUMN manual_client_name TEXT",
	"ALTER TABLE items ADD COLUMN itemCodeAlias TEXT",
	"ALTER TABLE users ADD COLUMN indirizzo TEXT",
	"ALTER TABLE users ADD COLUMN città TEXT",
	"ALTER TABLE users ADD COLUMN provincia TEXT",
	"ALTER TABLE users ADD COLUMN regione TEXT",
	"ALTER TABLE users ADD COLUMN cap TEXT",
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, schema); err != nil {
		return err
	}
	for _, stmt := range addColumns {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	// Link listings to the quotation created from them, and clear links to
	// quotations that were deleted.
	if _, err := s.DB.ExecContext(ctx, `
		UPDATE listings SET quotation_id = (
			SELECT q.id FROM quotations q WHERE q.msg_id = listings.msg_id
		) WHERE quotation_id IS NULL
		  AND msg_id IN (SELECT msg_id FROM quotations WHERE msg_id IS NOT NULL)`); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `
		UPDATE listings SET quotation_id = NULL
		WHERE quotation_id IS NOT NULL AND quotation_id NOT IN (SELECT id FROM quotations)`); err != nil {
		return err
	}
	if err := s.ensureDedupIndexes(ctx); err != nil {
		return err
	}
	return s.ensureSelectionsTriggers(ctx)
}

// selectionsVersionKey is the app_meta counter bumped by triggers on every
// write that can change the parsed order selections (see SelectionsVersion).
const selectionsVersionKey = "selections_version"

// Triggers are created with IF NOT EXISTS: to change one, rename it (and DROP
// the old name here) so existing databases pick up the new definition.
var selectionsTriggers = func() []string {
	bump := "BEGIN UPDATE app_meta SET value = CAST(value AS INTEGER) + 1 WHERE key = '" + selectionsVersionKey + "'; END"
	defs := []string{
		"trg_sel_replies_ins AFTER INSERT ON replies",
		"trg_sel_replies_upd AFTER UPDATE ON replies",
		"trg_sel_replies_del AFTER DELETE ON replies",
		"trg_sel_listings_ins AFTER INSERT ON listings WHEN NEW.quotation_id IS NOT NULL",
		"trg_sel_listings_upd AFTER UPDATE OF msg_id, quotation_id ON listings",
		"trg_sel_listings_del AFTER DELETE ON listings WHEN OLD.quotation_id IS NOT NULL",
		"trg_sel_users_ins AFTER INSERT ON users",
		"trg_sel_users_upd AFTER UPDATE OF id, name ON users",
		"trg_sel_users_del AFTER DELETE ON users",
		"trg_sel_quotations_ins AFTER INSERT ON quotations",
		"trg_sel_quotations_upd AFTER UPDATE OF id, quotation_number, quotation_date ON quotations",
		"trg_sel_quotations_del AFTER DELETE ON quotations",
		"trg_sel_qitems_ins AFTER INSERT ON quotation_items",
		"trg_sel_qitems_upd AFTER UPDATE ON quotation_items",
		"trg_sel_qitems_del AFTER DELETE ON quotation_items",
	}
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = "CREATE TRIGGER IF NOT EXISTS " + d + " " + bump
	}
	return out
}()

// ensureSelectionsTriggers keeps the selections counter in step with the
// data, whoever writes it: the collector, the dashboard, a chat import or a
// manual fix with the sqlite3 shell.
func (s *Store) ensureSelectionsTriggers(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, "INSERT OR IGNORE INTO app_meta (key, value) VALUES (?, '0')", selectionsVersionKey); err != nil {
		return err
	}
	for _, stmt := range selectionsTriggers {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

// SelectionsVersion changes whenever replies, linked listings, users,
// quotations or quotation items change, so callers can cache results
// derived from them.
func (s *Store) SelectionsVersion(ctx context.Context) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM app_meta WHERE key = ?", selectionsVersionKey).Scan(&v)
	return v, err
}

// ensureDedupIndexes enforces one row per (chat, author, body, timestamp) in
// listings and replies. The same WhatsApp message can reach us twice with
// different msg_ids (live event + history sync, synthetic id + real id), and
// this index is what makes the second insert a no-op — atomically.
func (s *Store) ensureDedupIndexes(ctx context.Context) error {
	create := func() error {
		if _, err := s.DB.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_listings_dedup ON listings(chat_id, author_id, body, timestamp)"); err != nil {
			return err
		}
		_, err := s.DB.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_replies_dedup ON replies(chat_id, author_id, body, timestamp)")
		return err
	}
	if create() == nil {
		return nil
	}
	log.Println("[db] collapsing duplicate listings/replies before creating dedup indexes")
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Repoint replies at the surviving listing before dropping the duplicates.
	if _, err := tx.ExecContext(ctx, `
		UPDATE replies SET listing_msg_id = (
			SELECT k.msg_id FROM listings d
			JOIN listings k ON k.id = (
				SELECT MIN(id) FROM listings x
				WHERE x.chat_id IS d.chat_id AND x.author_id IS d.author_id
				  AND x.body IS d.body AND x.timestamp IS d.timestamp)
			WHERE d.msg_id = replies.listing_msg_id)
		WHERE listing_msg_id IN (
			SELECT msg_id FROM listings WHERE id NOT IN (
				SELECT MIN(id) FROM listings GROUP BY chat_id, author_id, body, timestamp))`); err != nil {
		return err
	}
	for _, t := range []string{"listings", "replies"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			DELETE FROM %s WHERE id NOT IN (
				SELECT MIN(id) FROM %s GROUP BY chat_id, author_id, body, timestamp)`, t, t)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return create()
}

// Meta / SetMeta store small key-value settings (e.g. last backup time).
func (s *Store) Meta(ctx context.Context, key string) string {
	var v string
	_ = s.DB.QueryRowContext(ctx, "SELECT value FROM app_meta WHERE key = ?", key).Scan(&v)
	return v
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
