package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"adesgo/internal/llm"
	"adesgo/internal/textutil"
)

// LLM pass of the Prodotti import (see cmd/prodotti -llm). Resumable: it only
// fills empty Cantina/Area fields and remembers which listings it has read,
// and it stops cleanly at OpenRouter's daily free quota.

const llmPace = 3500 * time.Millisecond // free tier: 20 requests / minute

// extractJSON returns the first JSON array in an LLM reply (models wrap it
// in prose or ``` fences).
func extractJSON(s string) string {
	i, j := strings.Index(s, "["), strings.LastIndex(s, "]")
	if i < 0 || j <= i {
		return ""
	}
	return s[i : j+1]
}

const enrichPrompt = `Sei un esperto di vini. Per ogni vino della lista indica il produttore (cantina / château / domaine)
e la zona di origine (denominazione o regione, es. "Pauillac", "Borgogna - Meursault", "Langhe", "Mosella").
Se non lo sai con ragionevole sicurezza lascia la stringa vuota: non inventare.
Rispondi SOLO con un array JSON, un oggetto per vino, nello stesso ordine:
[{"n": 1, "cantina": "...", "area": "..."}, ...]

Vini:
%s`

// EnrichProducts fills empty Cantina / Area of products, batch per request.
// Returns how many products changed and the requests used.
func (s *Service) EnrichProducts(ctx context.Context, client *llm.Client, batch, maxRequests int, apply bool) (changed, requests int, err error) {
	items, err := s.Items(ctx)
	if err != nil {
		return 0, 0, err
	}
	var todo []Item
	for _, it := range items {
		if it.Description != "" && (it.Winery == "" || it.Area == "") {
			todo = append(todo, it)
		}
	}
	log.Printf("[prodotti] %d products without Cantina or Area", len(todo))
	for start := 0; start < len(todo) && requests < maxRequests; start += batch {
		end := min(start+batch, len(todo))
		chunk := todo[start:end]
		var list strings.Builder
		for i, it := range chunk {
			fmt.Fprintf(&list, "%d. %s\n", i+1, it.Description)
		}
		if requests > 0 {
			time.Sleep(llmPace)
		}
		requests++
		text, err := client.Complete(ctx, fmt.Sprintf(enrichPrompt, list.String()), 3000)
		if errors.Is(err, llm.ErrDailyCap) {
			return changed, requests, err
		}
		if err != nil {
			log.Printf("[prodotti] batch %d-%d: %v", start+1, end, err)
			continue
		}
		var out []struct {
			N       int    `json:"n"`
			Cantina string `json:"cantina"`
			Area    string `json:"area"`
		}
		if json.Unmarshal([]byte(extractJSON(text)), &out) != nil {
			log.Printf("[prodotti] batch %d-%d: reply is not JSON", start+1, end)
			continue
		}
		for _, o := range out {
			if o.N < 1 || o.N > len(chunk) {
				continue
			}
			it := chunk[o.N-1]
			cantina, area := strings.TrimSpace(textutil.StripEmoji(o.Cantina)), strings.TrimSpace(textutil.StripEmoji(o.Area))
			did := false
			if it.Winery == "" && cantina != "" {
				did = true
				if apply {
					if err := s.SetItemField(ctx, it.ID, "winery", cantina); err != nil {
						return changed, requests, err
					}
				}
			}
			if it.Area == "" && area != "" {
				did = true
				if apply {
					if err := s.SetItemField(ctx, it.ID, "area", area); err != nil {
						return changed, requests, err
					}
				}
			}
			if did {
				changed++
				if changed <= 10 {
					log.Printf("[prodotti]   %s → cantina %q, area %q", it.Description, cantina, area)
				}
			}
		}
	}
	return changed, requests, nil
}

const listingPrompt = `Questo è un annuncio di vendita vini di un gruppo WhatsApp. Elenca i vini offerti.
Per ogni vino: "vino" = nome completo come prodotto (produttore + nome del vino, senza quantità, prezzo,
formato bottiglia o note), "annata" = anno (stringa vuota se non indicato o non millesimato),
"cantina" = produttore, "area" = denominazione/regione (vuota se incerta). Non inventare.
Se l'annuncio non offre vini (è un commento, un saluto, una descrizione) rispondi [].
Rispondi SOLO con un array JSON: [{"vino": "...", "annata": "...", "cantina": "...", "area": "..."}]

Annuncio:
---
%s
---`

// ProductsFromUnparsedListings asks the LLM for the wines of listings the
// rules didn't recognise. Listings already read are remembered in app_meta.
func (s *Service) ProductsFromUnparsedListings(ctx context.Context, client *llm.Client, maxRequests int, apply bool) (found []ProductCandidate, requests int, err error) {
	rows, err := s.db().QueryContext(ctx, "SELECT id, COALESCE(body,'') FROM listings ORDER BY timestamp DESC")
	if err != nil {
		return nil, 0, err
	}
	type listing struct {
		id   int64
		body string
	}
	var todo []listing
	for rows.Next() {
		var l listing
		if rows.Scan(&l.id, &l.body) != nil {
			continue
		}
		recognised := false
		for _, line := range strings.Split(l.body, "\n") {
			if n, _ := wineFromLine(line); n != "" {
				recognised = true
				break
			}
		}
		if !recognised && strings.TrimSpace(l.body) != "" && s.Store.Meta(ctx, "prodotti_llm:"+strconv.FormatInt(l.id, 10)) == "" {
			todo = append(todo, l)
		}
	}
	rows.Close()
	log.Printf("[prodotti] %d listings to read with the LLM", len(todo))

	existing, err := s.ExistingProductKeys(ctx)
	if err != nil {
		return nil, 0, err
	}
	for _, l := range todo {
		if requests >= maxRequests {
			break
		}
		if requests > 0 {
			time.Sleep(llmPace)
		}
		requests++
		body := []rune(l.body)
		if len(body) > 2500 {
			body = body[:2500]
		}
		text, err := client.Complete(ctx, fmt.Sprintf(listingPrompt, string(body)), 2500)
		if errors.Is(err, llm.ErrDailyCap) {
			return found, requests, err
		}
		if err != nil {
			log.Printf("[prodotti] listing %d: %v", l.id, err)
			continue
		}
		var wines []struct {
			Vino, Annata, Cantina, Area string
		}
		if json.Unmarshal([]byte(extractJSON(text)), &wines) != nil {
			log.Printf("[prodotti] listing %d: reply is not JSON", l.id)
			continue
		}
		var add []ProductCandidate
		for _, w := range wines {
			name := strings.TrimSpace(sizeRe.ReplaceAllString(textutil.StripEmoji(w.Vino), ""))
			vintage := ""
			if v := vintageRe.FindString(w.Annata); v != "" {
				vintage = v
				name = strings.TrimSpace(strings.Replace(name, v, "", 1))
			}
			if name == "" || len([]rune(name)) > 90 {
				continue
			}
			c := ProductCandidate{Description: withVintage(spacesRe.ReplaceAllString(name, " "), vintage), Vintage: vintage,
				Winery: strings.TrimSpace(w.Cantina), Area: strings.TrimSpace(w.Area), Listings: 1}
			k := ProductKey(c.Description)
			if existing[k] {
				continue
			}
			existing[k] = true
			add = append(add, c)
		}
		found = append(found, add...)
		if apply {
			if _, err := s.InsertProducts(ctx, add); err != nil {
				return found, requests, err
			}
			_ = s.Store.SetMeta(ctx, "prodotti_llm:"+strconv.FormatInt(l.id, 10), "1")
		}
	}
	return found, requests, nil
}
