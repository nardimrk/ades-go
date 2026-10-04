package service

import (
	"adesgo/internal/db"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"adesgo/internal/textutil"
)

// CleanupReport says what CleanupListings removed or merged.
type CleanupReport struct {
	Duplicates    int      // copies of the same message deleted
	MergedGroups  int      // inserzioni whose reposts were merged into the first post
	MergedPosts   int      // repost rows merged (deleted)
	RepliesMoved  int      // replies re-linked to the kept listing
	Numbered      int      // listings numbered INS260001… (by year)
	Examples      []string // a few lines to show the user
	RemainingRows int
}

var cleanupStanzaRe = regexp.MustCompile(`^(?:true|false)_[^_]+_([^_]+)_`)

type cleanRow struct {
	id                      int64
	msgID, chat, body, last string
	title, consegna         string
	created, ts             int64
	qid                     int64 // 0 = none
	replies                 int
}

func (r cleanRow) score() int {
	n := 0
	if r.replies > 0 {
		n += 4
	}
	if r.qid != 0 {
		n += 2
	}
	if !strings.HasPrefix(r.msgID, "import_") {
		n++
	}
	return n
}

// CleanupListings removes the copies of the same message (stored twice from
// the phone and as received, or imported from a chat export after being
// received live) and merges the reposts of each inserzione (posts chained
// within 15 days, of the same order) into its first post: replies move to
// it, the latest text becomes its latest edit, its initial quantities are
// the first counts the seller gave. Then the listings are numbered again
// by year in posting order (INS260001…). Without apply nothing is written.
func (s *Service) CleanupListings(ctx context.Context, apply bool) (*CleanupReport, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT l.id, l.msg_id, COALESCE(l.chat_id,''), COALESCE(l.original_body, l.body, ''),
		       COALESCE(l.last_body,''), COALESCE(l.title,''), COALESCE(l.consegna_stimata,''),
		       COALESCE(CAST(strftime('%s', l.created_at) AS INTEGER), l.timestamp, 0), COALESCE(l.timestamp,0),
		       COALESCE(l.quotation_id,0), (SELECT COUNT(*) FROM replies r WHERE r.listing_msg_id = l.msg_id)
		FROM listings l ORDER BY l.created_at, l.id`)
	if err != nil {
		return nil, err
	}
	var all []cleanRow
	for rows.Next() {
		var r cleanRow
		if err := rows.Scan(&r.id, &r.msgID, &r.chat, &r.body, &r.last, &r.title, &r.consegna, &r.created, &r.ts, &r.qid, &r.replies); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	rows.Close()
	rep := &CleanupReport{}

	// 1. copies of the same message: drop → keep
	drop := map[int64]int64{}
	byID := map[int64]*cleanRow{}
	for i := range all {
		byID[all[i].id] = &all[i]
	}
	pick := func(group []*cleanRow) {
		sort.SliceStable(group, func(i, j int) bool {
			if a, b := group[i].score(), group[j].score(); a != b {
				return a > b
			}
			return group[i].created < group[j].created
		})
		for _, r := range group[1:] {
			if _, done := drop[r.id]; !done && drop[group[0].id] == 0 {
				drop[r.id] = group[0].id
			}
		}
	}
	stanza := map[string][]*cleanRow{}
	for i := range all {
		if m := cleanupStanzaRe.FindStringSubmatch(all[i].msgID); m != nil {
			stanza[m[1]] = append(stanza[m[1]], &all[i])
		}
	}
	for _, g := range stanza {
		if len(g) > 1 {
			pick(g)
		}
	}
	norm := func(b string) string { return strings.Join(strings.Fields(b), " ") }
	for i := range all {
		imp := &all[i]
		if !strings.HasPrefix(imp.msgID, "import_") || drop[imp.id] != 0 {
			continue
		}
		for j := range all {
			o := &all[j]
			if o.id == imp.id || strings.HasPrefix(o.msgID, "import_") || drop[o.id] != 0 || o.chat != imp.chat {
				continue
			}
			if d := o.created - imp.created; d >= -3*3600 && d <= 3*3600 && norm(o.body) == norm(imp.body) {
				pick([]*cleanRow{o, imp})
				break
			}
		}
	}
	rep.Duplicates = len(drop)

	// 2. reposts: chains of the same campaign within 15 days, same order
	var live []ListingRow
	for _, r := range all {
		if drop[r.id] == 0 {
			live = append(live, ListingRow{MsgID: r.msgID, TS: r.ts, ChatID: r.chat, Testo: r.body, Campaign: textutil.CampaignKey(r.body)})
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].TS < live[j].TS })
	byMsg := map[string]*cleanRow{}
	for i := range all {
		byMsg[all[i].msgID] = &all[i]
	}
	type chain []*cleanRow
	var chains []chain
	for _, c := range GroupCampaigns(live) {
		posts := make([]*cleanRow, 0, len(c.MsgIDs))
		for _, id := range c.MsgIDs {
			posts = append(posts, byMsg[id])
		}
		sort.SliceStable(posts, func(i, j int) bool { return posts[i].created < posts[j].created })
		cur := chain{posts[0]}
		qid := posts[0].qid
		for _, p := range posts[1:] {
			prev := cur[len(cur)-1]
			if p.created-prev.created > 15*86400 || (p.qid != 0 && qid != 0 && p.qid != qid) {
				chains = append(chains, cur)
				cur, qid = chain{p}, p.qid
				continue
			}
			cur = append(cur, p)
			if qid == 0 {
				qid = p.qid
			}
		}
		chains = append(chains, cur)
	}
	type merge struct {
		first   *cleanRow
		others  []*cleanRow
		qid     int64
		title   string
		cons    string
		last    string
		ts      int64
		initial string
	}
	var merges []merge
	for _, ch := range chains {
		if len(ch) < 2 {
			continue
		}
		m := merge{first: ch[0], others: ch[1:], ts: ch[0].ts}
		ids := make([]string, len(ch))
		for i, p := range ch {
			ids[i] = p.msgID
			if m.qid == 0 {
				m.qid = p.qid
			}
			if m.title == "" && strings.TrimSpace(p.title) != "" {
				m.title = p.title
			}
			if p.consegna != "" {
				m.cons = p.consegna
			}
			if p.ts > m.ts {
				m.ts = p.ts
			}
		}
		latest := ch[len(ch)-1]
		m.last = latest.last
		if m.last == "" {
			m.last = latest.body
		}
		opts, err := s.initialOptions(ctx, ids)
		if err != nil {
			return nil, err
		}
		qty := map[string]int{}
		for _, o := range opts {
			if o.HasQty {
				qty[o.Letter] = o.Qty
			}
		}
		if len(qty) > 0 {
			b, _ := json.Marshal(qty)
			m.initial = string(b)
		}
		merges = append(merges, m)
		rep.MergedPosts += len(m.others)
		if len(rep.Examples) < 8 {
			title := m.title
			if title == "" {
				title = textutil.CampaignKey(m.first.body)
			}
			rep.Examples = append(rep.Examples, fmt.Sprintf("%d post → 1: %s (iniziale %s)", len(ch), title, m.initial))
		}
	}
	rep.MergedGroups = len(merges)

	// 3. write (or not)
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	moveReplies := func(from, to *cleanRow) error {
		res, err := tx.ExecContext(ctx, "UPDATE replies SET listing_msg_id = ? WHERE listing_msg_id = ?", to.msgID, from.msgID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		rep.RepliesMoved += int(n)
		if _, err := tx.ExecContext(ctx, "UPDATE quotations SET msg_id = ? WHERE msg_id = ?", to.msgID, from.msgID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM listings WHERE id = ?", from.id)
		return err
	}
	for dropID, keepID := range drop {
		d, k := byID[dropID], byID[keepID]
		if k.qid == 0 && d.qid != 0 {
			if _, err := tx.ExecContext(ctx, "UPDATE listings SET quotation_id = ? WHERE id = ?", d.qid, k.id); err != nil {
				return nil, err
			}
		}
		if err := moveReplies(d, k); err != nil {
			return nil, err
		}
	}
	for _, m := range merges {
		for _, o := range m.others {
			if err := moveReplies(o, m.first); err != nil {
				return nil, err
			}
		}
		var qid, initial, title, cons, last any
		if m.qid != 0 {
			qid = m.qid
		}
		if m.initial != "" {
			initial = m.initial
		}
		if m.title != "" {
			title = m.title
		}
		if m.cons != "" {
			cons = m.cons
		}
		if m.last != "" && m.last != m.first.body {
			last = m.last
		}
		if _, err := tx.ExecContext(ctx, `UPDATE listings SET quotation_id = COALESCE(?, quotation_id),
			initial_qty = COALESCE(?, initial_qty), title = COALESCE(?, title), consegna_stimata = COALESCE(?, consegna_stimata),
			last_body = COALESCE(?, last_body), timestamp = ? WHERE id = ?`, qid, initial, title, cons, last, m.ts, m.first.id); err != nil {
			return nil, err
		}
	}
	if err := renumberListings(ctx, tx, rep); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM listings").Scan(&rep.RemainingRows); err != nil {
		return nil, err
	}
	if !apply {
		return rep, nil // rolled back by the defer
	}
	return rep, tx.Commit()
}

// renumberListings numbers every listing again by year, in posting order
// (INS260001…, see db.RenumberListings).
func renumberListings(ctx context.Context, tx *sql.Tx, rep *CleanupReport) error {
	if _, err := db.RenumberListings(ctx, tx); err != nil {
		return err
	}
	return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM listings").Scan(&rep.Numbered)
}
