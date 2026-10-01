package classify

import (
	"context"
	"testing"

	"adesgo/internal/db"
	"adesgo/internal/llm"
)

const coutetListing = `Château Coutet

Demi Bouteilles Disponibili:

A. 21 x Château Coutet 2016 a 27,50€

B. 9 x Château Coutet 2019 a 23,50€

Cassa intera (12 x 375ml) sconto 15%.`

const coutetOffer = `Buongiorno a tutti, sono lieto di confermare tutte le bottiglie richieste ieri.

Al netto degli ordini rimangono disponibili ora 4 mezze bottiglie di Coutet 2019.`

func TestClaimOffer(t *testing.T) {
	cases := map[string]string{
		"Mie!":          "4B",
		"mie":           "4B",
		"Le prendo io!": "4B",
		"Per me grazie": "4B",
		"Grazie":        "",
		"Che annata?":   "",
	}
	for body, want := range cases {
		if got := ClaimOffer(body, coutetOffer, coutetListing); got != want {
			t.Errorf("ClaimOffer(%q) = %q, want %q", body, got, want)
		}
	}
	if got := ClaimOffer("Mie!", "Grazie a tutti!", coutetListing); got != "" {
		t.Errorf("claim without an offer = %q, want empty", got)
	}
}

func TestResolveNewOrderFallback(t *testing.T) {
	c := &Classifier{LLM: llm.New("", "")} // LLM disabled: rules only
	ts := int64(1790844323)
	notes := []db.Candidate{{Body: coutetOffer, Timestamp: 1790843454}}
	if got := c.ResolveNewOrder(context.Background(), "Mie!", coutetListing, ts, notes); got != "4B" {
		t.Errorf("got %q, want 4B", got)
	}
	// an offer older than the window is not claimable
	if got := c.ResolveNewOrder(context.Background(), "Mie!", coutetListing, ts+3*86400, notes); got != "" {
		t.Errorf("stale offer: got %q, want empty", got)
	}
	// codes are already handled by the regular parser
	if got := c.ResolveNewOrder(context.Background(), "2B", coutetListing, ts, notes); got != "" {
		t.Errorf("explicit code: got %q, want empty", got)
	}
}

func TestResolveOrderUpdateClaimFallback(t *testing.T) {
	c := &Classifier{LLM: llm.New("", "")}
	ts := int64(1790844323)
	notes := []db.Candidate{{Body: coutetOffer, Timestamp: 1790843454}}
	cases := []struct{ prior, body, want string }{
		{"2B grazie", "Mie!", "6B"},
		{"2A", "Mie!", "4B"},
		{"2B grazie", "Grazie", ""},
	}
	for _, tc := range cases {
		prior := []db.Candidate{{Body: tc.prior, Timestamp: 1790776565}}
		if got := c.ResolveOrderUpdate(context.Background(), tc.body, coutetListing, ts, prior, notes); got != tc.want {
			t.Errorf("%s + %q = %q, want %q", tc.prior, tc.body, got, tc.want)
		}
	}
}
