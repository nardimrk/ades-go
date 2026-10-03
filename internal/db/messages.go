package db

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
)

// Queries used by the WhatsApp collector (port of app/database.py).

type Listing struct {
	MsgID      string
	ChatID     string
	ChatName   string
	AuthorID   string
	AuthorName string
	Body       string
	WineName   *string
	Price      *float64
	Vintage    *int
	MediaURL   *string
	Timestamp  int64
	InitialQty string // JSON {"A":36}: bottles offered per option, "" = none
}

type Reply struct {
	MsgID        string
	ListingMsgID *string
	ChatID       string
	AuthorID     string
	Body         string
	Timestamp    int64
}

// Candidate is a previous message used for similarity / order resolution.
type Candidate struct {
	MsgID        string
	Body         string
	Timestamp    int64
	ListingMsgID string
}

type Customer struct {
	ID   string
	Name string
}

func (s *Store) UpsertUser(ctx context.Context, id, name string) error {
	if id == "" {
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO users (id, name, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			name       = CASE WHEN excluded.name != '' THEN excluded.name ELSE users.name END,
			updated_at = datetime('now')`, id, name)
	return err
}

// SetPhoneIfEmpty stores the phone number of a customer who has none yet
// (a number typed in Clienti is never replaced). phone is digits with or
// without "+"; users that don't exist are left alone.
func (s *Store) SetPhoneIfEmpty(ctx context.Context, userID, phone string) error {
	digits := strings.TrimPrefix(strings.TrimSpace(phone), "+")
	if userID == "" || len(digits) < 8 || strings.Trim(digits, "0123456789") != "" {
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET telefono = ? WHERE id = ? AND COALESCE(TRIM(telefono),'') = ''`, "+"+digits, userID)
	return err
}

// FillPhones fills the empty phone numbers it can derive: "<n>@c.us" ids are
// the number itself, and "<n>@lid" ids are looked up in whatsmeow's LID →
// phone map. Safe to run at every start; returns how many were filled.
func (s *Store) FillPhones(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE users SET telefono = '+' || substr(id, 1, length(id) - 5)
		WHERE id LIKE '%@c.us' AND COALESCE(TRIM(telefono),'') = '' AND substr(id, 1, length(id) - 5) GLOB '[0-9]*'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	var hasMap int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'whatsmeow_lid_map'").Scan(&hasMap); err != nil || hasMap == 0 {
		return n, err // no WhatsApp session in this database (e.g. a test copy)
	}
	res, err = s.DB.ExecContext(ctx, `UPDATE users SET telefono = '+' || (
			SELECT m.pn FROM whatsmeow_lid_map m WHERE m.lid = substr(users.id, 1, length(users.id) - 4))
		WHERE id LIKE '%@lid' AND COALESCE(TRIM(telefono),'') = ''
		  AND EXISTS (SELECT 1 FROM whatsmeow_lid_map m WHERE m.lid = substr(users.id, 1, length(users.id) - 4) AND m.pn GLOB '[0-9]*')`)
	if err != nil {
		return n, err
	}
	m, _ := res.RowsAffected()
	return n + m, nil
}

// InsertListing is a no-op when the same message (by msg_id or by content —
// see ensureDedupIndexes) is already stored.
func (s *Store) InsertListing(ctx context.Context, l Listing) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT OR IGNORE INTO listings
		(msg_id, chat_id, chat_name, author_id, author_name, body, original_body, initial_qty,
		 wine_name, price, vintage, media_url, timestamp, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, datetime(?, 'unixepoch'))`,
		l.MsgID, l.ChatID, l.ChatName, l.AuthorID, l.AuthorName, l.Body, l.Body, l.InitialQty,
		l.WineName, l.Price, l.Vintage, l.MediaURL, l.Timestamp, l.Timestamp)
	return err
}

// UpdateListing records the seller's edit of a listing ("Disponibili: 11 x
// …" during the sale). The listing keeps its original text, wine and
// quantities: the edit only goes to last_body, and the time of the edit is
// kept (it orders the campaigns). media_url keeps the old value when the
// edit has none.
func (s *Store) UpdateListing(ctx context.Context, listingMsgID string, l Listing) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE listings
		SET last_body = ?, media_url = COALESCE(?, media_url), timestamp = ?
		WHERE msg_id = ?`,
		l.Body, l.MediaURL, l.Timestamp, listingMsgID)
	return err
}

