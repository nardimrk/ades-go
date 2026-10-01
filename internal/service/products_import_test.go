package service

import (
	"strings"
	"testing"
)

func TestWineFromLine(t *testing.T) {
	cases := map[string][2]string{
		"17 x Cheval des Andes 2023 a 65€":                                {"Cheval des Andes", "2023"},
		"A. 6x Château Pontet-Canet 2014 a 84€ + iva":                     {"Château Pontet-Canet", "2014"},
		"B. 12 x Magnum a 48€ + iva":                                      {"", ""},
		"D. Venduto.":                                                     {"", ""},
		"A. CASSA INTERA Mâcon-Prissé Le Clos Bio a 120€ (93/100)":        {"Mâcon-Prissé Le Clos Bio", ""},
		"24 x Château d’Yquem 2021 375ml a 159€ + iva":                    {"Château d’Yquem", "2021"},
		"6 x D. Ausone 2007 a 640€":                                       {"Ausone", "2007"},
		"12 x Le Petit Cheval Blanc (Bianco di Cheval Blanc) 2023 a 100€": {"Le Petit Cheval Blanc", "2023"},
		"Prezzo speciale a 25€":                                           {"", ""},
		"Disponibili: 90 x Branaire Ducru 2025 a 35€ !!":                  {"Branaire Ducru", "2025"},
	}
	for in, want := range cases {
		n, v := wineFromLine(in)
		if n != want[0] || v != want[1] {
			t.Errorf("wineFromLine(%q) = %q, %q; want %q, %q", in, n, v, want[0], want[1])
		}
	}
}

func TestListingHeader(t *testing.T) {
	cases := map[string][2]string{
		"Marc Colin 2023 disponibili:\nA. 10 x Santenay a 30€":                           {"Marc Colin", "2023"},
		"Domaine Bertrand-Bachelet   Disponibili:\nBianchi 2024\nA. 60 x Santenay a 23€": {"Domaine Bertrand-Bachelet", "2024"},
		"Bianchi 2023\nA. 18 x Eclat de Calcaire a 39€":                                  {"", "2023"},
		"Esclusiva ADE’S in Italia\nA. 1 x Vino a 10€":                                   {"", ""},
		"Oggi vi propongo un vino straordinario, prodotto in Borgogna.\nA. 1 x X a 9€":   {"", ""},
	}
	for in, want := range cases {
		p, v := listingHeader(strings.Split(in, "\n"))
		if p != want[0] || v != want[1] {
			t.Errorf("listingHeader(%q) = %q, %q; want %q, %q", in, p, v, want[0], want[1])
		}
	}
}

func TestProductKey(t *testing.T) {
	same := [][2]string{
		{"Château Canon 2017", "Chateau Canon 2017"},
		{"Soldera Case Basse - Brunello di Montalcino 2020", "Soldera Case Basse Brunello di Montalcino 2020"},
		{"Domaine Vacheron Sancerre 2023", "vacheron sancerre 2023"},
	}
	for _, p := range same {
		if ProductKey(p[0]) != ProductKey(p[1]) {
			t.Errorf("%q and %q should be the same product", p[0], p[1])
		}
	}
	if ProductKey("Château Canon 2017") == ProductKey("Château Canon 2018") {
		t.Error("different vintages must be different products")
	}
	if !sharesWords("Roc Breïa Chardonnay", "Theo Dancer - Roc Breïa") || sharesWords("Schieferreich Riesling", "Thanisch Riesling Mosella") {
		t.Error("sharesWords")
	}
}

func TestExtractJSON(t *testing.T) {
	if got := extractJSON("Ecco:\n```json\n[{\"n\":1}]\n```"); got != `[{"n":1}]` {
		t.Errorf("extractJSON = %q", got)
	}
	if extractJSON("nessun vino") != "" {
		t.Error("expected empty")
	}
}
