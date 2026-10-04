// Command numeri gives orders and listings the codes with the year, once:
// IMP0544 → ORD26nnnn (by year of the order date) and INS0725 → INS26nnnn
// (by year of the post), numbered from 0001 every year in date order. The
// confirmed orders follow their order; the old codes are kept in
// number_renames, so old links still open the right order.
//
//	adesgo-numeri                  # dry run: what would change
//	adesgo-numeri -apply           # backup next to the DB, then apply
//
// In Docker (stop the app first):
//
//	docker compose run --rm --entrypoint adesgo-numeri app
//	docker compose run --rm --entrypoint adesgo-numeri app -apply
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"adesgo/internal/db"
)

func main() {
	def := os.Getenv("DB_PATH")
	if def == "" {
		def = "data/wine.db"
	}
	dbPath := flag.String("db", def, "SQLite database")
	apply := flag.Bool("apply", false, "write the changes (default: dry run)")
	flag.Parse()
	ctx := context.Background()

	if _, err := os.Stat(*dbPath); err != nil {
		log.Fatalf("database %s: %v", *dbPath, err)
	}
	st, err := db.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	// never renumber a damaged database: restore a healthy backup first
	var check string
	if err := st.DB.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		log.Fatalf("il database non è integro (PRAGMA quick_check: %q %v): niente fatto", check, err)
	}

	if *apply {
		backup := fmt.Sprintf("%s.backup_%s", *dbPath, time.Now().Format("20060102_150405"))
		if _, err := st.DB.ExecContext(ctx, "VACUUM INTO ?", backup); err != nil {
			log.Fatalf("backup: %v", err)
		}
		fmt.Println("backup:", backup)
	}

	var before struct{ quots, orders, listings int }
	st.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM quotations").Scan(&before.quots)
	st.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders").Scan(&before.orders)
	st.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM listings").Scan(&before.listings)

	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback()
	changed, err := db.Renumber(ctx, tx)
	if err != nil {
		log.Fatal(err)
	}

	// checks before writing: every order still found by its confirmed
	// orders, codes unique and of the new form
	var orphanBefore, orphanAfter, badQ, badL int
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders o WHERE NOT EXISTS (SELECT 1 FROM quotations q WHERE q.quotation_number = o.quotation_number)`).Scan(&orphanAfter)
	st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders o WHERE NOT EXISTS (SELECT 1 FROM quotations q WHERE q.quotation_number = o.quotation_number)`).Scan(&orphanBefore)
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM quotations WHERE quotation_number NOT GLOB 'ORD[0-9][0-9][0-9][0-9][0-9][0-9]*'`).Scan(&badQ)
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM listings WHERE listing_number NOT GLOB 'INS[0-9][0-9][0-9][0-9][0-9][0-9]*'`).Scan(&badL)
	if orphanAfter != orphanBefore || badQ > 0 || badL > 0 {
		log.Fatalf("controllo fallito, niente scritto: ordini confermati senza ordine %d→%d, codici ordine non validi %d, codici inserzione non validi %d",
			orphanBefore, orphanAfter, badQ, badL)
	}

	var nOrd, nIns int
	for _, c := range changed {
		if c.Kind == "ordine" {
			nOrd++
		} else {
			nIns++
		}
	}
	mode := "DRY RUN (niente scritto: aggiungi -apply)"
	if *apply {
		if err := tx.Commit(); err != nil {
			log.Fatal(err)
		}
		mode = "APPLICATO"
	}
	fmt.Printf("%s\n  ordini rinumerati: %d di %d (ordini confermati collegati: %d)\n  inserzioni rinumerate: %d di %d\n",
		mode, nOrd, before.quots, before.orders, nIns, before.listings)
	// examples: the first and last few of each kind
	show := func(kind string) {
		var ex []db.Renumbered
		for _, c := range changed {
			if c.Kind == kind {
				ex = append(ex, c)
			}
		}
		for i, c := range ex {
			if i < 3 || i >= len(ex)-3 {
				fmt.Printf("   · %s %s → %s\n", kind, c.Old, c.New)
			} else if i == 3 {
				fmt.Println("   · …")
			}
		}
	}
	show("ordine")
	show("inserzione")
}
