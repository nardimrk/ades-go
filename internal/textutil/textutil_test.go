package textutil

import (
	"fmt"
	"reflect"
	"testing"
)

// Expected values come from running the Python originals (difflib, re).
func TestRatio(t *testing.T) {
	cases := []struct {
		a, b string
		want string
	}{
		{"abcd", "bcde", "0.7500"},
		{"", "", "1.0000"},
		{"private Thread currentThread;", "private volatile Thread currentThread;", "0.8657"},
		{"Champagne Brut", "Champagne Rosé", "0.7143"},
	}
	for _, c := range cases {
		if got := fmt.Sprintf("%.4f", Ratio(c.a, c.b)); got != c.want {
			t.Errorf("Ratio(%q, %q) = %s, want %s", c.a, c.b, got, c.want)
		}
	}
}

func TestParseOrderCodes(t *testing.T) {
	cases := map[string][]OrderCode{
		"3A, 2b":          {{Qty: 3, Option: "A"}, {Qty: 2, Option: "B"}},
		"2 x C grazie":    {{Qty: 2, Option: "C"}},
		"6 A \n 6 C":      {{Qty: 6, Option: "A"}, {Qty: 6, Option: "C"}},
		"1-d":             {{Qty: 1, Option: "D"}},
		"ciao 2Andrea":    nil, // letter followed by a word char
		"a2A":             nil, // digits preceded by a word char
		"2K":              nil, // K is not an option letter
		"vintage 2019 3A": {{Qty: 3, Option: "A"}},
	}
	for in, want := range cases {
		if got := ParseOrderCodes(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseOrderCodes(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStripQuantityAndPrice(t *testing.T) {
	cases := map[string]string{
		"Barolo 2019 6 bottiglie 45,50€": "Barolo 2019 bottiglie €",
		"12x Chablis 2020":               "x Chablis 2020",
		"Magnum 1500":                    "Magnum",
		"Annata 20190":                   "Annata",
	}
	for in, want := range cases {
		if got := StripQuantityAndPrice(in); got != want {
			t.Errorf("StripQuantityAndPrice(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCampaignKey(t *testing.T) {
	body := "Buongiorno a tutti!\nDomaine Leflaive disponibili 12 bottiglie\nA. 6x Puligny 2020 a 120€\nB. Venduto."
	if got := CampaignKey(body); got != "Domaine Leflaive" {
		t.Errorf("CampaignKey = %q", got)
	}
	opts := "A. Venduto.\nB. 3x Chablis Grand Cru 2019 a 80€"
	if got := CampaignKey(opts); got != "Chablis Grand Cru 2019 a €" {
		t.Errorf("CampaignKey(options only) = %q", got)
	} // a repost that adds the bottle count stays in the same campaign
	first := "Disponibili:\n\nA. Champagne Theophile a 33€\n\nB. Cassa intera Champagne Theophile a 180€"
	repost := "Disponibili:\n\nA. 174 x Champagne Theophile a 33€\n\nB. Cassa intera Champagne Theophile a 180€"
	if a, b := CampaignKey(first), CampaignKey(repost); a != b {
		t.Errorf("CampaignKey(first) = %q, CampaignKey(repost) = %q", a, b)
	}
}

func TestParseOptions(t *testing.T) {
	body := "Domaine X\nA. 6x Pinot Noir 2020 a 25€\nB. Venduto\nC. Cassa intera a 300€\nD. 3 x Chardonnay\n2021 a 30,50€"
	want := []Option{{Letter: "A", WineName: "Pinot Noir", Quantity: 6, Price: 25}, {Letter: "D", WineName: "Chardonnay", Quantity: 3, Price: 30.5}}
	if got := ParseOptions(body); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseOptions = %+v, want %+v", got, want)
	}
}

func TestParseWhatsAppExport(t *testing.T) {
	text := "‎[01/09/26, 10:00:00] Ale: Riga uno\nriga due\n[01/09/26, 10:01] Messaggi crittografati.\n" +
		"[01/09/26, 10:02:00] Bob: Questo messaggio è stato eliminato\n[1/9/2026, 9:03 PM] Bob: 2A"
	msgs := ParseWhatsAppExport(text)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages: %+v", len(msgs), msgs)
	}
	if msgs[0].Author != "Ale" || msgs[0].Body != "Riga uno\nriga due" {
		t.Errorf("first = %+v", msgs[0])
	}
	if msgs[1].DT == nil || msgs[1].DT.Hour() != 21 || msgs[1].Body != "2A" {
		t.Errorf("second = %+v", msgs[1])
	}
	if got := fixMojibake("Ã¨ 25â\u0082¬"); got != "è 25€" {
		t.Errorf("fixMojibake = %q", got)
	}
}

func TestCleanTitle(t *testing.T) {
	cases := map[string]string{
		"x Ducru-Beaucaillou 2025 a €.":                              "Ducru-Beaucaillou 2025",
		"x Leoville Poyferré 2025 a €":                               "Leoville Poyferré 2025",
		"x Château Coutet 2023 a € + iva":                            "Château Coutet 2023",
		"Champagne Pierre Baillette":                                 "Champagne Pierre Baillette",
		"x Mezze bottiglie di Les Griffons de Pichon Baron 2018 a €": "Mezze bottiglie di Les Griffons de Pichon Baron 2018",
		"Preciso che le bottiglie sono da ml.":                       "Preciso che le bottiglie sono da ml",
	}
	for in, want := range cases {
		if got := CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
	long := "La proprietà di Pierre Baillette sorge nel comune di Trois Puits, nella Montagna di Reims. Il Domaine conta appezzamenti"
	if got := CleanTitle(long); len([]rune(got)) > MaxTitleLen+1 || got[len(got)-3:] != "…" {
		t.Errorf("long title not shortened: %q", got)
	}
}

func TestParseOptionsWithoutQuantity(t *testing.T) {
	body := "Thanisch Riesling Secchi\nDisponibili:\n\nA. Schieferreich Riesling 2025 a 14,50€\n(CASSA INTERA a 78€)\n\n" +
		"B. Lieser Niederberg Helden Riesling Alte Reben -R- 2024 a 19,50€\n(CASSA INTERA a 108€)\n\nD. Mixed Case: 2 x A-B-C a 110€"
	want := []Option{
		{Letter: "A", WineName: "Schieferreich Riesling", Price: 14.5},
		{Letter: "A", WineName: "Schieferreich Riesling (cassa intera)", Price: 78, Case: true},
		{Letter: "B", WineName: "Lieser Niederberg Helden Riesling Alte Reben -R-", Price: 19.5},
		{Letter: "B", WineName: "Lieser Niederberg Helden Riesling Alte Reben -R- (cassa intera)", Price: 108, Case: true},
		{Letter: "D", WineName: "Mixed Case: 2 x A-B-C", Price: 110},
	}
	if got := ParseOptions(body); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseOptions =\n%+v\nwant\n%+v", got, want)
	}
}

func TestCaseOptions(t *testing.T) {
	body := "Château Coutet \n\nDemi Bouteilles Disponibili:\n\nA. 21 x Château Coutet 2016 a 27,50€\n\n" +
		"B. 9 x Château Coutet 2019 a 23,50€\n\nCassa intera (12 x 375ml) sconto 15%."
	if size, disc, ok := CaseOffer(body); !ok || size != 12 || disc != 15 {
		t.Fatalf("CaseOffer = %d, %v, %v", size, disc, ok)
	}
	want := []Option{
		{Letter: "A", WineName: "Château Coutet", Quantity: 21, Price: 27.5},
		{Letter: "B", WineName: "Château Coutet", Quantity: 9, Price: 23.5},
		{Letter: "A", WineName: "Château Coutet (cassa da 12, sconto 15%)", Price: 280.5, Case: true},
		{Letter: "B", WineName: "Château Coutet (cassa da 12, sconto 15%)", Price: 239.7, Case: true},
	}
	if got := WithCaseOptions(body, ParseOptions(body)); !reflect.DeepEqual(got, want) {
		t.Errorf("WithCaseOptions =\n%+v\nwant\n%+v", got, want)
	}
	// no discount, or case options already in the listing: unchanged
	for _, b := range []string{"A. 6 x Riesling 2020 a 20€\n\nCassa intera (6 x 750ml).", "È possibile condividere la cassa.\nA. 6 x Riesling 2020 a 20€"} {
		if got := WithCaseOptions(b, ParseOptions(b)); len(got) != 1 {
			t.Errorf("WithCaseOptions(%q) = %+v", b, got)
		}
	}
	if n := CaseSizeFromName("Roederer Brut Rose (cassa da 6)"); n != 6 {
		t.Errorf("CaseSizeFromName = %d", n)
	}
	if got := FormatOrderCodes(ParseOrders("3A, 1 cassa B")); got != "3A, 1 cassa B" {
		t.Errorf("FormatOrderCodes round trip = %q", got)
	}
}

func TestParseOrders(t *testing.T) {
	cases := map[string][]string{
		"1 D":                           {"1D"},
		"1 cassa A":                     {"1A-CASSA"},
		"3 casse A\n3 casse B":          {"3A-CASSA", "3B-CASSA"},
		"1cassa A,\n1cassa B\n1cassa C": {"1A-CASSA", "1B-CASSA", "1C-CASSA"},
		"Aggiungo una cassa A grazie!":  {"1A-CASSA"},
		"A 6":                           {"6A"},
		"6-A\n6-B\nMate 🤙":              {"6A", "6B"},
		"1 cassa Amarone":               {"1-CASSA"},
		"1 cassa grazie":                {"1-CASSA"},
		"Una cassa Roederer Brut Rose’ 2017. Grazie": {"1-CASSA"},
		"2 casse":        {"2-CASSA"},
		"Di nulla":       nil,
		"Cassa intera A": {"1A-CASSA"},
		"Ciao Alessandro, una cassa di B per piacere":         {"1B-CASSA"},
		"una cassa intera di B":                               {"1B-CASSA"},
		"12x375ml B":                                          {"12B"},
		"6 x 75cl di A e 2B":                                  {"2B", "6A"},
		"Tutte le bottiglie disponibili sono. state vendute.": nil,
	}
	for in, want := range cases {
		var got []string
		for _, c := range ParseOrders(in) {
			got = append(got, fmt.Sprintf("%d%s", c.Qty, c.Key()))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseOrders(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStripEmoji(t *testing.T) {
	cases := map[string]string{
		"Luca Ferronato ☀️☀️☀️": "Luca Ferronato",
		"Mate 🤙🙌":               "Mate",
		"👨‍👩‍👧 Famiglia Rossi":  "Famiglia Rossi",
		"Château Coutet":        "Château Coutet",
		"€ 25 · 3A":             "€ 25 · 3A",
	}
	for in, want := range cases {
		if got := StripEmoji(in); got != want {
			t.Errorf("StripEmoji(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsMediaBlob(t *testing.T) {
	jpeg := "/9j/4AAQSkZJRgABAQAAAQABAAD/2wCEAAQFBQkGCQkJCQkKCAkICgsLCgoLCwwKCwoLCgwMDAwNDQwMDAwMDw4PDAwNDw8PDw0OERERDhEQEBETERMREQ0BBAQECAYIBwgIBwgG"
	cases := map[string]bool{
		jpeg:                  true,
		"\n" + jpeg + "\n":    true,
		"A1 x2, grazie":       false,
		"/9j/ prefix in text": false,
		"":                    false,
	}
	for in, want := range cases {
		if got := IsMediaBlob(in); got != want {
			t.Errorf("IsMediaBlob(%.20q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseSingleWine(t *testing.T) {
	cases := []struct {
		body string
		want []Option
	}{
		{"Disponibili:\n\n30 x Château Langoa Barton 2022 a 47,50€", []Option{{Letter: "A", WineName: "Château Langoa Barton 2022", Quantity: 30, Price: 47.5}}},
		{"Riesling Secco\nDisponibili: Riesling 2025 a 14,50€ (CASSA INTERA a 78€)", []Option{
			{Letter: "A", WineName: "Riesling 2025", Price: 14.5},
			{Letter: "A", WineName: "Riesling 2025 (cassa intera)", Price: 78, Case: true}}},
		{"A. 6x Barolo a 40€", nil},                                // lettered: the usual rules
		{"12 x Barolo 2019 a 40€\n6 x Barbaresco 2018 a 30€", nil}, // two wines
		{"Venduto 12 x Barolo a 40€", nil},
		{"Buongiorno a tutti!", nil},
	}
	for _, c := range cases {
		got, ok := ParseSingleWine(c.body)
		if ok != (c.want != nil) || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseSingleWine(%q) = %+v, %v; want %+v", c.body, got, ok, c.want)
		}
	}
}
