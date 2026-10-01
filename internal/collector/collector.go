// Package collector turns WhatsApp group messages into listings and replies
// (port of app/collector.py + app/import_history.py). It is transport
// agnostic: package wa converts whatsmeow events into Message values.
package collector

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"adesgo/internal/classify"
	"adesgo/internal/config"
	"adesgo/internal/db"
	"adesgo/internal/llm"
)

// Message is a normalized incoming group message.
type Message struct {
	ID        string // serialized id ("false_<chat>_<id>_<author>"); "" = unknown
	ChatID    string
	ChatName  string
	AuthorID  string // preferred author id (LID when known, like the old bridge)
	AuthorAlt string // alternative id of the same author (phone number JID), may be ""
	Author    string // display name
	FromMe    bool
	Body      string
	Timestamp int64
	QuotedID  string // raw stanza id of the quoted message, "" when not a quote
	MediaURL  *string
	Source    string // "live" or "history", for logs
}

type Collector struct {
	cfg   *config.Config
	store *db.Store
	llm   *llm.Client
	cls   *classify.Classifier
}

func New(cfg *config.Config, store *db.Store, llmClient *llm.Client) *Collector {
	return &Collector{cfg: cfg, store: store, llm: llmClient, cls: &classify.Classifier{LLM: llmClient}}
}

func (c *Collector) isOwner(m *Message) bool {
	if c.cfg.OwnerID != "" {
		return m.AuthorID == c.cfg.OwnerID || (m.AuthorAlt != "" && m.AuthorAlt == c.cfg.OwnerID)
	}
	return m.FromMe
}

// Handle classifies and stores one message. Messages must be fed in
// chronological order per chat (reply → listing linking relies on it).
func (c *Collector) Handle(ctx context.Context, m *Message) error {
	if !c.cfg.IsChannel(m.ChatID) {
		return nil
	}
	if m.ID == "" {
		// Stable fallback id: whole-second timestamps alone would collide in a
		// burst, and a body hash keeps a redelivered message idempotent.
		sum := md5.Sum([]byte(m.Body))
		m.ID = fmt.Sprintf("%s_%d_%s", m.ChatID, m.Timestamp, hex.EncodeToString(sum[:])[:10])
	}
	authorName := m.Author
	if authorName == "" {
		authorName = m.AuthorID
	}

	kind := classify.Reply
	var match *db.Candidate
	if c.isOwner(m) && m.QuotedID == "" {
		if c.store.BodyMentionsQuotation(ctx, m.Body) {
			log.Printf("[SKIP]    quotation update ignored: %q", short(m.Body))
			return nil
		}
		candidates, err := c.store.RecentOwnListings(ctx, m.ChatID, m.AuthorID, m.Timestamp, 14)
		if err != nil {
			return err
		}
		kind, match = c.cls.OwnerMessage(ctx, m.Body, candidates)
	}

	switch kind {
	case classify.Create:
		f := c.llm.ParseListing(ctx, m.Body)
		if err := c.store.InsertListing(ctx, db.Listing{
			MsgID: m.ID, ChatID: m.ChatID, ChatName: m.ChatName,
			AuthorID: m.AuthorID, AuthorName: authorName, Body: m.Body,
			WineName: f.WineName, Price: f.Price, Vintage: f.Vintage,
			MediaURL: m.MediaURL, Timestamp: m.Timestamp,
		}); err != nil {
			return err
		}
		log.Printf("[LISTING] (%s) %s: %q", m.Source, authorName, short(m.Body))
		return nil

	case classify.Update:
		f := c.llm.ParseListing(ctx, m.Body)
		if err := c.store.UpdateListing(ctx, match.MsgID, db.Listing{
			Body: m.Body, WineName: f.WineName, Price: f.Price, Vintage: f.Vintage,
			MediaURL: m.MediaURL, Timestamp: m.Timestamp,
		}); err != nil {
			return err
		}
		log.Printf("[UPDATE]  (%s) %s: %q", m.Source, authorName, short(m.Body))
		return nil
	}

	// Reply
	listingID, hasListing := c.store.FindListingForReply(ctx, m.ChatID, m.Timestamp, m.QuotedID)
	if err := c.store.UpsertUser(ctx, m.AuthorID, m.Author); err != nil {
		return err
	}

	targetAuthor, targetListing, storedBody := m.AuthorID, listingID, m.Body
	resolved := ""

	// Someone (typically the owner) narrating a change to a NAMED customer's
	// order in third person: attribute the resolved order to that customer.
	customers, err := c.store.KnownCustomers(ctx, m.ChatID)
	if err != nil {
		return err
	}
	if named := classify.FindNamedCustomer(m.Body, customers); named != nil && named.ID != m.AuthorID {
		target, err := c.store.RecentRepliesByAuthor(ctx, m.ChatID, named.ID, m.Timestamp)
		if err != nil {
			return err
		}
		if lid, code, ok := c.cls.ResolveAdminOrderNote(ctx, m.Body, target); ok {
			targetListing, targetAuthor, resolved = lid, named.ID, code
			hasListing = true
			storedBody = m.Body + " → " + code
		}
	}

	// Otherwise, the sender changing their own order in natural language, or
	// ordering without option codes ("Mie!" after the owner offers the
	// remaining bottles, "1 Coutet 2019").
	if storedBody == m.Body && hasListing && m.AuthorID != "" && !c.isOwner(m) {
		prior, err := c.store.RecentOwnReplies(ctx, listingID, m.AuthorID, m.Timestamp)
		if err != nil {
			return err
		}
		listing, notes, err := c.store.ListingOwnerNotes(ctx, listingID, m.Timestamp)
		if err != nil {
			return err
		}
		code := ""
		if classify.HasPriorOrder(prior) {
			code = c.cls.ResolveOrderUpdate(ctx, m.Body, listing, m.Timestamp, prior, notes)
		} else {
			code = c.cls.ResolveNewOrder(ctx, m.Body, listing, m.Timestamp, notes)
		}
		if code != "" {
			resolved = code
			storedBody = m.Body + " → " + code
		}
	}

	var listingPtr *string
	if hasListing {
		listingPtr = &targetListing
	}
	if err := c.store.InsertReply(ctx, db.Reply{
		MsgID: m.ID, ListingMsgID: listingPtr, ChatID: m.ChatID,
		AuthorID: targetAuthor, Body: storedBody, Timestamp: m.Timestamp,
	}); err != nil {
		return err
	}
	suffix := ""
	if resolved != "" {
		suffix = "  (order update resolved → " + resolved + ")"
	}
	log.Printf("[REPLY]   (%s) %s: %q%s", m.Source, authorName, short(m.Body), suffix)
	return nil
}

func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > 60 {
		return string(r[:60])
	}
	return s
}
