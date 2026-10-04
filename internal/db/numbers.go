package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// Order and listing numbers carry the year and restart every year:
// "ORD260544" is the 544th order of 2026 (by order date), "INS260725" the
// 725th listing posted in 2026.
const (
	OrderPrefix   = "ORD"
	ListingPrefix = "INS"
)

// Code is prefix + two-digit year + the number, at least 4 digits.
func Code(prefix string, year, n int) string {
	return fmt.Sprintf("%s%02d%04d", prefix, year%100, n)
}

// queryer is a *sql.DB or a *sql.Tx.
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// nextCode is the next free code of that year in table.col.
func nextCode(ctx context.Context, q queryer, table, col, prefix string, year int) (string, error) {
	p := fmt.Sprintf("%s%02d", prefix, year%100)
	var max int
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(substr(`+col+`, ?) AS INTEGER)), 0) FROM `+table+`
		WHERE `+col+` GLOB ?`, len(p)+1, p+"[0-9][0-9][0-9][0-9]*").Scan(&max)
	if err != nil {
		return "", err
	}
	return Code(prefix, year, max+1), nil
}

// NextOrderNumber is the number of a new order dated date ("2026-10-04";
// today when it can't be read).
func NextOrderNumber(ctx context.Context, q queryer, date string) (string, error) {
	return nextCode(ctx, q, "quotations", "quotation_number", OrderPrefix, yearOf(date))
}

func yearOf(date string) int {
	if len(date) >= 4 {
		if y, err := strconv.Atoi(date[:4]); err == nil && y > 1900 {
			return y
		}
	}
	return time.Now().Year()
}

// listingYear: the year a listing was posted (its WhatsApp timestamp, local
// time), else when it was stored.
func listingYear(ts int64, createdAt string) int {
	if ts > 0 {
		return time.Unix(ts, 0).Year()
	}
	return yearOf(createdAt)
}

// Renumbered: one code changed by Renumber.
type Renumbered struct {
	Kind     string // "ordine" or "inserzione"
	Old, New string
}

// Renumber gives every order (quotations, and the confirmed orders that
// point to them) and every listing a code of the new form: by year of the
// order date / of the post, numbered from 0001 in date order. It returns
// what changed. Run it in a transaction: codes go through a temporary value
// so they never collide.
func Renumber(ctx context.Context, tx *sql.Tx) ([]Renumbered, error) {
	var out []Renumbered
	// orders, by year of the order date
	quots, err := loadRecs(ctx, tx, `SELECT id, COALESCE(quotation_number,''), COALESCE(quotation_date,'') FROM quotations
		ORDER BY quotation_date, id`, func(r *sql.Rows, x *numRec) error { return r.Scan(&x.id, &x.old, &x.date) })
	if err != nil {
		return nil, err
	}
	// codes go through a temporary value so an old one never meets a new one
	if _, err := tx.ExecContext(ctx, "UPDATE quotations SET quotation_number = 'tmp-' || id"); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE orders SET quotation_number = 'tmp:' || quotation_number WHERE quotation_number IS NOT NULL"); err != nil {
		return nil, err
	}
	seq := map[int]int{}
	for _, q := range quots {
		y := yearOf(q.date)
		seq[y]++
		code := Code(OrderPrefix, y, seq[y])
		if _, err := tx.ExecContext(ctx, "UPDATE quotations SET quotation_number = ? WHERE id = ?", code, q.id); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE orders SET quotation_number = ? WHERE quotation_number = 'tmp:' || ?", code, q.old); err != nil {
			return nil, err
		}
		if q.old != code {
			out = append(out, Renumbered{"ordine", q.old, code})
		}
	}
	// confirmed orders of a quotation that no longer exists keep their code
	if _, err := tx.ExecContext(ctx, "UPDATE orders SET quotation_number = substr(quotation_number, 5) WHERE quotation_number LIKE 'tmp:%'"); err != nil {
		return nil, err
	}
	ls, err := RenumberListings(ctx, tx)
	if err != nil {
		return nil, err
	}
	out = append(out, ls...)
	// keep the old codes, so old links and searches still find them: first
	// earlier renames follow this one (IMP0544 → ORD260544 → ORD260543),
	// then this run's are added
	now := map[string]string{}
	for _, r := range out {
		now[r.Old] = r.New
	}
	type prev struct{ old, new string }
	var prevs []prev
	rows, err := tx.QueryContext(ctx, "SELECT old, new FROM number_renames")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p prev
		if err := rows.Scan(&p.old, &p.new); err != nil {
			rows.Close()
			return nil, err
		}
		prevs = append(prevs, p)
	}
	rows.Close()
	for _, p := range prevs {
		if n, ok := now[p.new]; ok {
			if _, err := tx.ExecContext(ctx, "UPDATE number_renames SET new = ? WHERE old = ?", n, p.old); err != nil {
				return nil, err
			}
		}
	}
	for _, r := range out {
		if _, err := tx.ExecContext(ctx, `INSERT INTO number_renames (old, new, kind, renamed_at) VALUES (?, ?, ?, datetime('now'))
			ON CONFLICT(old) DO UPDATE SET new = excluded.new, kind = excluded.kind, renamed_at = excluded.renamed_at`, r.Old, r.New, r.Kind); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RenamedTo returns the current code of an old one ("IMP0544" →
// "ORD260544"), "" when it was never renamed.
func (s *Store) RenamedTo(ctx context.Context, old string) string {
	var n string
	s.DB.QueryRowContext(ctx, "SELECT new FROM number_renames WHERE old = ?", old).Scan(&n)
	return n
}

type numRec struct {
	id        int64
	old, date string
	ts        int64
}

func loadRecs(ctx context.Context, tx *sql.Tx, query string, scan func(*sql.Rows, *numRec) error) ([]numRec, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rs []numRec
	for rows.Next() {
		var r numRec
		if err := scan(rows, &r); err != nil {
			return nil, err
		}
		rs = append(rs, r)
	}
	return rs, rows.Err()
}

// RenumberListings numbers every listing again by year of its post
// ("INS260001", …), in posting order, and returns what changed.
func RenumberListings(ctx context.Context, tx *sql.Tx) ([]Renumbered, error) {
	var out []Renumbered
	// listings, by year of the post
	lst, err := loadRecs(ctx, tx, `SELECT id, COALESCE(listing_number,''), COALESCE(created_at,''), COALESCE(timestamp,0) FROM listings
		ORDER BY timestamp, id`, func(r *sql.Rows, x *numRec) error { return r.Scan(&x.id, &x.old, &x.date, &x.ts) })
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE listings SET listing_number = 'tmp-' || id"); err != nil {
		return nil, err
	}
	seq := map[int]int{}
	for _, l := range lst {
		y := listingYear(l.ts, l.date)
		seq[y]++
		code := Code(ListingPrefix, y, seq[y])
		if _, err := tx.ExecContext(ctx, "UPDATE listings SET listing_number = ? WHERE id = ?", code, l.id); err != nil {
			return nil, err
		}
		if l.old != code {
			out = append(out, Renumbered{"inserzione", l.old, code})
		}
	}
	return out, nil
}