// InsertReply stores a reply. Re-processing the same message (same msg_id)
// upgrades a row whose LLM order resolution failed the first time, but never
// downgrades an already-resolved one (" → " marks a resolved body).
func (s *Store) InsertReply(ctx context.Context, r Reply) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO replies
		(msg_id, listing_msg_id, chat_id, author_id, body, timestamp, created_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime(?, 'unixepoch'))
		ON CONFLICT(msg_id) DO UPDATE SET
			listing_msg_id = excluded.listing_msg_id,
			author_id      = excluded.author_id,
			body           = excluded.body
		WHERE instr(replies.body, ' → ') = 0 AND excluded.body != replies.body
		ON CONFLICT(chat_id, author_id, body, timestamp) DO NOTHING`,
		r.MsgID, r.ListingMsgID, r.ChatID, r.AuthorID, r.Body, r.Timestamp, r.Timestamp)
	return err
}

// FindListingForReply returns the listing a reply belongs to: the quoted
// message if it is a known listing, otherwise the latest listing in the chat
// posted before the reply.
//
// quotedID is the raw WhatsApp stanza id. Listings stored by the old
// whatsapp-web.js bridge use a serialized id ("false_<chat>_<id>_<author>"),
// so the lookup matches the raw id inside it too.
func (s *Store) FindListingForReply(ctx context.Context, chatID string, ts int64, quotedID string) (string, bool) {
	var id string
	if quotedID != "" {
		err := s.DB.QueryRowContext(ctx, `
			SELECT msg_id FROM listings
			WHERE msg_id = ? OR (chat_id = ? AND (msg_id LIKE ? ESCAPE '\' OR msg_id LIKE ? ESCAPE '\'))
			LIMIT 1`,
			quotedID, chatID, "%\\_"+likeEscape(quotedID)+"\\_%", "%\\_"+likeEscape(quotedID)).Scan(&id)
		if err == nil {
			return id, true
		}
	}
	err := s.DB.QueryRowContext(ctx, `
		SELECT msg_id FROM listings
		WHERE chat_id = ? AND timestamp <= ?
		ORDER BY timestamp DESC LIMIT 1`, chatID, ts).Scan(&id)
	return id, err == nil
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

var quotationInBodyRe = regexp.MustCompile(`\b(\d{2}S\d{4})\b`)

// BodyMentionsQuotation reports whether body contains a known quotation
// number (e.g. 26S0002) that was sent to the group.
func (s *Store) BodyMentionsQuotation(ctx context.Context, body string) bool {
	m := quotationInBodyRe.FindStringSubmatch(body)
	if m == nil {
		return false
	}
	var msgID sql.NullString
	err := s.DB.QueryRowContext(ctx, "SELECT msg_id FROM quotations WHERE quotation_number = ?", m[1]).Scan(&msgID)
	return err == nil && msgID.Valid && msgID.String != ""
}

// RecentOwnListings: this author's listings in this chat from the last
// maxDays up to beforeTS, newest first.
func (s *Store) RecentOwnListings(ctx context.Context, chatID, authorID string, beforeTS int64, maxDays int) ([]Candidate, error) {
	cutoff := beforeTS - int64(maxDays)*86400
	return s.candidates(ctx, `
		SELECT msg_id, COALESCE(body,''), timestamp, '' FROM listings
		WHERE chat_id = ? AND author_id = ? AND timestamp BETWEEN ? AND ?
		ORDER BY timestamp DESC`, chatID, authorID, cutoff, beforeTS)
}

// replyBodySQL: a reply's body as the order rules see it, with the codes set
// by hand (Inserzioni, "Controlla risposte") appended like the LLM's "→ 3A".
const replyBodySQL = `COALESCE(body,'') || CASE WHEN COALESCE(order_override,'') NOT IN ('','-') THEN ' → ' || order_override ELSE '' END`

// RecentOwnReplies: this author's earlier replies to one listing, newest first.
func (s *Store) RecentOwnReplies(ctx context.Context, listingMsgID, authorID string, beforeTS int64) ([]Candidate, error) {
	return s.candidates(ctx, `
		SELECT COALESCE(msg_id,''), `+replyBodySQL+`, timestamp, COALESCE(listing_msg_id,'') FROM replies
		WHERE listing_msg_id = ? AND author_id = ? AND timestamp <= ?
		ORDER BY timestamp DESC`, listingMsgID, authorID, beforeTS)
}

// ListingOwnerNotes returns a listing's body and its author's own replies to
// it (e.g. "rimangono disponibili 4 bottiglie..."), newest first.
func (s *Store) ListingOwnerNotes(ctx context.Context, listingMsgID string, beforeTS int64) (string, []Candidate, error) {
	var body, author string
	err := s.DB.QueryRowContext(ctx,
		"SELECT COALESCE(body,''), COALESCE(author_id,'') FROM listings WHERE msg_id = ?", listingMsgID).Scan(&body, &author)
	if err == sql.ErrNoRows {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	notes, err := s.RecentOwnReplies(ctx, listingMsgID, author, beforeTS)
	return body, notes, err
}

// RecentRepliesByAuthor: this author's replies anywhere in the chat, newest first.
func (s *Store) RecentRepliesByAuthor(ctx context.Context, chatID, authorID string, beforeTS int64) ([]Candidate, error) {
	return s.candidates(ctx, `
		SELECT COALESCE(msg_id,''), `+replyBodySQL+`, timestamp, COALESCE(listing_msg_id,'') FROM replies
		WHERE chat_id = ? AND author_id = ? AND timestamp <= ?
		ORDER BY timestamp DESC`, chatID, authorID, beforeTS)
}

func (s *Store) candidates(ctx context.Context, q string, args ...any) ([]Candidate, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		var ts sql.NullInt64
		if err := rows.Scan(&c.MsgID, &c.Body, &ts, &c.ListingMsgID); err != nil {
			return nil, err
		}
		c.Timestamp = ts.Int64
		out = append(out, c)
	}
	return out, rows.Err()
}

// KnownCustomers: everyone who ever replied in this chat and has a name.
func (s *Store) KnownCustomers(ctx context.Context, chatID string) ([]Customer, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT u.id, u.name FROM users u
		JOIN replies r ON r.author_id = u.id
		WHERE r.chat_id = ? AND u.name IS NOT NULL AND u.name != ''`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Customer
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UnnamedLIDUsers returns @lid users with no usable display name yet.
func (s *Store) UnnamedLIDUsers(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id FROM users
		WHERE id LIKE '%@lid' AND (name IS NULL OR name = '' OR name LIKE '%@%')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) SetUserName(ctx context.Context, id, name string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, "UPDATE users SET name = ?, updated_at = datetime('now') WHERE id = ?", name, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// KnownChatNames returns {chat_id: name} seen historically in listings, used
// when WhatsApp can't resolve a group name.
func (s *Store) KnownChatNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT chat_id, chat_name, COUNT(*) AS n FROM listings
		WHERE chat_name IS NOT NULL AND chat_name != '' AND chat_name != chat_id
		GROUP BY chat_id, chat_name ORDER BY n DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var n int
		if rows.Scan(&id, &name, &n) == nil {
			if _, ok := out[id]; !ok {
				out[id] = name
			}
		}
	}
	return out
}

// OldestMessageID returns the serialized id of the oldest stored message in a
// chat that carries a real WhatsApp id (used to request older history).
func (s *Store) OldestMessageID(ctx context.Context, chatID string) (string, int64, bool) {
	var id string
	var ts int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT msg_id, timestamp FROM (
			SELECT msg_id, timestamp FROM listings WHERE chat_id = ? AND (msg_id LIKE 'true\_%' ESCAPE '\' OR msg_id LIKE 'false\_%' ESCAPE '\')
			UNION ALL
			SELECT msg_id, timestamp FROM replies WHERE chat_id = ? AND (msg_id LIKE 'true\_%' ESCAPE '\' OR msg_id LIKE 'false\_%' ESCAPE '\')
		) ORDER BY timestamp ASC LIMIT 1`, chatID, chatID).Scan(&id, &ts)
	return id, ts, err == nil
}
