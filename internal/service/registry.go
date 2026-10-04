package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"adesgo/internal/textutil"
)

// ── Clienti ──────────────────────────────────────────────────────────────────

type User struct {
	ID        string
	Name      string
	Indirizzo string
	Citta     string
	Provincia string
	Regione   string
	CAP       string
	Telefono  string // "+393492869246", "" = unknown
	Replies   int    // messages written in the groups (all its ids)
	Code      string // "CLI0012"
	Aliases   []string // other ids merged into this customer
}

func (s *Service) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db().QueryContext(ctx, `
		SELECT id, COALESCE(name,''), COALESCE(indirizzo,''), COALESCE(città,''),
		       COALESCE(provincia,''), COALESCE(regione,''), COALESCE(cap,''), COALESCE(telefono,''),
		       (SELECT COUNT(*) FROM replies r WHERE r.author_id = users.id OR r.author_id IN (SELECT a.id FROM users a WHERE a.merged_into = users.id)),
		       COALESCE(customer_code,''), COALESCE((SELECT group_concat(a.id, char(10)) FROM users a WHERE a.merged_into = users.id), '')
		FROM users WHERE id NOT LIKE '%@g.us' AND merged_into IS NULL AND COALESCE(is_seller,0) = 0 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var aliases string
		if err := rows.Scan(&u.ID, &u.Name, &u.Indirizzo, &u.Citta, &u.Provincia, &u.Regione, &u.CAP, &u.Telefono, &u.Replies, &u.Code, &aliases); err != nil {
			return nil, err
		}
		if aliases != "" {
			u.Aliases = strings.Split(aliases, "\n")
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ManualUserPrefix marks customers added by hand from the Clienti page (the
// others come from WhatsApp and have their WhatsApp id).
const ManualUserPrefix = "manual:"

// IsManualUser reports a customer added by hand (no WhatsApp id): from
// Clienti ("manual:…") or from a manual order ("manual-…").
func IsManualUser(id string) bool {
	return strings.HasPrefix(id, ManualUserPrefix) || strings.HasPrefix(id, "manual-")
}

// CreateManualUser adds an empty customer, to be filled in from the UI.
func (s *Service) CreateManualUser(ctx context.Context) (User, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return User{}, err
	}
	u := User{ID: ManualUserPrefix + hex.EncodeToString(b)}
	_, err := s.db().ExecContext(ctx, "INSERT INTO users (id, name, updated_at) VALUES (?, '', datetime('now'))", u.ID)
	return u, err
}

// CreateCustomer adds a customer typed in by hand (e.g. from "Nuovo
// ordine"): a name is required and must not be another customer's, since
// orders pick customers by name. The phone is normalized like in Clienti.
func (s *Service) CreateCustomer(ctx context.Context, u User) (User, error) {
	u.Name = strings.TrimSpace(textutil.StripEmoji(u.Name))
	if u.Name == "" {
		return User{}, fmt.Errorf("scrivi il nome del cliente")
	}
	byName, _, err := s.ClientOptions(ctx)
	if err != nil {
		return User{}, err
	}
	for n := range byName {
		if strings.EqualFold(strings.TrimSpace(n), u.Name) {
			return User{}, fmt.Errorf("esiste già un cliente «%s»: sceglilo dall'elenco", n)
		}
	}
	if u.Telefono, err = NormalizePhone(u.Telefono); err != nil {
		return User{}, err
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return User{}, err
	}
	u.ID = ManualUserPrefix + hex.EncodeToString(b)
	if _, err := s.db().ExecContext(ctx, `INSERT INTO users (id, name, telefono, indirizzo, "città", provincia, cap, updated_at)
		VALUES (?, ?, NULLIF(?,''), NULLIF(?,''), NULLIF(?,''), NULLIF(?,''), NULLIF(?,''), datetime('now'))`,
		u.ID, u.Name, u.Telefono, strings.TrimSpace(u.Indirizzo), strings.TrimSpace(u.Citta), strings.TrimSpace(u.Provincia), strings.TrimSpace(u.CAP)); err != nil {
		return User{}, err
	}
	return u, s.Store.AssignCustomerCodes(ctx)
}

// DeleteUser removes a customer, unless they have confirmed orders. Their
// messages stay in the inserzioni (shown with the WhatsApp id instead of the
// name), and a customer who writes again in a group is created again.
func (s *Service) DeleteUser(ctx context.Context, id string) error {
	var n int
	if err := s.db().QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM orders WHERE user_id = ?) + (SELECT COUNT(*) FROM quotations WHERE manual_client_id = ?)`, id, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("il cliente ha degli ordini confermati: non si può eliminare")
	}
	if _, err := s.db().ExecContext(ctx, "UPDATE users SET merged_into = NULL WHERE merged_into = ?", id); err != nil {
		return err
	}
	res, err := s.db().ExecContext(ctx, "DELETE FROM users WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("cliente non trovato")
	}
	return nil
}

