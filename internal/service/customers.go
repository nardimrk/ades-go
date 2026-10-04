package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// A customer can have several ids in users: the WhatsApp one (…@lid), the
// name of an imported chat (import:<name>), a manual one (manual-…). The
// duplicates point to the main record (users.merged_into), which holds the
// customer's data and code (CLI0001). Replies and orders keep their original
// id; every page resolves it to the main record.

// customerKind tells where an id comes from.
func customerKind(id string) string {
	switch {
	case strings.HasSuffix(id, "@lid"), strings.HasSuffix(id, "@c.us"):
		return "whatsapp"
	case strings.HasPrefix(id, "import:"):
		return "import"
	case IsManualUser(id):
		return "manual"
	}
	return "other"
}

// kindRank orders the ids of a merge: the main record is the WhatsApp one,
// else the manual one, else the imported one.
func kindRank(id string) int {
	switch customerKind(id) {
	case "whatsapp":
		return 0
	case "manual":
		return 1
	case "import":
		return 2
	}
	return 3
}

// canonicalID resolves an id to its main record.
func (s *Service) canonicalID(ctx context.Context, id string) (string, error) {
	var m sql.NullString
	err := s.db().QueryRowContext(ctx, "SELECT merged_into FROM users WHERE id = ?", id).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("cliente %q non trovato", id)
	}
	if err != nil {
		return "", err
	}
	if m.Valid && m.String != "" {
		return m.String, nil
	}
	return id, nil
}

// MergeCustomers merges the customer alias into main: alias (and the ids
// already merged into it) point to main; the main record's empty fields
// take the alias's values; saved orders and manual orders of alias move to
// main. Undo with UnmergeCustomer (orders stay with main).
func (s *Service) MergeCustomers(ctx context.Context, alias, main string) error {
	var err error
	if main, err = s.canonicalID(ctx, main); err != nil {
		return err
	}
	if alias, err = s.canonicalID(ctx, alias); err != nil {
		return err
	}
	if alias == main {
		return errors.New("è già lo stesso cliente")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET merged_into = ? WHERE merged_into = ?", main, alias); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET merged_into = ? WHERE id = ?", main, alias); err != nil {
			return err
		}
		// the main record keeps its data; what it lacks comes from the alias
		// (a name made only of the number or "." is not a name)
		if _, err := tx.ExecContext(ctx, `UPDATE users SET
			name      = CASE WHEN COALESCE(TRIM(name),'') = '' OR name LIKE '%@%' OR TRIM(name) = '.' THEN (SELECT name FROM users a WHERE a.id = ?) ELSE name END,
			telefono  = COALESCE(NULLIF(TRIM(telefono),''), (SELECT NULLIF(TRIM(telefono),'') FROM users a WHERE a.id = ?)),
			indirizzo = COALESCE(NULLIF(TRIM(indirizzo),''), (SELECT NULLIF(TRIM(indirizzo),'') FROM users a WHERE a.id = ?)),
			"città"   = COALESCE(NULLIF(TRIM("città"),''), (SELECT NULLIF(TRIM("città"),'') FROM users a WHERE a.id = ?)),
			provincia = COALESCE(NULLIF(TRIM(provincia),''), (SELECT NULLIF(TRIM(provincia),'') FROM users a WHERE a.id = ?)),
			regione   = COALESCE(NULLIF(TRIM(regione),''), (SELECT NULLIF(TRIM(regione),'') FROM users a WHERE a.id = ?)),
			cap       = COALESCE(NULLIF(TRIM(cap),''), (SELECT NULLIF(TRIM(cap),'') FROM users a WHERE a.id = ?)),
			updated_at = datetime('now')
			WHERE id = ?`, alias, alias, alias, alias, alias, alias, alias, main); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE orders SET user_id = ? WHERE user_id = ?", main, alias); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE quotations SET manual_client_id = ? WHERE manual_client_id = ?", main, alias)
		return err
	})
}

// UnmergeCustomer makes a merged id a customer of its own again.
func (s *Service) UnmergeCustomer(ctx context.Context, id string) error {
	res, err := s.db().ExecContext(ctx, "UPDATE users SET merged_into = NULL WHERE id = ? AND merged_into IS NOT NULL", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("questo cliente non è unito a un altro")
	}
	return s.Store.AssignCustomerCodes(ctx)
}

// ── suggestions ──────────────────────────────────────────────────────────────

