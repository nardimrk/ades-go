// Command clienti cleans the customers: it removes the WhatsApp groups saved
// as customers, creates the records missing for reply authors, marks the
// seller's ids and gives every customer a code CLI0001… (see
// service.CleanupCustomers). Merging duplicates is done on /clienti/unisci.
//
//	go run ./cmd/clienti                 # dry run: what would change
//	go run ./cmd/clienti -apply          # backup first, then apply
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"adesgo/internal/config"
	"adesgo/internal/db"
	"adesgo/internal/service"
)

func main() {
	dbPath := flag.String("db", "data/wine.db", "SQLite database")
	apply := flag.Bool("apply", false, "write the changes (default: dry run)")
	flag.Parse()
	ctx := context.Background()

	st, err := db.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	if *apply {
		backup := fmt.Sprintf("%s.backup_%s", *dbPath, time.Now().Format("20060102_150405"))
		if _, err := st.DB.ExecContext(ctx, "VACUUM INTO ?", backup); err != nil {
			log.Fatalf("backup: %v", err)
		}
		fmt.Println("backup:", backup)
	}
	svc := service.New(st, &config.Config{}, nil)
	rep, err := svc.CleanupCustomers(ctx, *apply)
	if err != nil {
		log.Fatal(err)
	}
	mode := "DRY RUN (niente scritto)"
	if *apply {
		mode = "APPLICATO"
	}
	fmt.Println(mode)
	fmt.Printf("  gruppi WhatsApp tolti dai clienti: %d %v\n", len(rep.GroupsRemoved), rep.GroupsRemoved)
	fmt.Printf("  clienti creati per autori di risposte senza scheda: %d %v\n", len(rep.AuthorsCreated), rep.AuthorsCreated)
	fmt.Printf("  ID segnati come venditore (non clienti): %d %v\n", len(rep.Sellers), rep.Sellers)
	if *apply {
		fmt.Printf("  clienti con codice CLI: %d\n", rep.Coded)
	}
}
