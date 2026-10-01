// Package llm is the optional OpenRouter integration (port of app/agent.py).
// Every method degrades gracefully: with no API key, or when every model
// fails, it returns "not available" and the caller falls back to its rules.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"adesgo/internal/textutil"
)

const apiURL = "https://openrouter.ai/api/v1/chat/completions"

const defaultModel = "inclusionai/ling-3.0-flash-sante:free"

// Free models to fall through when the configured one is rate-limited: each
// provider's free pool is flaky independently of the others. This doesn't
// help once OpenRouter's account-wide daily free cap is hit.
var fallbackModels = []string{
	"inclusionai/ling-3.0-flash-sante:free",
	"liquid/lfm-2.5-2.6b:free",
	"dots-studio/dots-3-note-preview:free",
	"poolside/laguna-s-2.1:free",
	"poolside/laguna-xs-2.1:free",
}

// ErrDailyCap is returned once OpenRouter's account-wide free daily quota is
// used up: every free model is blocked until the reset, so retrying is useless.
var ErrDailyCap = errors.New("OpenRouter: quota giornaliera gratuita esaurita")

type Client struct {
	apiKey string
	model  string
	http   *http.Client
}

func New(apiKey, model string) *Client {
	if model == "" {
		model = defaultModel
	}
	return &Client{apiKey: apiKey, model: model, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) Enabled() bool { return c != nil && c.apiKey != "" }

func (c *Client) chat(ctx context.Context, prompt string, maxTokens int) (string, error) {
	models := []string{c.model}
	for _, m := range fallbackModels {
		if m != c.model {
			models = append(models, m)
		}
	}
	var lastErr error
	for i, model := range models {
		text, err := c.complete(ctx, model, prompt, maxTokens)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if errors.Is(err, ErrDailyCap) {
			return "", err
		}
		if i < len(models)-1 {
			log.Printf("[llm] %s failed (%v), trying %s...", model, err, models[i+1])
		}
	}
	return "", lastErr
}

func (c *Client) complete(ctx context.Context, model, prompt string, maxTokens int) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": maxTokens,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		if resp.StatusCode == http.StatusTooManyRequests && bytes.Contains(msg, []byte("free_tier_daily")) {
			return "", ErrDailyCap
		}
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// ── listing parsing ──────────────────────────────────────────────────────────

const parsePrompt = `Sei un assistente che estrae dati strutturati da annunci di vendita vini.
Dal testo qui sotto restituisci SOLO un oggetto JSON con questi campi:
- wine_name (string|null): nome del vino
- producer (string|null): produttore/cantina
- price (number|null): prezzo in euro, solo numero
- vintage (number|null): annata (anno)
- region (string|null): regione/denominazione
- quantity (string|null): formato/quantità (es. "6 bottiglie", "magnum")

Nessun testo extra, nessun markdown. Testo annuncio:
---
%s
---`

// ParseListing tries the LLM and falls back to the regex parser.
func (c *Client) ParseListing(ctx context.Context, body string) textutil.ListingFields {
	if c.Enabled() && body != "" {
		// reasoning models need room to think before emitting the JSON
		text, err := c.chat(ctx, fmt.Sprintf(parsePrompt, body), 500)
		if err == nil {
			text = strings.TrimSpace(text)
			text = strings.TrimPrefix(text, "```json")
			text = strings.TrimPrefix(text, "```")
			text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
			var data struct {
				WineName *string  `json:"wine_name"`
				Price    *float64 `json:"price"`
				Vintage  *float64 `json:"vintage"`
			}
			if json.Unmarshal([]byte(text), &data) == nil {
				f := textutil.ListingFields{WineName: data.WineName, Price: data.Price}
				if data.Vintage != nil {
					v := int(*data.Vintage)
					f.Vintage = &v
				}
				return f
			}
			log.Printf("[llm] parse: invalid JSON, falling back to regex")
		} else {
			log.Printf("[llm] parse failed, falling back to regex: %v", err)
		}
	}
	return textutil.ParseListing(body)
}

// ── create / update / reply tie-break ────────────────────────────────────────