// userColumns are the customer fields editable from the Clienti page.
var userColumns = map[string]string{
	"name": "name", "indirizzo": "indirizzo", "citta": "città",
	"provincia": "provincia", "regione": "regione", "cap": "cap", "telefono": "telefono",
}

// NormalizePhone turns a typed number into "+<country><number>": spaces,
// dots and dashes are dropped, "00" becomes "+", and a number without a
// country code is taken as Italian ("349 2869246" → "+393492869246").
func NormalizePhone(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	plus := strings.HasPrefix(v, "+")
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, v)
	switch {
	case plus:
	case strings.HasPrefix(digits, "00"):
		digits = digits[2:]
	default:
		digits = "39" + digits
	}
	if strings.Trim(v, "+0123456789 .-/()") != "" || len(digits) < 8 || len(digits) > 15 {
		return "", fmt.Errorf("numero di telefono non valido: %q", v)
	}
	return "+" + digits, nil
}

// SetUserField updates one field of one customer.
func (s *Service) SetUserField(ctx context.Context, id, field, value string) error {
	col, ok := userColumns[field]
	if !ok {
		return fmt.Errorf("campo %q non valido", field)
	}
	if field == "telefono" {
		var err error
		if value, err = NormalizePhone(value); err != nil {
			return err
		}
	}
	res, err := s.db().ExecContext(ctx,
		`UPDATE users SET "`+col+`" = ?, updated_at = datetime('now') WHERE id = ?`, strings.TrimSpace(value), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("cliente non trovato")
	}
	return nil
}

// ── Prodotti ─────────────────────────────────────────────────────────────────

type Item struct {
	ID          int64 // 0 = new row
	Code        string
	CodeAlias   string
	Description string
	Winery      string
	Area        string
	Delete      bool
}

func (s *Service) Items(ctx context.Context) ([]Item, error) {
	rows, err := s.db().QueryContext(ctx, `
		SELECT id, COALESCE(itemCode,''), COALESCE(itemCodeAlias,''), COALESCE(description,''),
		       COALESCE(winery,''), COALESCE(area,'')
		FROM items WHERE deleted_at IS NULL ORDER BY itemCode`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Code, &it.CodeAlias, &it.Description, &it.Winery, &it.Area); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// nextItemCode returns the next free ITMnnnn code.
func nextItemCode(ctx context.Context, q querier) string {
	var n sql.NullInt64
	q.QueryRowContext(ctx, `SELECT MAX(CAST(SUBSTR(itemCode, 4) AS INTEGER)) FROM items WHERE itemCode LIKE 'ITM%'`).Scan(&n)
	return fmt.Sprintf("ITM%04d", n.Int64+1)
}

// itemColumns are the product fields editable from the Prodotti page.
var itemColumns = map[string]string{
	"alias": "itemCodeAlias", "description": "description", "winery": "winery", "area": "area",
}

// CreateItem adds an empty product with the next ITMnnnn code.
func (s *Service) CreateItem(ctx context.Context) (Item, error) {
	var it Item
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		it.Code = nextItemCode(ctx, tx)
		res, err := tx.ExecContext(ctx, "INSERT INTO items (itemCode, description, created_at) VALUES (?, '', ?)",
			it.Code, time.Now().Format("2006-01-02T15:04:05.000000"))
		if err != nil {
			return err
		}
		it.ID, err = res.LastInsertId()
		return err
	})
	return it, err
}

