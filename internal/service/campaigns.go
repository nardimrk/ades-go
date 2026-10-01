package service

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"adesgo/internal/textutil"
)

// A campaign groups a listing and its reposts (same CampaignKey in a chat).

type ListingRow struct {
	MsgID       string
	TS          int64
	Data        string // created_at "YYYY-MM-DD HH:MM:SS"
	ChatID      string
	Venditore   string
	Testo       string
	QuotationID *int64
	Title       string
	Risposte    int
	Campaign    string
}

type Campaign struct {
	ChatID       string
	Key          string
	MsgIDs       []string
	Data         string // first post
	LastData     string // latest post
	Testo        string // latest body
	Venditore    string
	Risposte     int
	NUpdates     int
	Title        string // custom title ("" = never renamed)
	DisplayTitle string
	MaxTS        int64
	QuotationIDs []int64
}

func (c Campaign) Time() time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", c.Data, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (s *Service) ListingRows(ctx context.Context) ([]ListingRow, error) {
	rows, err := s.db().QueryContext(ctx, `
		SELECT l.msg_id, COALESCE(l.timestamp,0), COALESCE(l.created_at,''), COALESCE(l.chat_id,''),
		       COALESCE(l.author_name,''), COALESCE(l.body,''), l.quotation_id, COALESCE(l.title,''),
		       COUNT(r.id)
		FROM listings l
		LEFT JOIN replies r ON r.listing_msg_id = l.msg_id
		GROUP BY l.id ORDER BY l.timestamp ASC, l.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListingRow
	for rows.Next() {
		var r ListingRow
		var qid sql.NullInt64
		if err := rows.Scan(&r.MsgID, &r.TS, &r.Data, &r.ChatID, &r.Venditore, &r.Testo, &qid, &r.Title, &r.Risposte); err != nil {
			return nil, err
		}
		if qid.Valid {
			v := qid.Int64
			r.QuotationID = &v
		}
		r.Campaign = textutil.CampaignKey(r.Testo)
		out = append(out, r)
	}
	return out, rows.Err()
}

// FilterRows keeps rows whose campaign, body or title contains q (case-insensitive).
func FilterRows(rows []ListingRow, q string) []ListingRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return rows
	}
	var out []ListingRow
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Campaign), q) || strings.Contains(strings.ToLower(r.Testo), q) ||
			strings.Contains(strings.ToLower(r.Title), q) {
			out = append(out, r)
		}
	}
	return out
}

// GroupCampaigns groups rows (chronological) by (chat, campaign key), most
// recently active campaign first.
func GroupCampaigns(rows []ListingRow) []Campaign {
	idx := map[[2]string]int{}
	var out []Campaign
	for _, r := range rows {
		k := [2]string{r.ChatID, r.Campaign}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, Campaign{ChatID: r.ChatID, Key: r.Campaign, Data: r.Data, Venditore: r.Venditore})
		}
		c := &out[i]
		c.MsgIDs = append(c.MsgIDs, r.MsgID)
		c.Testo = r.Testo
		c.Risposte += r.Risposte
		c.NUpdates++
		if c.Title == "" && strings.TrimSpace(r.Title) != "" {
			c.Title = r.Title
		}
		if r.TS > c.MaxTS {
			c.MaxTS = r.TS
		}
		if r.Data > c.LastData {
			c.LastData = r.Data
		}
		if r.QuotationID != nil && !containsID(c.QuotationIDs, *r.QuotationID) {
			c.QuotationIDs = append(c.QuotationIDs, *r.QuotationID)
		}
	}
	for i := range out {
		out[i].DisplayTitle = textutil.StripEmoji(out[i].Title)
		if out[i].DisplayTitle == "" {
			out[i].DisplayTitle = textutil.CleanTitle(textutil.StripEmoji(out[i].Key))
		}
		sort.Slice(out[i].QuotationIDs, func(a, b int) bool { return out[i].QuotationIDs[a] < out[i].QuotationIDs[b] })
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MaxTS > out[j].MaxTS })
	return out
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// ── Inserzioni list ──────────────────────────────────────────────────────────

// CampaignRow is a campaign with its group name, for the Inserzioni table.
type CampaignRow struct {
	Campaign
	ChatName string
	Orphan   bool // chat not in CHANNEL_ID
}

// InserzioniList returns every campaign (most recently active first),
// filtered by search and, when chatID is set, by group.
func (s *Service) InserzioniList(ctx context.Context, search, chatID string) ([]CampaignRow, error) {
	rows, err := s.ListingRows(ctx)
	if err != nil {
		return nil, err
	}
	rows = FilterRows(rows, search)
	names := s.GroupNames(ctx)
	known := map[string]bool{}
	for _, id := range s.Cfg.ChannelIDs {
		known[id] = true
	}
	var out []CampaignRow
	for _, c := range GroupCampaigns(rows) {
		if chatID != "" && c.ChatID != chatID {
			continue
		}
		name := names[c.ChatID]
		if name == "" {
			name = c.ChatID
		}
		out = append(out, CampaignRow{Campaign: c, ChatName: name, Orphan: !known[c.ChatID]})
	}
	return out, nil
}

// FindCampaign returns the campaign with this chat + key (unfiltered).
func (s *Service) FindCampaign(ctx context.Context, chatID, key string) (*Campaign, error) {
	rows, err := s.ListingRows(ctx)
	if err != nil {
		return nil, err
	}
	var chatRows []ListingRow
	for _, r := range rows {
		if r.ChatID == chatID {
			chatRows = append(chatRows, r)
		}
	}
	for _, c := range GroupCampaigns(chatRows) {
		if c.Key == key {
			c := c
			return &c, nil
		}
	}
	return nil, nil
}

type ReplyView struct {
	ID     int64
	Utente string
	Testo  string
	Ora    string
}

func (s *Service) CampaignReplies(ctx context.Context, msgIDs []string) ([]ReplyView, error) {
	if len(msgIDs) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(msgIDs)), ",")
	args := make([]any, len(msgIDs))
	for i, id := range msgIDs {
		args[i] = id
	}
	rows, err := s.db().QueryContext(ctx, `
		SELECT r.id, `+userNameSQL+`, COALESCE(r.body,''), COALESCE(r.timestamp,0)
		FROM replies r LEFT JOIN users u ON u.id = r.author_id
		WHERE r.listing_msg_id IN (`+ph+`)
		ORDER BY r.timestamp, r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReplyView
	for rows.Next() {
		var r ReplyView
		var name sql.NullString
		var ts int64
		if err := rows.Scan(&r.ID, &name, &r.Testo, &ts); err != nil {
			return nil, err
		}
		r.Utente = FmtName(nullStr(name))
		r.Ora = FmtTS(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) RenameCampaign(ctx context.Context, msgIDs []string, title string) error {
	if len(msgIDs) == 0 {
		return nil
	}
	var t any
	if title = strings.TrimSpace(title); title != "" {
		t = title
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(msgIDs)), ",")
	args := []any{t}
	for _, id := range msgIDs {
		args = append(args, id)
	}
	_, err := s.db().ExecContext(ctx, "UPDATE listings SET title = ? WHERE msg_id IN ("+ph+")", args...)
	return err
}

func (s *Service) DeleteReply(ctx context.Context, id int64) error {
	_, err := s.db().ExecContext(ctx, "DELETE FROM replies WHERE id = ?", id)
	return err
}