const classifyPrompt = `Sei un assistente che classifica messaggi WhatsApp in un gruppo di vendita vini.
Il messaggio è stato scritto dal gestore del gruppo (proprietario). Decidi se è:
- create: un nuovo annuncio di vendita, diverso dall'annuncio precedente mostrato sotto (vino diverso, offerta diversa, o non c'è un annuncio precedente pertinente)
- update: una ripubblicazione o modifica dello stesso annuncio precedente (es. cambia prezzo, quantità rimasta o altri dettagli, ma è la stessa offerta/vino)
- reply: non è affatto un annuncio (es. un commento, una risposta a un cliente, saluti, conferma di un ordine)

Rispondi SOLO con una parola tra: create, update, reply. Nessun altro testo.

Messaggio da classificare:
---
%s
---

Annuncio precedente più simile (se assente, considera che non esiste):
---
%s
---`

// ClassifyListing returns "create", "update", "reply" or "" (unavailable).
func (c *Client) ClassifyListing(ctx context.Context, body, candidate string) string {
	if !c.Enabled() || body == "" {
		return ""
	}
	if candidate == "" {
		candidate = "(nessuno)"
	}
	text, err := c.chat(ctx, fmt.Sprintf(classifyPrompt, body, candidate), 300)
	if err != nil {
		log.Printf("[llm] classify failed, falling back to rules: %v", err)
		return ""
	}
	text = strings.ToLower(strings.TrimSpace(text))
	for _, kind := range []string{"create", "update", "reply"} {
		if strings.Contains(text, kind) {
			return kind
		}
	}
	return ""
}

// ── natural-language order changes ───────────────────────────────────────────

const orderUpdatePrompt = `Sei un assistente che interpreta messaggi WhatsApp di clienti che hanno
già ordinato in un'inserzione di vendita vini. L'ordine più recente del cliente in questa
inserzione è: %s
(il formato è NUMERO+LETTERA, es. "3A" = 3 bottiglie dell'opzione A; più codici separati da
virgola indicano più opzioni ordinate)

Ultimi messaggi del gestore in questa inserzione, dal più recente (possono offrire bottiglie
rimaste, es. "rimangono disponibili 4 mezze bottiglie di Coutet 2019"):
---
%s
---

Il cliente ha appena scritto:
---
%s
---

Se questo messaggio modifica la quantità di un'opzione già ordinata sopra (es. "aggiungo una
bottiglia", "un'altra di quella A", "metti 5 invece di 3", "annullo, ne prendo solo 2"), rispondi
SOLO con il nuovo codice risultante, stesso formato NUMERO+LETTERA (es. "4A").
Se il cliente prende le bottiglie rimaste offerte dal gestore per un'opzione già ordinata
(es. "mie!", "le prendo io", "per me"), somma la quantità offerta a quella già ordinata
(es. ordine "2B" + gestore "rimangono 4 bottiglie di B" + "mie!" = "6B").
Se il messaggio NON modifica l'ordine (una domanda, un saluto, un ringraziamento, un commento
non legato all'ordine, o un ordine di un'opzione mai citata sopra), rispondi SOLO con: NONE

Nessun altro testo.`

var codeRe = regexp.MustCompile(`(\d+)\s*([A-J])`)

// ResolveOrderUpdate asks whether body modifies the prior order (e.g.
// "aggiungo una bottiglia" after "3A" → "4A"). ownerNotes are the owner's
// latest messages on the listing ("" when none). Returns "" when not.
func (c *Client) ResolveOrderUpdate(ctx context.Context, body, priorCodes, ownerNotes string) string {
	if !c.Enabled() || body == "" {
		return ""
	}
	if ownerNotes == "" {
		ownerNotes = "(nessuno)"
	}
	text, err := c.chat(ctx, fmt.Sprintf(orderUpdatePrompt, priorCodes, ownerNotes, body), 300)
	if err != nil {
		log.Printf("[llm] order-update resolution failed: %v", err)
		return ""
	}
	m := codeRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(text)))
	if m == nil {
		return ""
	}
	return m[1] + m[2]
}

// ── orders written without option codes ──────────────────────────────────────

