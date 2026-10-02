package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Order notifications: the bell in the menu counts the customer replies that
// became an order since the user last opened its preview. "Seen" is kept per
// login email in app_meta as the highest reply id already shown.

// noticeMaxAge: replies older than this never notify, so a chat import that
// adds old messages (new ids, old timestamps) doesn't flood the bell.
const noticeMaxAge = 14 * 24 * time.Hour

// noticeLimit: the preview shows at most this many orders (newest first).
const noticeLimit = 30

// OrderNotice is one reply that was read as an order.
type OrderNotice struct {
	ReplyID    int64
	Utente     string
	Preventivo string
	TS         int64
	Items      []NoticeItem
}

type NoticeItem struct {
	Opzione string
	Vino    string
	Vintage int // 0 when unknown
	Qta     int
	Prezzo  float64
}

func (n OrderNotice) Total() float64 {
	var t float64
	for _, it := range n.Items {
		t += float64(it.Qta) * it.Prezzo
	}
	return round2(t)
}

// OrderNotices is the unseen part of the bell for one user.
type OrderNotices struct {
	Items []OrderNotice // newest first, at most noticeLimit
	Count int           // all unseen orders (may exceed len(Items))
	MaxID int64         // highest reply id among them: "seen" moves here
}

func seenKey(email string) string { return "notif_seen:" + strings.ToLower(email) }

// lastSeenReply returns the highest reply id the user has seen. On a user's
// first visit it starts from the current newest reply, so the bell starts at
// zero instead of counting the whole history.
func (s *Service) lastSeenReply(ctx context.Context, email string) (int64, error) {
	if v := s.Store.Meta(ctx, seenKey(email)); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			return id, nil
		}
	}
	var max int64
	if err := s.db().QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM replies").Scan(&max); err != nil {
		return 0, err
	}
	return max, s.Store.SetMeta(ctx, seenKey(email), strconv.FormatInt(max, 10))
}

// UnseenOrders lists the orders received since the user last opened the bell.
func (s *Service) UnseenOrders(ctx context.Context, email string) (OrderNotices, error) {
	var out OrderNotices
	last, err := s.lastSeenReply(ctx, email)
	if err != nil {
		return out, err
	}
	rows, err := s.db().QueryContext(ctx, `SELECT id, COALESCE(timestamp,0) FROM replies
		WHERE id > ? AND COALESCE(timestamp,0) >= ? AND COALESCE(msg_id,'') NOT LIKE ?`,
		last, time.Now().Add(-noticeMaxAge).Unix(), ManualReplyPrefix+"%")
	if err != nil {
		return out, err
	}
	ts := map[int64]int64{}
	for rows.Next() {
		var id, t int64
		if err := rows.Scan(&id, &t); err != nil {
			rows.Close()
			return out, err
		}
		ts[id] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(ts) == 0 {
		return out, nil
	}

	// every option code read from the reply, before later replies replace it:
	// a correction ("no, 2A") is an order received too
	sel, err := s.parseSelections(ctx)
	if err != nil {
		return out, err
	}
	by := map[int64]*OrderNotice{}
	for _, x := range sel {
		t, ok := ts[x.ReplyID]
		if !ok {
			continue
		}
		n := by[x.ReplyID]
		if n == nil {
			n = &OrderNotice{ReplyID: x.ReplyID, Utente: x.Utente, Preventivo: x.Preventivo, TS: t}
			by[x.ReplyID] = n
		}
		n.Items = append(n.Items, NoticeItem{Opzione: x.Opzione, Vino: x.Vino, Vintage: x.Vintage, Qta: x.Qta, Prezzo: x.Prezzo})
	}
	all := make([]OrderNotice, 0, len(by))
	for _, n := range by {
		all = append(all, *n)
		if n.ReplyID > out.MaxID {
			out.MaxID = n.ReplyID
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].TS != all[j].TS {
			return all[i].TS > all[j].TS
		}
		return all[i].ReplyID > all[j].ReplyID
	})
	out.Count = len(all)
	if len(all) > noticeLimit {
		all = all[:noticeLimit]
	}
	out.Items = all
	return out, nil
}

// MarkOrdersSeen moves the user's "seen" mark up to reply id upTo (never back).
func (s *Service) MarkOrdersSeen(ctx context.Context, email string, upTo int64) error {
	last, err := s.lastSeenReply(ctx, email)
	if err != nil || upTo <= last {
		return err
	}
	return s.Store.SetMeta(ctx, seenKey(email), strconv.FormatInt(upTo, 10))
}

// RelTime: a short Italian relative time, like the iOS notification list.
func RelTime(ts int64, now time.Time) string {
	t := time.Unix(ts, 0)
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "adesso"
	case d < time.Hour:
		return fmt.Sprintf("%d min fa", int(d.Minutes()))
	case d < 6*time.Hour:
		return fmt.Sprintf("%d h fa", int(d.Hours()))
	}
	y, m, dd := now.Date()
	today := time.Date(y, m, dd, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return t.Format("15:04")
	case !t.Before(today.AddDate(0, 0, -1)):
		return "ieri " + t.Format("15:04")
	}
	return t.Format("02/01 15:04")
}
