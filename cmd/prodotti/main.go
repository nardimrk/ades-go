// Command prodotti fills the Prodotti table from the listings: every wine
// offered becomes a product named "<wine> <vintage>" (prices ignored).
//
//	go run ./cmd/prodotti                 # dry run: counts, sample, -out list
//	go run ./cmd/prodotti -apply          # insert the new products
//	go run ./cmd/prodotti -llm -apply     # LLM pass: fill Cantina/Area, read unparsed listings
//	go run ./cmd/prodotti -review a.txt,b.txt [-apply]
//	                                      # apply reviewed corrections "id|name|cantina|area|del"
//
// The LLM pass uses OPENROUTER_API_KEY / OPENROUTER_MODEL from .env, is
// resumable and stops at OpenRouter's daily free quota (rerun the next day).
// Products already in Prodotti (same wine + vintage, ignoring case, accents,
// "Château") are never duplicated. Back up data/wine.db before -apply.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"adesgo/internal/config"
	"adesgo/internal/db"
	"adesgo/internal/llm"
	"adesgo/internal/service"
)

func main() {
	dbPath := flag.String("db", "data/wine.db", "SQLite database")
	apply := flag.Bool("apply", false, "insert the products (default: dry run)")
	out := flag.String("out", "", "write the full candidate list (TSV) to this file")
	useLLM := flag.Bool("llm", false, "LLM pass instead of the rules")
	maxReq := flag.Int("max", 400, "LLM pass: max requests to OpenRouter")
	review := flag.String("review", "", "comma-separated decision files to apply (see service.ProductDecision)")
	flag.Parse()

	ctx := context.Background()
	st, err := db.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	svc := service.New(st, &config.Config{}, nil)

	if *review != "" {
		ds, err := service.ReadProductDecisions(strings.Split(*review, ","))
		if err != nil {
			log.Fatal(err)
		}
		rep, err := svc.ApplyProductDecisions(ctx, ds, *apply)
		if err != nil {
			log.Fatal(err)
		}
		if *out != "" {
			if err := os.WriteFile(*out, []byte(strings.Join(rep.Lines, "\n")+"\n"), 0o644); err != nil {
				log.Fatal(err)
			}
			fmt.Println("changes written to", *out)
		}
		fmt.Printf("decisions: %d · added: %d · renamed: %d · cantina set: %d · area set: %d · deleted: %d · unchanged: %d\n",
			len(ds), rep.Added, rep.Renamed, rep.Winery, rep.Area, rep.Deleted, rep.Unchanged)
		if !*apply {
			fmt.Println("dry run: nothing written (use -apply)")
		}
		return
	}

	if *useLLM {
		cfg := config.Load()
		client := llm.New(cfg.OpenRouterAPIKey, cfg.OpenRouterModel)
		if !client.Enabled() {
			log.Fatal("OPENROUTER_API_KEY not set")
		}
		found, req1, err := svc.ProductsFromUnparsedListings(ctx, client, *maxReq, *apply)
		fmt.Printf("listings read: %d requests · new products: %d\n", req1, len(found))
		for i, c := range found {
			if i < 15 {
				fmt.Printf("  + %s  [%s · %s]\n", c.Description, c.Winery, c.Area)
			}
		}
		if err == nil {
			var changed, req2 int
			changed, req2, err = svc.EnrichProducts(ctx, client, 25, *maxReq-req1, *apply)
			fmt.Printf("Cantina/Area: %d products filled in %d requests\n", changed, req2)
		}
		if errors.Is(err, llm.ErrDailyCap) {
			fmt.Println("stopped: OpenRouter daily free quota used up — rerun after the reset, it resumes where it stopped")
		} else if err != nil {
			log.Fatal(err)
		}
		if !*apply {
			fmt.Println("dry run: nothing written (use -apply)")
		}
		return
	}

	cands, withoutWine, err := svc.ProductCandidatesFromListings(ctx)
	if err != nil {
		log.Fatal(err)
	}
	existing, err := svc.ExistingProductKeys(ctx)
	if err != nil {
		log.Fatal(err)
	}
	var fresh []service.ProductCandidate
	noVintage := 0
	for _, c := range cands {
		if existing[service.ProductKey(c.Description)] {
			continue
		}
		if c.Vintage == "" {
			noVintage++
		}
		fresh = append(fresh, c)
	}
	fmt.Printf("wines found: %d · already in Prodotti: %d · new: %d (without vintage: %d)\n",
		len(cands), len(cands)-len(fresh), len(fresh), noVintage)
	fmt.Printf("listings with no wine line recognised: %d\n", withoutWine)

	if *out != "" {
		var b strings.Builder
		b.WriteString("descrizione\tannata\tinserzioni\tesempio\n")
		for _, c := range fresh {
			fmt.Fprintf(&b, "%s\t%s\t%d\t%s\n", c.Description, c.Vintage, c.Listings, c.Example)
		}
		if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("list written to", *out)
	}
	if !*apply {
		fmt.Println("dry run: nothing written (use -apply)")
		return
	}
	n, err := svc.InsertProducts(ctx, fresh)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("inserted %d products\n", n)
}