const newOrderPrompt = `Sei un assistente che interpreta messaggi WhatsApp di clienti in un gruppo di vendita vini.
Il cliente NON ha ancora ordinato in questa inserzione. Le opzioni sono indicate da lettere
(A, B, C...) nell'annuncio:
---
%s
---

Ultimi messaggi del gestore in questa inserzione, dal più recente (se presenti):
---
%s
---

Il cliente ha appena scritto:
---
%s
---

Regole:
- Se il cliente prende le bottiglie rimaste offerte dal gestore (es. gestore: "rimangono disponibili
  4 mezze bottiglie di Coutet 2019", cliente: "Mie!", "mia", "le prendo io", "per me", "io!",
  "prenoto"), l'ordine è la quantità offerta, sull'opzione dell'annuncio che corrisponde al vino
  e all'annata indicati dal gestore (es. "4B" se B è Coutet 2019).
- Se il cliente indica il vino per nome o annata invece della lettera (es. "1 Coutet 2019",
  "due del 2016"), usa la lettera dell'opzione corrispondente (es. "1B").
- Se il cliente chiede bottiglie senza numero ("una", "due"...), convertilo in cifra.
- Rispondi SOLO con i codici NUMERO+LETTERA separati da virgola (es. "4B" oppure "1A, 2B").
- Se il messaggio non è un ordine (domanda, saluto, ringraziamento, commento) o non si capisce
  con certezza quale opzione o quantità, rispondi SOLO con: NONE

Nessun altro testo.`

// ResolveNewOrder reads an order written without option codes by a customer
// who hasn't ordered yet ("Mie!" after the owner offers the remaining
// bottles, "1 Coutet 2019"). Returns canonical codes ("4B", "1A, 2B") or "".
func (c *Client) ResolveNewOrder(ctx context.Context, body, listing, ownerNotes string) string {
	if !c.Enabled() || body == "" {
		return ""
	}
	if ownerNotes == "" {
		ownerNotes = "(nessuno)"
	}
	text, err := c.chat(ctx, fmt.Sprintf(newOrderPrompt, listing, ownerNotes, body), 300)
	if err != nil {
		log.Printf("[llm] new-order resolution failed: %v", err)
		return ""
	}
	text = strings.ToUpper(strings.TrimSpace(text))
	if strings.Contains(text, "NONE") {
		return ""
	}
	return textutil.FormatOrderCodes(textutil.ParseOrderCodes(text))
}

// ── listing titles ───────────────────────────────────────────────────────────

const titlePrompt = `Sei un assistente che dà un titolo breve agli annunci di vendita vini di un gruppo WhatsApp.
Scrivi SOLO il titolo, in una riga: produttore/cantina + vino + annata se presente.
Se l'annuncio propone più vini dello stesso produttore, usa il produttore e la denominazione comune.
Regole: massimo 6 parole e 45 caratteri; niente quantità, prezzi, "x", "€", "+ iva", "Disponibili",
emoji, virgolette o punteggiatura finale; nessuna spiegazione.

Annuncio:
---
%s
---`

// Title asks the LLM for a short listing title. The error is ErrDailyCap
// when the free quota is exhausted.
func (c *Client) Title(ctx context.Context, body string) (string, error) {
	if !c.Enabled() {
		return "", errors.New("OpenRouter non configurato")
	}
	if r := []rune(body); len(r) > 1500 {
		body = string(r[:1500])
	}
	text, err := c.chat(ctx, fmt.Sprintf(titlePrompt, body), 300)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "Titolo:"), "titolo:"))
	line = strings.Trim(line, " *_\"'“”«»`")
	title := textutil.CleanTitle(line)
	if title == "" || len([]rune(title)) > textutil.MaxTitleLen {
		return "", fmt.Errorf("titolo non valido: %q", text)
	}
	return title, nil
}

// Complete sends a free-form prompt (with the usual model fallbacks) and
// returns the reply text. The error is ErrDailyCap when the quota is over.
func (c *Client) Complete(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if !c.Enabled() {
		return "", errors.New("OpenRouter non configurato")
	}
	return c.chat(ctx, prompt, maxTokens)
}
