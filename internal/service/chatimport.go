package service

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"adesgo/internal/textutil"
)

// Import of WhatsApp ".txt" chat exports (Importa Chat page).

const (
	listingSimilarityThreshold = 0.9
	listingSimilarityMaxDays   = 15
)

type ImportItem struct {
	textutil.ExportMessage
	Index        int
	IsListing    bool
	ListingIndex int // index of the listing a reply belongs to, -1 = none
}

type MergeGroup struct {
	Canonical  ImportItem
	Duplicates []ImportItem
	Replies    []ImportItem
}

// ClassifyExport tags messages as listings or replies. An owner message is a
// listing if it has a price, or if (numbers stripped) it is ≥ 0.9 similar to
// a listing the owner posted within the previous 15 days.
func ClassifyExport(msgs []textutil.ExportMessage, ownerLower string) []ImportItem {
	type prior struct {
		dt   *time.Time
		norm string
	}
	var priors []prior
	items := make([]ImportItem, 0, len(msgs))
	current := -1
	for i, m := range msgs {
		isListing := false
		norm := ""
		if strings.ToLower(strings.TrimSpace(m.Author)) == ownerLower {
			if textutil.HasImportPrice(m.Body) {
				isListing = true
			} else {
				norm = textutil.StripNumbers(m.Body)
				for k := len(priors) - 1; k >= 0; k-- {
					p := priors[k]
					if m.DT != nil && p.dt != nil && absInt(floorDays(m.DT.Sub(*p.dt))) > listingSimilarityMaxDays {
						continue
					}
					if textutil.Ratio(norm, p.norm) >= listingSimilarityThreshold {
						isListing = true
						break
					}
				}
			}
		}
		it := ImportItem{ExportMessage: m, Index: i, IsListing: isListing, ListingIndex: current}
		if isListing {
			current = i
			it.ListingIndex = -1
			if norm == "" {
				norm = textutil.StripNumbers(m.Body)
			}
			priors = append(priors, prior{m.DT, norm})
		}
		items = append(items, it)
	}
	return items
}

// floorDays mirrors Python's timedelta.days (floor of the day count).
func floorDays(d time.Duration) int { return int(math.Floor(d.Hours() / 24)) }

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func calendarDays(a, b time.Time) int {
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(da.Sub(db).Hours() / 24)
}

// MergeSimilarListings folds reposts of the same listing into the earliest
// one. A repost chains onto a group if it's within 15 calendar days of the
// group's previous repost, so a slow day-by-day drawdown still merges.
func MergeSimilarListings(items []ImportItem) []MergeGroup {
	repliesBy := map[int][]ImportItem{}
	for _, it := range items {
		if !it.IsListing {
			repliesBy[it.ListingIndex] = append(repliesBy[it.ListingIndex], it)
		}
	}
	type group struct {
		MergeGroup
		indices []int
		lastDT  *time.Time
		norm    string
	}
	var groups []*group
	for _, l := range items {
		if !l.IsListing {
			continue
		}
		norm := textutil.StripNumbers(l.Body)
		var best *group
		bestRatio := 0.0
		for _, g := range groups {
			if l.DT != nil && g.lastDT != nil && calendarDays(*l.DT, *g.lastDT) > listingSimilarityMaxDays {
				continue
			}
			r := textutil.Ratio(norm, g.norm)
			if r >= listingSimilarityThreshold && r > bestRatio {
				best, bestRatio = g, r
			}
		}
		if best == nil {
			best = &group{MergeGroup: MergeGroup{Canonical: l}, lastDT: l.DT, norm: norm}
			groups = append(groups, best)
		} else {
			best.Duplicates = append(best.Duplicates, l)
			if l.DT != nil {
				best.lastDT = l.DT
			}
		}
		best.indices = append(best.indices, l.Index)
	}
	out := make([]MergeGroup, 0, len(groups))
	for _, g := range groups {
		for _, idx := range g.indices {
			g.Replies = append(g.Replies, repliesBy[idx]...)
		}
		sort.SliceStable(g.Replies, func(i, j int) bool { return g.Replies[i].Index < g.Replies[j].Index })
		out = append(out, g.MergeGroup)
	}
	return out
}

// AuthorCount is an author of the export with their message count.
type AuthorCount struct {
	Author string
	Count  int
}