// MergeCandidate is one side of a suggested merge.
type MergeCandidate struct {
	ID       string
	Code     string
	Name     string
	Kind     string // whatsapp, import, manual
	Replies  int
	First    string // first / last reply "2025-03"
	Last     string
	Telefono string
}

// MergeSuggestion: two customers that are probably the same person.
type MergeSuggestion struct {
	Main, Alias MergeCandidate // Alias is merged into Main
	Score       int
	Reasons     []string
}

var nameJunkRe = regexp.MustCompile(`(?i)\b(vino|cell|whatsapp|amico|amica|cugino|cugina|figlio|figlia|avv|dott|ing|sig|sig\.ra)\b`)

// nameParticles are parts of surnames ("Dalla Fontana") that alone say
// nothing: two people sharing only these are not the same person.
var nameParticles = map[string]bool{"da": true, "dal": true, "dalla": true, "dalle": true, "de": true, "dei": true, "del": true,
	"della": true, "delle": true, "di": true, "la": true, "lo": true, "le": true, "van": true, "von": true}

// nameKey compares names ignoring case, accents, emoji and punctuation.
func nameKey(s string) string {
	t := norm.NFKD.String(s)
	var b strings.Builder
	for _, r := range t {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func nameTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(nameKey(nameJunkRe.ReplaceAllString(s, " "))) {
		if len([]rune(w)) >= 2 && !nameParticles[w] {
			out[w] = true
		}
	}
	return out
}

// MergeSuggestions proposes pairs of customers that are probably the same
// person (an imported chat name and a WhatsApp id, or a manual customer and
// another one), best first. Only the evidence is shown: the user decides.
func (s *Service) MergeSuggestions(ctx context.Context) ([]MergeSuggestion, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT u.id, COALESCE(u.customer_code,''), COALESCE(u.name,''), COALESCE(u.telefono,''),
		       COUNT(r.id), COALESCE(strftime('%Y-%m', MIN(r.timestamp), 'unixepoch'),''), COALESCE(strftime('%Y-%m', MAX(r.timestamp), 'unixepoch'),'')
		FROM users u LEFT JOIN replies r ON r.author_id = u.id
		WHERE u.merged_into IS NULL AND COALESCE(u.is_seller,0) = 0 AND u.id NOT LIKE '%@g.us'
		GROUP BY u.id`)
	if err != nil {
		return nil, err
	}
	var all []MergeCandidate
	for rows.Next() {
		var c MergeCandidate
		if err := rows.Scan(&c.ID, &c.Code, &c.Name, &c.Telefono, &c.Replies, &c.First, &c.Last); err != nil {
			rows.Close()
			return nil, err
		}
		c.Kind = customerKind(c.ID)
		all = append(all, c)
	}
	rows.Close()

	var out []MergeSuggestion
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			a, b := all[i], all[j]
			if a.Kind == b.Kind && a.Kind != "manual" {
				continue // two WhatsApp ids, or two import names, are two people
			}
			score, reasons := 0, []string(nil)
			if a.Telefono != "" && a.Telefono == b.Telefono {
				score += 100
				reasons = append(reasons, "stesso telefono "+a.Telefono)
			}
			ka, kb := nameKey(a.Name), nameKey(b.Name)
			ta, tb := nameTokens(a.Name), nameTokens(b.Name)
			switch {
			case ka != "" && ka == kb:
				score += 60
				reasons = append(reasons, "stesso nome")
			case len(ta) >= 2 && len(tb) >= 2 && (subset(ta, tb) || subset(tb, ta)):
				score += 45
				reasons = append(reasons, "un nome contiene l'altro")
			case len(ta) >= 2 && len(tb) >= 2 && overlap(ta, tb) >= 2:
				score += 35
				reasons = append(reasons, "nome e cognome in comune")
			}
			if score == 0 {
				continue
			}
			// one stops writing (export) before the other starts (live)
			if a.Last != "" && b.First != "" && a.Last <= b.First || b.Last != "" && a.First != "" && b.Last <= a.First {
				score += 10
				reasons = append(reasons, "attivi uno dopo l'altro")
			}
			main, alias := a, b
			if kindRank(b.ID) < kindRank(a.ID) {
				main, alias = b, a
			}
			out = append(out, MergeSuggestion{Main: main, Alias: alias, Score: score, Reasons: reasons})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Alias.Replies+out[i].Main.Replies > out[j].Alias.Replies+out[j].Main.Replies
	})
	return out, nil
}

func subset(a, b map[string]bool) bool {
	for w := range a {
		if !b[w] {
			return false
		}
	}
	return true
}

func overlap(a, b map[string]bool) int {
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return n
}

// MergedCustomer is an id merged into another customer (shown with "Separa").
type MergedCustomer struct {
	ID, Name, Kind   string
	MainID, MainName string
	MainCode         string
}

// MergedCustomers lists the merges done, by main customer.
func (s *Service) MergedCustomers(ctx context.Context) ([]MergedCustomer, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT a.id, COALESCE(a.name,''), m.id, COALESCE(m.name,''), COALESCE(m.customer_code,'')
		FROM users a JOIN users m ON m.id = a.merged_into ORDER BY m.name, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MergedCustomer
	for rows.Next() {
		var c MergedCustomer
		if err := rows.Scan(&c.ID, &c.Name, &c.MainID, &c.MainName, &c.MainCode); err != nil {
			return nil, err
		}
		c.Kind = customerKind(c.ID)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ── cleanup ──────────────────────────────────────────────────────────────────

// CustomersReport says what CleanupCustomers changed.
type CustomersReport struct {
	GroupsRemoved  []string // WhatsApp groups saved as customers
	AuthorsCreated []string // reply authors without a customer record
	Sellers        []string // ids marked as the seller (not customers)
	Coded          int      // customers with a code CLI0001…
}

// CleanupCustomers: removes the WhatsApp groups saved as customers, creates
// the customer records missing for reply authors, marks the seller's ids
// (authors of the listings, and imported names of the group itself or of
// the seller), then gives every customer a code. Without apply nothing is
// written.
func (s *Service) CleanupCustomers(ctx context.Context, apply bool) (*CustomersReport, error) {
	rep := &CustomersReport{}
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	list := func(q string, args ...any) ([]string, error) {
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	// groups (no reply or order points to them)
	if rep.GroupsRemoved, err = list(`SELECT id FROM users WHERE id LIKE '%@g.us'
		AND id NOT IN (SELECT author_id FROM replies WHERE author_id IS NOT NULL)
		AND id NOT IN (SELECT user_id FROM orders WHERE user_id IS NOT NULL)`); err != nil {
		return nil, err
	}
	for _, id := range rep.GroupsRemoved {
		if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id); err != nil {
			return nil, err
		}
	}
	// reply authors with no record
	if rep.AuthorsCreated, err = list(`SELECT DISTINCT author_id FROM replies
		WHERE author_id IS NOT NULL AND author_id != '' AND author_id NOT IN (SELECT id FROM users)`); err != nil {
		return nil, err
	}
	for _, id := range rep.AuthorsCreated {
		name := strings.TrimPrefix(id, "import:")
		if _, err := tx.ExecContext(ctx, "INSERT INTO users (id, name, updated_at) VALUES (?, ?, datetime('now'))", id, name); err != nil {
			return nil, err
		}
	}
	// the seller: authors of the listings, and imported names equal to a
	// seller's name or to the group's ("ADE's Wine Club")
	sellers, err := list(`SELECT DISTINCT author_id FROM listings WHERE author_id IS NOT NULL AND author_id != ''`)
	if err != nil {
		return nil, err
	}
	sellerNames := map[string]bool{"ade s wine club": true}
	for _, id := range sellers {
		var n string
		tx.QueryRowContext(ctx, "SELECT COALESCE(name,'') FROM users WHERE id = ?", id).Scan(&n)
		if k := nameKey(n); k != "" {
			sellerNames[k] = true
		}
	}
	imports, err := list("SELECT id FROM users WHERE id LIKE 'import:%'")
	if err != nil {
		return nil, err
	}
	for _, id := range imports {
		if sellerNames[nameKey(strings.TrimPrefix(id, "import:"))] {
			sellers = append(sellers, id)
		}
	}
	for _, id := range sellers {
		res, err := tx.ExecContext(ctx, "UPDATE users SET is_seller = 1, customer_code = NULL WHERE id = ? AND COALESCE(is_seller,0) = 0", id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			rep.Sellers = append(rep.Sellers, id)
		}
	}
	if !apply {
		return rep, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := s.Store.AssignCustomerCodes(ctx); err != nil {
		return nil, err
	}
	err = s.db().QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE customer_code IS NOT NULL").Scan(&rep.Coded)
	return rep, err
}
