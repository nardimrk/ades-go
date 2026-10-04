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
-- LLM review of a reply (Inserzioni, "Controlla risposte"): kind is
-- 'dubbia' or 'ambigua'; status 'open', 'applied' or 'dismissed'. body is the
-- reply text the check was made on, so an edited reply is checked again.
CREATE TABLE IF NOT EXISTS reply_checks (
    reply_id   INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    proposal   TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'open',
    checked_at TEXT DEFAULT (datetime('now'))
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
	"ALTER TABLE quotation_items ADD COLUMN vintage INTEGER",
	"ALTER TABLE orders ADD COLUMN user_name TEXT",
	"ALTER TABLE orders ADD COLUMN user_id TEXT",
	"ALTER TABLE listings ADD COLUMN quotation_id INTEGER REFERENCES quotations(id)",
	"ALTER TABLE listings ADD COLUMN message TEXT",
	"ALTER TABLE listings ADD COLUMN title TEXT",
	// estimated delivery date of a campaign ("YYYY-MM-DD"), set in Inserzioni
	// and copied to the quotation linked to it
	"ALTER TABLE listings ADD COLUMN consegna_stimata TEXT",
	"ALTER TABLE quotations ADD COLUMN consegna_stimata TEXT",
	"ALTER TABLE quotations ADD COLUMN manual_client_id TEXT",
	"ALTER TABLE quotations ADD COLUMN manual_client_name TEXT",
	"ALTER TABLE items ADD COLUMN itemCodeAlias TEXT",
	"ALTER TABLE users ADD COLUMN indirizzo TEXT",
	"ALTER TABLE users ADD COLUMN città TEXT",
	"ALTER TABLE users ADD COLUMN provincia TEXT",
	"ALTER TABLE users ADD COLUMN regione TEXT",
	"ALTER TABLE users ADD COLUMN cap TEXT",
	// order codes set by hand on a reply ("3A, 1 cassa B"; "-" = not an
	// order): read instead of the body by the order parser
	"ALTER TABLE replies ADD COLUMN order_override TEXT",
	// the customer's phone ("+393492869246"): filled from WhatsApp when
	// empty (see FillPhones), editable in Clienti
	"ALTER TABLE users ADD COLUMN telefono TEXT",
	// the listing as first posted: edits overwrite body, this keeps the
	// original quantities (the availability the order page compares with)
	"ALTER TABLE listings ADD COLUMN original_body TEXT",
	// the bottles offered per option when the listing was first posted
	// ({"A":36,"B":12}), editable on the listing page; NULL = read them from
	// original_body
	"ALTER TABLE listings ADD COLUMN initial_qty TEXT",
	// the latest edit of the listing by the seller (the listing itself keeps
	// its original text)
	"ALTER TABLE listings ADD COLUMN last_body TEXT",
	// manual orders: the estimated delivery of each wine ("YYYY-MM-DD")
	"ALTER TABLE quotation_items ADD COLUMN consegna_stimata TEXT",
	// manual orders: a wine connected to an option of a listing's order (it
	// takes that listing's estimated delivery and counts in its stock)
	"ALTER TABLE quotation_items ADD COLUMN linked_quotation_id INTEGER",
	"ALTER TABLE quotation_items ADD COLUMN linked_option TEXT",
	// the inserzione's number ("INS0123"), unique, in posting order
	"ALTER TABLE listings ADD COLUMN listing_number TEXT",
	// customers: a duplicate id points to the main record (the customer),
	// sellers are not customers, every customer has a code (CLI0001)
	"ALTER TABLE users ADD COLUMN merged_into TEXT",
	"ALTER TABLE users ADD COLUMN is_seller INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE users ADD COLUMN customer_code TEXT",
	// the customer's VAT number ("01234567890", or "DE123456789" abroad),
	// editable in Clienti
	"ALTER TABLE users ADD COLUMN partita_iva TEXT",
	// the customer's country (not used yet; "" = Italia)
	"ALTER TABLE users ADD COLUMN stato TEXT",
	// the customer's email address, editable in Clienti
	"ALTER TABLE users ADD COLUMN email TEXT",
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
	if err := s.seedPlaces(ctx); err != nil {
		return fmt.Errorf("regioni/province: %w", err)
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
	// listings stored before original_body existed: their current text is
	// the best original there is (earlier edits are lost)
	if _, err := s.DB.ExecContext(ctx, "UPDATE listings SET original_body = body WHERE original_body IS NULL"); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_users_code ON users(customer_code)"); err != nil {
		return err
	}
	if err := s.AssignCustomerCodes(ctx); err != nil {
		return err
	}
	if err := s.AssignListingNumbers(ctx); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_listings_number ON listings(listing_number)"); err != nil {
		return err
	}
	if err := s.ensureDedupIndexes(ctx); err != nil {
		return err
	}
	if err := s.ensureSelectionsTriggers(ctx); err != nil {
		return err
	}
	return s.ensureListTriggers(ctx)
}

