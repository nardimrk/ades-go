package service

import (
	"adesgo/internal/textutil"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// ConsegneCampaign is a campaign selectable on the Consegne page (only
// campaigns linked to a quotation), with its delivery totals.
type ConsegneCampaign struct {
	ID           string // stable short id (hash of chat + key) for forms/URLs
	ChatID       string
	ChatName     string
	Key          string
	Title        string
	Published    string // first post "YYYY-MM-DD HH:MM:SS"
	Data         string // latest post
	Label        string
	URL          string
	QuotationIDs []int64

	Clienti   int
	Bottiglie int // bottles ordered (a case counts its bottles when its size is known)
	Totale    float64

	customers map[string]bool
}

// Day is the day of the month of publication ("30").
func (c ConsegneCampaign) Day() string {
	if len(c.Published) >= 10 {
		return c.Published[8:10]
	}
	return ""
}

// ConsegneMonth groups the campaigns published in one month.
type ConsegneMonth struct {
	Key       string // "2026-09"
	Year      int
	Month     int
	Campaigns []ConsegneCampaign
	Clienti   int // distinct customers across the month
	Bottiglie int
	Totale    float64
}

func campaignID(chatID, key string) string {
	h := sha1.Sum([]byte(chatID + "\x00" + key))
	return hex.EncodeToString(h[:6])
}

// CampaignURL links to the campaign on the Inserzioni page.
func CampaignURL(chatID, key string) string {
	return "/inserzioni?" + url.Values{"chat": {chatID}, "c": {key}}.Encode()
}

// ConsegneCampaigns returns the campaigns with a quotation, newest
// publication first, each with customers / units / total from the replies.
func (s *Service) ConsegneCampaigns(ctx context.Context) ([]ConsegneCampaign, error) {
	rows, err := s.ListingRows(ctx)
	if err != nil {
		return nil, err
	}
	sel, err := s.ComputeSelections(ctx)
	if err != nil {
		return nil, err
	}
	byQuote := map[int64][]Selection{}
	for _, x := range sel {
		byQuote[x.QuotationID] = append(byQuote[x.QuotationID], x)
	}
	names := s.GroupNames(ctx)
	camps := GroupCampaigns(rows)
	out := make([]ConsegneCampaign, 0, len(camps))
	for _, c := range camps {
		if len(c.QuotationIDs) == 0 {
			continue // no quotation, so nothing to deliver
		}
		chatLabel := names[c.ChatID]
		if chatLabel == "" {
			chatLabel = c.ChatID
		}
		cc := ConsegneCampaign{
			ID: campaignID(c.ChatID, c.Key), ChatID: c.ChatID, ChatName: chatLabel, Key: c.Key,
			Title: c.DisplayTitle, Published: c.Data, Data: c.LastData, URL: CampaignURL(c.ChatID, c.Key),
			QuotationIDs: c.QuotationIDs,
		}
		cc.Label = cc.Title + " " + chatLabel
		clienti := map[string]bool{}
		for _, q := range c.QuotationIDs {
			for _, x := range byQuote[q] {
				clienti[x.Utente] = true
				cc.Bottiglie += x.Bottles()
				cc.Totale += float64(x.Qta) * x.Prezzo
			}
		}
		cc.Clienti, cc.Totale, cc.customers = len(clienti), round2(cc.Totale), clienti
		out = append(out, cc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Published > out[j].Published })
	return out, nil
}

// ConsegneMonths groups the campaigns by month of publication, keeping the
// order of camps (newest first).
func ConsegneMonths(camps []ConsegneCampaign) []ConsegneMonth {
	var months []ConsegneMonth
	idx := map[string]int{}
	customers := map[string]map[string]bool{}
	for _, c := range camps {
		key := ""
		if len(c.Published) >= 7 {
			key = c.Published[:7]
		}
		i, ok := idx[key]
		if !ok {
			i = len(months)
			idx[key] = i
			m := ConsegneMonth{Key: key}
			if t, err := time.Parse("2006-01", key); err == nil {
				m.Year, m.Month = t.Year(), int(t.Month())
			}
			months = append(months, m)
			customers[key] = map[string]bool{}
		}
		m := &months[i]
		m.Campaigns = append(m.Campaigns, c)
		m.Bottiglie += c.Bottiglie
		m.Totale = round2(m.Totale + c.Totale)
		for u := range c.customers {
			customers[key][u] = true
		}
	}
	for i := range months {
		months[i].Clienti = len(customers[months[i].Key])
	}
	return months
}

type DeliveryLine struct {
	Cliente    string
	Inserzione string
	URL        string
	Vino       string
	Qta        int
	Prezzo     float64
	Provincia  string
	Citta      string
}

func (l DeliveryLine) Totale() float64 { return round2(float64(l.Qta) * l.Prezzo) }

// Bottles is the number of bottles of the line: Qta, times the case size for
// a case of known size ("(cassa da 12…)").
func (l DeliveryLine) Bottles() int { return bottles(l.Vino, l.Qta) }

func bottles(wine string, qty int) int {
	if n := textutil.CaseSizeFromName(wine); n > 0 {
		return qty * n
	}
	return qty
}

type CustomerDelivery struct {
	Cliente   string
	Provincia string // from Clienti, "" = not set
	Citta     string
	Bottiglie int
	Totale    float64
	Lines     []DeliveryLine
}

// DeliveryArea: the customers of one provincia, by city.
type DeliveryArea struct {
	Provincia string // "" = not set in Clienti
	Cities    []DeliveryCity
	Clienti   int
	Bottiglie int
	Totale    float64
}

type DeliveryCity struct {
	Citta     string // "" = not set in Clienti
	Customers []CustomerDelivery
	Bottiglie int
	Totale    float64
}

type Consegne struct {
	Selected   []ConsegneCampaign
	NoQuote    bool               // selection has no linked quotation
	Customers  []CustomerDelivery // ordered by provincia, città, name
	Areas      []DeliveryArea
	Clienti    int
	Bottiglie  int
	Totale     float64
	ExportRows []DeliveryLine
}

// Consegne computes the deliveries for the selected campaign ids.
func (s *Service) Consegne(ctx context.Context, ids []string) (*Consegne, error) {
	all, err := s.ConsegneCampaigns(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	res := &Consegne{}
	byQID := map[int64]ConsegneCampaign{}
	for _, c := range all {
		if want[c.ID] {
			res.Selected = append(res.Selected, c)
			for _, q := range c.QuotationIDs {
				byQID[q] = c
			}
		}
	}
	if len(res.Selected) == 0 {
		return res, nil
	}
	if len(byQID) == 0 {
		res.NoQuote = true
		return res, nil
	}
	sel, err := s.ComputeSelections(ctx)
	if err != nil {
		return nil, err
	}
	locs, err := s.customerLocations(ctx)
	if err != nil {
		return nil, err
	}
	var lines []DeliveryLine
	clienti := map[string]location{}
	for _, x := range sel {
		c, ok := byQID[x.QuotationID]
		if !ok {
			continue
		}
		loc, ok := locs.byID[x.AuthorID]
		if !ok {
			loc = locs.byName[x.Utente]
		}
		l := DeliveryLine{Cliente: x.Utente, Inserzione: c.Title, URL: CampaignURL(c.ChatID, c.Key), Vino: wineWithVintage(x.Vino, x.Vintage), Qta: x.Qta, Prezzo: x.Prezzo,
			Provincia: loc.provincia, Citta: loc.citta}
		lines = append(lines, l)
		if _, seen := clienti[l.Cliente]; !seen {
			clienti[l.Cliente] = loc
		}
		res.Bottiglie += l.Bottles()
		res.Totale += l.Totale()
	}
	res.Clienti = len(clienti)

	// deliveries are planned by area: provincia, then città, then customer
	// (a customer is placed where their first line says)
	for i := range lines {
		loc := clienti[lines[i].Cliente]
		lines[i].Provincia, lines[i].Citta = loc.provincia, loc.citta
	}
	res.ExportRows = append([]DeliveryLine(nil), lines...)
	sort.SliceStable(res.ExportRows, func(i, j int) bool {
		a, b := res.ExportRows[i], res.ExportRows[j]
		if c := compareArea(a.Provincia, a.Citta, b.Provincia, b.Citta); c != 0 {
			return c < 0
		}
		if a.Cliente != b.Cliente {
			return a.Cliente < b.Cliente
		}
		return a.Inserzione < b.Inserzione
	})

	names := make([]string, 0, len(clienti))
	for n := range clienti {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := clienti[names[i]], clienti[names[j]]
		if c := compareArea(a.provincia, a.citta, b.provincia, b.citta); c != 0 {
			return c < 0
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		loc := clienti[n]
		cd := CustomerDelivery{Cliente: n, Provincia: loc.provincia, Citta: loc.citta}
		for _, l := range lines {
			if l.Cliente == n {
				cd.Lines = append(cd.Lines, l)
				cd.Bottiglie += l.Bottles()
				cd.Totale += l.Totale()
			}
		}
		res.Customers = append(res.Customers, cd)
	}
	res.Areas = groupByArea(res.Customers)
	return res, nil
}

type location struct{ provincia, citta string }

type customerLocs struct {
	byID   map[string]location
	byName map[string]location // display name (FmtName), for orders without a user id
}

// customerLocations reads provincia and città of every customer (Clienti).
func (s *Service) customerLocations(ctx context.Context) (customerLocs, error) {
	out := customerLocs{byID: map[string]location{}, byName: map[string]location{}}
	rows, err := s.db().QueryContext(ctx, `SELECT id, COALESCE(name,''), COALESCE(provincia,''), COALESCE("città",'') FROM users`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var loc location
		if err := rows.Scan(&id, &name, &loc.provincia, &loc.citta); err != nil {
			return out, err
		}
		loc.provincia, loc.citta = cleanPlace(loc.provincia), cleanPlace(loc.citta)
		out.byID[id] = loc
		if n := FmtName(name); n != "" {
			if _, dup := out.byName[n]; !dup {
				out.byName[n] = loc
			}
		}
	}
	return out, rows.Err()
}

// cleanPlace trims a place name and collapses inner spaces.
func cleanPlace(s string) string { return strings.Join(strings.Fields(s), " ") }

// placeKey compares place names ignoring case and accents.
var placeFold = strings.NewReplacer("à", "a", "á", "a", "è", "e", "é", "e", "ì", "i", "í", "i", "ò", "o", "ó", "o", "ù", "u", "ú", "u")

func placeKey(s string) string { return placeFold.Replace(strings.ToLower(s)) }

// comparePlace orders place names alphabetically, the unknown ("") last.
func comparePlace(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	return strings.Compare(placeKey(a), placeKey(b))
}

func compareArea(provA, cityA, provB, cityB string) int {
	if c := comparePlace(provA, provB); c != 0 {
		return c
	}
	return comparePlace(cityA, cityB)
}

// groupByArea splits customers already ordered by area into provincia →
// città groups. Names differing only in case or accents share a group.
func groupByArea(customers []CustomerDelivery) []DeliveryArea {
	var areas []DeliveryArea
	for _, cd := range customers {
		if n := len(areas); n == 0 || placeKey(areas[n-1].Provincia) != placeKey(cd.Provincia) {
			areas = append(areas, DeliveryArea{Provincia: cd.Provincia})
		}
		a := &areas[len(areas)-1]
		if n := len(a.Cities); n == 0 || placeKey(a.Cities[n-1].Citta) != placeKey(cd.Citta) {
			a.Cities = append(a.Cities, DeliveryCity{Citta: cd.Citta})
		}
		c := &a.Cities[len(a.Cities)-1]
		c.Customers = append(c.Customers, cd)
		c.Bottiglie += cd.Bottiglie
		c.Totale += cd.Totale
		a.Clienti++
		a.Bottiglie += cd.Bottiglie
		a.Totale += cd.Totale
	}
	return areas
}

// wineWithVintage adds the vintage to the wine name unless the name already
// has it: "Château Coutet" + 2016 → "Château Coutet 2016". A trailing
// "(cassa …)" stays at the end.
func wineWithVintage(name string, vintage int) string {
	y := strconv.Itoa(vintage)
	if vintage == 0 || strings.Contains(name, y) {
		return name
	}
	if i := strings.LastIndex(name, " ("); i > 0 {
		return name[:i] + " " + y + name[i:]
	}
	return name + " " + y
}

// ConsegneExcel renders the export rows as an .xlsx file.
func ConsegneExcel(rows []DeliveryLine) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	const sheet = "Consegne"
	f.SetSheetName("Sheet1", sheet)
	header := []any{"Provincia", "Città", "Cliente", "Inserzione", "Vino", "Qtà", "Prezzo (€)", "Totale (€)"}
	if err := f.SetSheetRow(sheet, "A1", &header); err != nil {
		return nil, err
	}
	for i, r := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+2)
		row := []any{r.Provincia, r.Citta, r.Cliente, r.Inserzione, r.Vino, r.Qta, r.Prezzo, r.Totale()}
		if err := f.SetSheetRow(sheet, cell, &row); err != nil {
			return nil, err
		}
	}
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	f.SetRowStyle(sheet, 1, 1, bold)
	f.SetColWidth(sheet, "A", "B", 18)
	f.SetColWidth(sheet, "C", "E", 30)
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
