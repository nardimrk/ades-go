// Package views holds the templ components of the dashboard.
package views

import (
	"fmt"

	"adesgo/internal/service"
	"adesgo/internal/textutil"
	"net/url"
	"strings"
	"time"
)

func money(v float64) string { return fmt.Sprintf("%.2f", v) }

func num(v float64) string { return fmt.Sprintf("%g", v) }

func itoa(v int) string { return fmt.Sprint(v) }

// plural: "1 cliente", "3 clienti".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func i64(v int64) string { return fmt.Sprint(v) }

// dateIT turns "YYYY-MM-DD ..." into "DD/MM/YYYY".
func dateIT(data string) string {
	d, _, _ := strings.Cut(data, " ")
	p := strings.Split(d, "-")
	if len(p) == 3 {
		return p[2] + "/" + p[1] + "/" + p[0]
	}
	return data
}

var itMonths = []string{"", "Gennaio", "Febbraio", "Marzo", "Aprile", "Maggio", "Giugno",
	"Luglio", "Agosto", "Settembre", "Ottobre", "Novembre", "Dicembre"}

// monthLabel: "Settembre 2026".
func monthLabel(year, month int) string {
	if month < 1 || month > 12 {
		return "Senza data"
	}
	return fmt.Sprintf("%s %d", itMonths[month], year)
}

// dayMonthShort turns "2026-09-30 …" into "30 set".
func dayMonthShort(data string) string {
	if len(data) < 10 {
		return ""
	}
	m := 0
	fmt.Sscanf(data[5:7], "%d", &m)
	if m < 1 || m > 12 {
		return data[8:10]
	}
	return data[8:10] + " " + strings.ToLower(itMonths[m][:3])
}

// noEmoji strips emoji from WhatsApp text at display time (stored data keeps them).
func noEmoji(s string) string { return textutil.StripEmoji(s) }

// orderReplies counts the replies read as an order.
func orderReplies(rs []service.ReplyView) int {
	n := 0
	for _, r := range rs {
		if r.Ordine {
			n++
		}
	}
	return n
}

// compactText drops emoji and blank lines, so a chat message takes as few
// lines as its content needs.
func compactText(s string) string {
	var out []string
	for _, l := range strings.Split(noEmoji(s), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, " \t\r"))
		}
	}
	return strings.Join(out, "\n")
}

// q builds a query string, skipping empty values.
func q(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	return v.Encode()
}

// nowMinute is the current local time for a datetime-local input.
func nowMinute() string { return time.Now().Format("2006-01-02T15:04") }

// clientChoices: every section's customer options, once each, in order.
func clientChoices(sections []service.CustomerSection) []string {
	seen := map[string]bool{}
	var out []string
	for _, sec := range sections {
		for _, c := range sec.Options {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}
