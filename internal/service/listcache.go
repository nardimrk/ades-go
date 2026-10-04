package service

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"adesgo/internal/db"
)

// versioned keeps a value computed from the DB until the app_meta counter
// it depends on changes (the counters are bumped by triggers, whoever
// writes: the dashboard, the collector, the sqlite3 shell).
type versioned[T any] struct {
	mu  sync.Mutex
	ver string
	ok  bool
	val T
}

func (c *versioned[T]) get(ctx context.Context, store *db.Store, load func(context.Context) (T, error), keys ...string) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// read the versions before loading: a write that lands meanwhile bumps
	// one again, so the next call reloads
	var ver string
	var verErr error
	for _, k := range keys {
		v, err := store.Version(ctx, k)
		if err != nil {
			verErr = err
		}
		ver += v + "|"
	}
	if verErr == nil && c.ok && ver == c.ver {
		return c.val, nil
	}
	v, err := load(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	c.ver, c.ok, c.val = ver, verErr == nil, v
	return v, nil
}

// UsersCached is Users, cached until a customer or a reply changes. The
// slice is shared: callers must not modify it.
func (s *Service) UsersCached(ctx context.Context) ([]User, error) {
	return s.usersCache.get(ctx, s.Store, s.Users, db.ClientiVersionKey)
}

// ItemsCached is Items, cached until a product changes. The slice is
// shared: callers must not modify it.
func (s *Service) ItemsCached(ctx context.Context) ([]Item, error) {
	return s.itemsCache.get(ctx, s.Store, s.Items, db.ProdottiVersionKey)
}

// ListPerPage: rows per page in Clienti and Prodotti.
const ListPerPage = 25

// Page: one page of a filtered list.
type Page struct {
	Num, Pages int // 1-based page, number of pages (at least 1)
	Total      int // rows matching the filters
	From, To   int // the rows shown, 1-based ("26–50"); 0 when none
}

// Paginate returns page num (clamped) of total rows, per per page, and the
// slice bounds of its rows.
func Paginate(total, num, per int) (p Page, lo, hi int) {
	p.Total = total
	p.Pages = (total + per - 1) / per
	if p.Pages < 1 {
		p.Pages = 1
	}
	p.Num = min(max(num, 1), p.Pages)
	lo = (p.Num - 1) * per
	hi = min(lo+per, total)
	if lo < hi {
		p.From, p.To = lo+1, hi
	}
	return p, lo, hi
}