// Counters for the Clienti and Prodotti lists (see Version), bumped by
// triggers on every write that changes what those pages show.
const (
	ClientiVersionKey  = "clienti_version"
	ProdottiVersionKey = "prodotti_version"
	// the listings with their reply counts (Inserzioni and every page
	// built on the campaigns)
	ListingsVersionKey = "listings_version"
	// confirmed orders (the "Confermato il" of Ordini)
	OrdiniVersionKey = "ordini_version"
	// the selections counter (see SelectionsVersion), for list caches too
	SelectionsVersionKey = selectionsVersionKey
)

var listTriggers = map[string][]string{
	ClientiVersionKey: {
		"trg_cli_users_ins AFTER INSERT ON users",
		"trg_cli_users_upd AFTER UPDATE ON users",
		"trg_cli_users_del AFTER DELETE ON users",
		// the replies count shown per customer
		"trg_cli_replies_ins AFTER INSERT ON replies",
		"trg_cli_replies_upd AFTER UPDATE OF author_id ON replies",
		"trg_cli_replies_del AFTER DELETE ON replies",
	},
	ListingsVersionKey: {
		"trg_lst_listings_ins AFTER INSERT ON listings",
		"trg_lst_listings_upd AFTER UPDATE ON listings",
		"trg_lst_listings_del AFTER DELETE ON listings",
		"trg_lst_replies_ins AFTER INSERT ON replies",
		"trg_lst_replies_upd AFTER UPDATE OF listing_msg_id ON replies",
		"trg_lst_replies_del AFTER DELETE ON replies",
	},
	OrdiniVersionKey: {
		"trg_ord_orders_ins AFTER INSERT ON orders",
		"trg_ord_orders_upd AFTER UPDATE ON orders",
		"trg_ord_orders_del AFTER DELETE ON orders",
		"trg_ord_items_ins AFTER INSERT ON order_items",
		"trg_ord_items_upd AFTER UPDATE ON order_items",
		"trg_ord_items_del AFTER DELETE ON order_items",
		// e.g. a manual order's customer (its title in the list)
		"trg_ord_quot_upd AFTER UPDATE ON quotations",
	},
	ProdottiVersionKey: {
		"trg_prod_items_ins AFTER INSERT ON items",
		"trg_prod_items_upd AFTER UPDATE ON items",
		"trg_prod_items_del AFTER DELETE ON items",
	},
}

func (s *Store) ensureListTriggers(ctx context.Context) error {
	for key, defs := range listTriggers {
		if _, err := s.DB.ExecContext(ctx, "INSERT OR IGNORE INTO app_meta (key, value) VALUES (?, '0')", key); err != nil {
			return err
		}
		bump := "BEGIN UPDATE app_meta SET value = CAST(value AS INTEGER) + 1 WHERE key = '" + key + "'; END"
		for _, d := range defs {
			if _, err := s.DB.ExecContext(ctx, "CREATE TRIGGER IF NOT EXISTS "+d+" "+bump); err != nil {
				return fmt.Errorf("%s: %w", d, err)
			}
		}
	}
	return nil
}

// Version reads an app_meta counter (e.g. ClientiVersionKey): it changes
// whenever the data behind it does, so callers can cache what they derive.
func (s *Store) Version(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM app_meta WHERE key = ?", key).Scan(&v)
	return v, err
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
		// merging/separating customers changes whose orders the replies are
		"trg_sel_users_merge AFTER UPDATE OF merged_into ON users",
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

// AssignListingNumbers gives the listings without a number the next ones
// ("INS0001", "INS0002", …), in posting order.
func (s *Store) AssignListingNumbers(ctx context.Context) error {
	var max int
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(substr(listing_number, 4) AS INTEGER)), 0)
		FROM listings WHERE listing_number LIKE 'INS%'`).Scan(&max); err != nil {
		return err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT id FROM listings WHERE listing_number IS NULL ORDER BY created_at, id")
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		max++
		if _, err := s.DB.ExecContext(ctx, "UPDATE listings SET listing_number = ? WHERE id = ?", fmt.Sprintf("INS%04d", max), id); err != nil {
			return err
		}
	}
	return nil
}

// AssignCustomerCodes gives the customers without a code the next ones
// ("CLI0001", …), in order of their first message. Sellers and WhatsApp
// groups get none; a merged id keeps the code it had.
func (s *Store) AssignCustomerCodes(ctx context.Context) error {
	var max int
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(substr(customer_code, 4) AS INTEGER)), 0)
		FROM users WHERE customer_code LIKE 'CLI%'`).Scan(&max); err != nil {
		return err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT u.id FROM users u
		WHERE u.customer_code IS NULL AND COALESCE(u.is_seller,0) = 0 AND u.merged_into IS NULL AND u.id NOT LIKE '%@g.us'
		ORDER BY (SELECT MIN(timestamp) FROM replies r WHERE r.author_id = u.id) IS NULL,
		         (SELECT MIN(timestamp) FROM replies r WHERE r.author_id = u.id), u.id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		max++
		if _, err := s.DB.ExecContext(ctx, "UPDATE users SET customer_code = ? WHERE id = ?", fmt.Sprintf("CLI%04d", max), id); err != nil {
			return err
		}
	}
	return nil
}
