package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestNumbers(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "wine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if c := Code(OrderPrefix, 2026, 544); c != "ORD260544" {
		t.Fatalf("Code = %s", c)
	}
	// old codes, out of date order, and a confirmed order
	for _, q := range [][2]string{{"IMP0001", "2025-12-30"}, {"IMP0002", "2026-01-05"}, {"IMP0003", "2025-06-01"}} {
		if _, err := st.DB.Exec("INSERT INTO quotations (quotation_number, quotation_date) VALUES (?, ?)", q[0], q[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.Exec("INSERT INTO orders (quotation_number, user_name) VALUES ('IMP0001', 'Mario')"); err != nil {
		t.Fatal(err)
	}
	ts := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 12, 0, 0, 0, time.Local).Unix() }
	for i, l := range []int64{ts(2025, 5, 1), ts(2026, 2, 1), ts(2025, 7, 1)} {
		if _, err := st.DB.Exec("INSERT INTO listings (msg_id, chat_id, body, timestamp, listing_number) VALUES (?, 'c', 'x', ?, ?)", "m"+string(rune('a'+i)), l, Code("INS", 0, i+1)); err != nil {
			t.Fatal(err)
		}
	}
	tx, _ := st.DB.Begin()
	changed, err := Renumber(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"IMP0003": "ORD250001", "IMP0001": "ORD250002", "IMP0002": "ORD260001"}
	got := map[string]string{}
	for _, c := range changed {
		got[c.Old] = c.New
	}
	for o, n := range want {
		if got[o] != n || st.RenamedTo(ctx, o) != n {
			t.Errorf("%s → %s (renames %s), want %s", o, got[o], st.RenamedTo(ctx, o), n)
		}
	}
	var on string
	st.DB.QueryRow("SELECT quotation_number FROM orders").Scan(&on)
	if on != "ORD250002" {
		t.Errorf("confirmed order points to %s", on)
	}
	var l1, l3 string
	st.DB.QueryRow("SELECT listing_number FROM listings WHERE msg_id = 'ma'").Scan(&l1)
	st.DB.QueryRow("SELECT listing_number FROM listings WHERE msg_id = 'mc'").Scan(&l3)
	if l1 != "INS250001" || l3 != "INS250002" {
		t.Errorf("listings %s %s", l1, l3)
	}
	// next numbers restart every year
	if n, _ := NextOrderNumber(ctx, st.DB, "2026-10-04"); n != "ORD260002" {
		t.Errorf("next 2026 = %s", n)
	}
	if n, _ := NextOrderNumber(ctx, st.DB, "2027-01-02"); n != "ORD270001" {
		t.Errorf("next 2027 = %s", n)
	}
	if _, err := st.DB.Exec("INSERT INTO listings (msg_id, chat_id, body, timestamp) VALUES ('md', 'c', 'x', ?)", ts(2026, 3, 1)); err != nil {
		t.Fatal(err)
	}
	if err := st.AssignListingNumbers(ctx); err != nil {
		t.Fatal(err)
	}
	var l4 string
	st.DB.QueryRow("SELECT listing_number FROM listings WHERE msg_id = 'md'").Scan(&l4)
	if l4 != "INS260002" {
		t.Errorf("new listing = %s", l4)
	}
	// running it again changes nothing
	tx, _ = st.DB.Begin()
	if again, err := Renumber(ctx, tx); err != nil || len(again) != 0 {
		t.Errorf("second run: %+v, %v", again, err)
	}
	tx.Rollback()
}