// SetItemField updates one field of one product. The description can't be
// emptied once set (it names the product in orders).
func (s *Service) SetItemField(ctx context.Context, id int64, field, value string) error {
	col, ok := itemColumns[field]
	if !ok {
		return fmt.Errorf("campo %q non valido", field)
	}
	value = strings.TrimSpace(value)
	if field == "description" && value == "" {
		return fmt.Errorf("la descrizione è obbligatoria")
	}
	res, err := s.db().ExecContext(ctx, `UPDATE items SET "`+col+`" = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		value, time.Now().Format("2006-01-02T15:04:05.000000"), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("prodotto non trovato")
	}
	return nil
}

// DeleteItem soft-deletes a product (orders keep their wine names).
func (s *Service) DeleteItem(ctx context.Context, id int64) error {
	_, err := s.db().ExecContext(ctx, "UPDATE items SET deleted_at = ? WHERE id = ?", time.Now().Format("2006-01-02T15:04:05.000000"), id)
	return err
}

// ── Statistiche ──────────────────────────────────────────────────────────────

type DiscussedListing struct {
	Inserzione string
	Venditore  string
	Data       string
	Risposte   int
}

type ActiveUser struct {
	Utente   string
	Risposte int
}

type Stats struct {
	Campagne     int
	Risposte     int
	Utenti       int
	PiuDiscusse  []DiscussedListing
	UtentiAttivi []ActiveUser
}

func (s *Service) Stats(ctx context.Context) (*Stats, error) {
	st := &Stats{}
	rows, err := s.ListingRows(ctx)
	if err != nil {
		return nil, err
	}
	// grouped by campaign key across all chats, like the original page
	idx := map[string]int{}
	for _, r := range rows {
		i, ok := idx[r.Campaign]
		if !ok {
			i = len(st.PiuDiscusse)
			idx[r.Campaign] = i
			st.PiuDiscusse = append(st.PiuDiscusse, DiscussedListing{Inserzione: r.Campaign, Venditore: r.Venditore, Data: r.Data})
		}
		st.PiuDiscusse[i].Risposte += r.Risposte
	}
	titles := map[string]string{}
	for _, r := range rows {
		if _, ok := titles[r.Campaign]; !ok && strings.TrimSpace(r.Title) != "" {
			titles[r.Campaign] = r.Title
		}
	}
	for i := range st.PiuDiscusse {
		key := st.PiuDiscusse[i].Inserzione
		if t := titles[key]; t != "" {
			st.PiuDiscusse[i].Inserzione = t
		} else {
			st.PiuDiscusse[i].Inserzione = textutil.CleanTitle(key)
		}
	}
	st.Campagne = len(idx)
	sort.SliceStable(st.PiuDiscusse, func(i, j int) bool { return st.PiuDiscusse[i].Risposte > st.PiuDiscusse[j].Risposte })
	if len(st.PiuDiscusse) > 20 {
		st.PiuDiscusse = st.PiuDiscusse[:20]
	}

	s.db().QueryRowContext(ctx, "SELECT COUNT(*) FROM replies").Scan(&st.Risposte)
	s.db().QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&st.Utenti)

	urows, err := s.db().QueryContext(ctx, `
		SELECT COALESCE(u.name,''), COUNT(r.id) AS n FROM replies r
		JOIN users u ON u.id = r.author_id
		GROUP BY r.author_id ORDER BY n DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer urows.Close()
	for urows.Next() {
		var a ActiveUser
		if urows.Scan(&a.Utente, &a.Risposte) == nil {
			a.Utente = textutil.StripEmoji(a.Utente)
			st.UtentiAttivi = append(st.UtentiAttivi, a)
		}
	}
	return st, urows.Err()
}
