package service

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// A reviewed batch of product corrections (produced by an AI or by hand):
// one line per product "id|new description|cantina|area|del". Empty fields
// leave the stored value as it is; "del" soft-deletes the product (orders
// keep their wine names). "+|description|cantina|area" adds a new product.
// Applied by cmd/prodotti -review.

type ProductDecision struct {
	ID          int64
	Description string
	Winery      string
	Area        string
	Delete      bool
	Add         bool // new product (ID 0)
}

// areaCodes are shorthands usable in the area column.
var areaCodes = map[string]string{
	"PAU": "Bordeaux - Pauillac", "MAR": "Bordeaux - Margaux", "SJU": "Bordeaux - Saint-Julien",
	"SES": "Bordeaux - Saint-Estèphe", "PES": "Bordeaux - Pessac-Léognan", "SEM": "Bordeaux - Saint-Émilion",
	"POM": "Bordeaux - Pomerol", "LAL": "Bordeaux - Lalande-de-Pomerol", "SAU": "Bordeaux - Sauternes",
	"BAR": "Bordeaux - Barsac", "BDX": "Bordeaux", "HME": "Bordeaux - Haut-Médoc", "MOU": "Bordeaux - Moulis",
	"CAS": "Bordeaux - Castillon Côtes de Bordeaux", "CHA": "Champagne", "MEU": "Borgogna - Meursault",
	"VAL": "Lombardia - Valtellina",
}

// ReadProductDecisions parses decision files ("#" lines are comments).
func ReadProductDecisions(paths []string) ([]ProductDecision, error) {
	var out []ProductDecision
	seen := map[int64]string{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		n := 0
		for sc.Scan() {
			n++
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Split(line, "|")
			for len(parts) < 5 {
				parts = append(parts, "")
			}
			if strings.TrimSpace(parts[0]) == "+" {
				area := strings.TrimSpace(parts[3])
				if full, ok := areaCodes[area]; ok {
					area = full
				}
				if strings.TrimSpace(parts[1]) == "" {
					f.Close()
					return nil, fmt.Errorf("%s:%d: new product without description", p, n)
				}
				out = append(out, ProductDecision{Add: true, Description: strings.TrimSpace(parts[1]),
					Winery: strings.TrimSpace(parts[2]), Area: area})
				continue
			}
			id, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("%s:%d: bad id %q", p, n, parts[0])
			}
			where := fmt.Sprintf("%s:%d", p, n)
			if prev, dup := seen[id]; dup {
				f.Close()
				return nil, fmt.Errorf("%s: id %d already decided at %s", where, id, prev)
			}
			seen[id] = where
			area := strings.TrimSpace(parts[3])
			if full, ok := areaCodes[area]; ok {
				area = full
			}
			out = append(out, ProductDecision{
				ID: id, Description: strings.TrimSpace(parts[1]), Winery: strings.TrimSpace(parts[2]),
				Area: area, Delete: strings.TrimSpace(parts[4]) == "del",
			})
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ProductReviewReport describes what applying decisions does.
type ProductReviewReport struct {
	Renamed, Winery, Area, Deleted, Unchanged, Added int
	Lines                                     []string // one per change, for review
}

// ApplyProductDecisions validates the decisions against the products table
// (ids must exist; a rename must not collide with another product that
// stays) and, with apply, writes them in one transaction.
func (s *Service) ApplyProductDecisions(ctx context.Context, ds []ProductDecision, apply bool) (*ProductReviewReport, error) {
	items, err := s.Items(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[int64]Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	deleted := map[int64]bool{}
	for _, d := range ds {
		if d.Add {
			continue
		}
		if _, ok := byID[d.ID]; !ok {
			return nil, fmt.Errorf("product %d not found (or already deleted)", d.ID)
		}
		if d.Delete {
			deleted[d.ID] = true
		}
	}
	// final name of every product that stays, to detect collisions
	final := map[string]int64{}
	for _, it := range items {
		if !deleted[it.ID] {
			final[ProductKey(it.Description)] = it.ID
		}
	}
	for _, d := range ds {
		if d.Add {
			if other, ok := final[ProductKey(d.Description)]; ok {
				return nil, fmt.Errorf("new product %q already exists as %d %q", d.Description, other, byID[other].Description)
			}
			final[ProductKey(d.Description)] = 0
			continue
		}
		if d.Delete || d.Description == "" {
			continue
		}
		old := byID[d.ID].Description
		if other, ok := final[ProductKey(d.Description)]; ok && other != d.ID {
			return nil, fmt.Errorf("renaming %d %q to %q collides with product %d %q",
				d.ID, old, d.Description, other, byID[other].Description)
		}
		delete(final, ProductKey(old))
		final[ProductKey(d.Description)] = d.ID
	}

	rep := &ProductReviewReport{}
	now := time.Now().Format("2006-01-02T15:04:05.000000")
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		exec := func(q string, args ...any) error {
			if !apply {
				return nil
			}
			_, err := tx.ExecContext(ctx, q, args...)
			return err
		}
		for _, d := range ds {
			if d.Add {
				rep.Added++
				rep.Lines = append(rep.Lines, fmt.Sprintf("ADD       %s [%s · %s]", d.Description, d.Winery, d.Area))
				if err := exec(`INSERT INTO items (itemCode, description, winery, area, created_at) VALUES (?, ?, ?, ?, ?)`,
					nextItemCode(ctx, tx), d.Description, d.Winery, d.Area, now); err != nil {
					return err
				}
				continue
			}
			it := byID[d.ID]
			if d.Delete {
				rep.Deleted++
				rep.Lines = append(rep.Lines, fmt.Sprintf("DEL  %4d %s", it.ID, it.Description))
				if err := exec("UPDATE items SET deleted_at = ? WHERE id = ?", now, it.ID); err != nil {
					return err
				}
				continue
			}
			var changes []string
			if d.Description != "" && d.Description != it.Description {
				rep.Renamed++
				changes = append(changes, fmt.Sprintf("name → %q", d.Description))
				if err := exec("UPDATE items SET description = ?, updated_at = ? WHERE id = ?", d.Description, now, it.ID); err != nil {
					return err
				}
			}
			if d.Winery != "" && d.Winery != it.Winery {
				rep.Winery++
				changes = append(changes, fmt.Sprintf("cantina %q", d.Winery))
				if err := exec("UPDATE items SET winery = ?, updated_at = ? WHERE id = ?", d.Winery, now, it.ID); err != nil {
					return err
				}
			}
			if d.Area != "" && d.Area != it.Area {
				rep.Area++
				changes = append(changes, fmt.Sprintf("area %q", d.Area))
				if err := exec("UPDATE items SET area = ?, updated_at = ? WHERE id = ?", d.Area, now, it.ID); err != nil {
					return err
				}
			}
			if len(changes) == 0 {
				rep.Unchanged++
				continue
			}
			rep.Lines = append(rep.Lines, fmt.Sprintf("EDIT %4d %s: %s", it.ID, it.Description, strings.Join(changes, ", ")))
		}
		return nil
	})
	return rep, err
}
