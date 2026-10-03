// Command pulizia cleans the listings table: it deletes the copies of the
// same message, merges the reposts of each inserzione into its first post
// and numbers the listings INS0001… (see service.CleanupListings).
//
//	go run ./cmd/pulizia                 # dry run: what would change
//	go run ./cmd/pulizia -apply          # backup first, then apply
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
	rep, err := svc.CleanupListings(ctx, *apply)
	if err != nil {
		log.Fatal(err)
	}
	mode := "DRY RUN (niente scritto)"
	if *apply {
		mode = "APPLICATO"
	}
	fmt.Printf("%s\n  copie dello stesso messaggio eliminate: %d\n  inserzioni con ripubblicazioni unite: %d (%d post uniti)\n  risposte spostate: %d\n  inserzioni rimaste e numerate: %d\n",
		mode, rep.Duplicates, rep.MergedGroups, rep.MergedPosts, rep.RepliesMoved, rep.Numbered)
	for _, e := range rep.Examples {
		fmt.Println("   ·", e)
	}
}