// fold lowers text and drops every accent and extra spaces, so "citta"
// finds "Città" and "chateau" finds "Château".
func fold(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if r, _, err := transform.String(t, s); err == nil {
		s = r
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// ClientiFilters are the column filters of Clienti, by key.
var ClientiFilters = []struct{ Key, Title string }{
	{"code", "Codice / ID"}, {"nome", "Nome"}, {"tel", "Telefono"}, {"piva", "Partita IVA"},
	{"email", "Email"}, {"indirizzo", "Indirizzo"}, {"citta", "Città"}, {"provincia", "Provincia"},
	{"regione", "Regione"}, {"cap", "CAP"},
}

func userColumn(u User, key string) string {
	switch key {
	case "code":
		return u.Code + " " + u.ID + " " + strings.Join(u.Aliases, " ")
	case "nome":
		return u.Name
	case "tel":
		return u.Telefono
	case "piva":
		return u.PIVA
	case "email":
		return u.Email
	case "indirizzo":
		return u.Indirizzo
	case "citta":
		return u.Citta
	case "provincia":
		return u.Provincia
	case "regione":
		return u.Regione
	case "cap":
		return u.CAP
	}
	return ""
}

// FilterUsers keeps the customers matching the search q (name, id, city,
// phone, partita IVA, email) and every column filter (key → text).
func FilterUsers(users []User, q string, cols map[string]string) []User {
	q = fold(q)
	type colQ struct{ key, q string }
	var cq []colQ
	for k, v := range cols {
		if v = fold(v); v != "" {
			cq = append(cq, colQ{k, v})
		}
	}
	if q == "" && len(cq) == 0 {
		return users
	}
	var out []User
	for _, u := range users {
		if q != "" && !strings.Contains(fold(u.Name+" "+u.ID+" "+u.Code+" "+u.Citta+" "+u.Telefono+" "+u.PIVA+" "+u.Email), q) {
			continue
		}
		ok := true
		for _, c := range cq {
			if !strings.Contains(fold(userColumn(u, c.key)), c.q) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, u)
		}
	}
	return out
}

// FilterItems keeps the products whose code, ERP code, description,
// winery or area contain q.
func FilterItems(items []Item, q string) []Item {
	if q = fold(q); q == "" {
		return items
	}
	var out []Item
	for _, it := range items {
		if strings.Contains(fold(it.Code+" "+it.CodeAlias+" "+it.Description+" "+it.Winery+" "+it.Area), q) {
			out = append(out, it)
		}
	}
	return out
}

// DistinctCities: the customers' cities, once each (case and spaces
// ignored), in alphabetical order.
func DistinctCities(users []User) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range users {
		c := strings.Join(strings.Fields(u.Citta), " ")
		k := strings.ToLower(c)
		if c == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// YearGroup: the items of one year older than the recent months.
type YearGroup[T any] struct {
	Year  int // 0 = without a date
	Items []T
}

// RecentMonths: how many months (this one included) the timelines of
// Inserzioni and Ordini show open.
const RecentMonths = 6

// splitRecent splits months (newest first; ym gives each one's year and
// month) into the last RecentMonths before now, and the older ones by year,
// newest first, without a date last.
func splitRecent[T any](months []T, ym func(T) (int, int), now time.Time) (recent []T, years []YearGroup[T]) {
	cut := now.Year()*12 + int(now.Month()) - RecentMonths // months after this are recent
	idx := map[int]int{}
	for _, m := range months {
		y, mo := ym(m)
		if y > 0 && y*12+mo > cut {
			recent = append(recent, m)
			continue
		}
		i, ok := idx[y]
		if !ok {
			i = len(years)
			idx[y] = i
			years = append(years, YearGroup[T]{Year: y})
		}
		years[i].Items = append(years[i].Items, m)
	}
	sort.SliceStable(years, func(i, j int) bool {
		a, b := years[i].Year, years[j].Year
		if a == 0 || b == 0 {
			return b == 0 && a != 0
		}
		return a > b
	})
	return recent, years
}

// OrdiniYear: the months of one older year in Ordini, with its totals.
type OrdiniYear struct {
	Year      int
	Months    []QuotationMonth
	Count     int
	Bottiglie int
	Totale    float64
}

// SplitRecentOrders keeps the last RecentMonths of Ordini open and groups
// the older months by year.
func SplitRecentOrders(months []QuotationMonth, now time.Time) (recent []QuotationMonth, years []OrdiniYear) {
	recent, groups := splitRecent(months, func(m QuotationMonth) (int, int) { return m.Year, m.Month }, now)
	for _, g := range groups {
		y := OrdiniYear{Year: g.Year, Months: g.Items}
		for _, m := range g.Items {
			y.Count += len(m.Rows)
			y.Bottiglie += m.Bottiglie
			y.Totale += m.Totale
		}
		years = append(years, y)
	}
	return recent, years
}

// QuotationSummaries is the Ordini list, cached until an order, a reply, a
// listing or a confirmation changes. The slice is a copy.
func (s *Service) QuotationSummaries(ctx context.Context) ([]QuotationSummary, error) {
	rows, err := s.summariesCache.get(ctx, s.Store, s.loadQuotationSummaries,
		db.SelectionsVersionKey, db.ListingsVersionKey, db.OrdiniVersionKey)
	if err != nil {
		return nil, err
	}
	return append([]QuotationSummary(nil), rows...), nil
}