// TopAuthors returns up to 30 authors, most active first.
func TopAuthors(msgs []textutil.ExportMessage) []AuthorCount {
	idx := map[string]int{}
	var out []AuthorCount
	for _, m := range msgs {
		i, ok := idx[m.Author]
		if !ok {
			i = len(out)
			idx[m.Author] = i
			out = append(out, AuthorCount{Author: m.Author})
		}
		out[i].Count++
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

func importTS(it ImportItem) int64 {
	if it.DT != nil {
		return it.DT.Unix()
	}
	return time.Now().Unix()
}

func importMsgID(chatID string, idx int, ts int64) string {
	return fmt.Sprintf("import_%s_%d_%d", chatID, ts, idx)
}

// ImportGroups writes the merged listings and their replies (reposts become
// replies of the canonical listing). Returns inserted counts.
func (s *Service) ImportGroups(ctx context.Context, groups []MergeGroup, chatID, chatName string) (listings, replies int, err error) {
	usersByName := map[string]string{}
	rows, err := s.db().QueryContext(ctx, "SELECT id, name FROM users WHERE name IS NOT NULL AND name != ''")
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			k := strings.ToLower(strings.TrimSpace(name))
			if _, ok := usersByName[k]; !ok {
				usersByName[k] = id
			}
		}
	}
	rows.Close()

	err = s.inTx(ctx, func(tx *sql.Tx) error {
		insertReply := func(it ImportItem, listingMsgID string) error {
			ts := importTS(it)
			authorID, err := resolveImportAuthor(ctx, tx, it.Author, usersByName)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO replies
				(msg_id, listing_msg_id, chat_id, author_id, body, timestamp, created_at)
				VALUES (?, ?, ?, ?, ?, ?, datetime(?, 'unixepoch'))`,
				importMsgID(chatID, it.Index, ts), listingMsgID, chatID, authorID, it.Body, ts, ts)
			replies++
			return err
		}
		for _, g := range groups {
			ts := importTS(g.Canonical)
			msgID := importMsgID(chatID, g.Canonical.Index, ts)
			// already stored (received live, or a previous import, whose
			// times may be off by the time zone): its replies go there
			if id, ok := existingListing(ctx, tx, chatID, g.Canonical.Body, ts); ok {
				for _, r := range append(append([]ImportItem(nil), g.Duplicates...), g.Replies...) {
					if err := insertReply(r, id); err != nil {
						return err
					}
				}
				continue
			}
			f := textutil.ParseImportFields(g.Canonical.Body)
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO listings
				(msg_id, chat_id, chat_name, author_name, body, original_body, wine_name, price, vintage, timestamp, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime(?, 'unixepoch'))`,
				msgID, chatID, chatName, g.Canonical.Author, g.Canonical.Body, g.Canonical.Body, f.WineName, f.Price, f.Vintage, ts, ts); err != nil {
				return err
			}
			listings++
			for _, d := range g.Duplicates {
				if err := insertReply(d, msgID); err != nil {
					return err
				}
			}
			for _, r := range g.Replies {
				if err := insertReply(r, msgID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		err = s.Store.AssignListingNumbers(ctx)
	}
	return listings, replies, err
}

// existingListing finds a listing of the chat with the same text posted
// within 3 hours of ts (an import may be shifted by the time zone).
func existingListing(ctx context.Context, tx *sql.Tx, chatID, body string, ts int64) (string, bool) {
	rows, err := tx.QueryContext(ctx, `SELECT msg_id, COALESCE(original_body, body, '') FROM listings
		WHERE chat_id = ? AND CAST(strftime('%s', created_at) AS INTEGER) BETWEEN ? AND ?`, chatID, ts-3*3600, ts+3*3600)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	want := strings.Join(strings.Fields(body), " ")
	for rows.Next() {
		var id, b string
		if rows.Scan(&id, &b) == nil && strings.Join(strings.Fields(b), " ") == want {
			return id, true
		}
	}
	return "", false
}

// resolveImportAuthor finds a user by display name, or creates a stable
// placeholder ("import:<name>") so re-imports resolve consistently.
func resolveImportAuthor(ctx context.Context, tx *sql.Tx, name string, byName map[string]string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if id, ok := byName[key]; ok {
		return id, nil
	}
	id := "import:" + strings.TrimSpace(name)
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, name, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(id) DO NOTHING`, id, strings.TrimSpace(name)); err != nil {
		return "", err
	}
	byName[key] = id
	return id, nil
}
