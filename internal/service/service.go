// Package service implements the dashboard's business logic (port of the
// Streamlit app): campaigns, quotations ("preventivi"), deliveries
// ("consegne"), customers, products, statistics and .txt chat import.
package service

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"adesgo/internal/config"
	"adesgo/internal/db"
	"adesgo/internal/textutil"
)

// GroupNamer resolves WhatsApp group names (implemented by wa.Manager).
type GroupNamer interface {
	GroupNames(ctx context.Context) map[string]string
}

type Service struct {
	Store  *db.Store
	Cfg    *config.Config
	Groups GroupNamer
}

func New(store *db.Store, cfg *config.Config, groups GroupNamer) *Service {
	return &Service{Store: store, Cfg: cfg, Groups: groups}
}

func (s *Service) db() *sql.DB { return s.Store.DB }

// GroupNames returns names for the configured channels (raw id as fallback).
func (s *Service) GroupNames(ctx context.Context) map[string]string {
	if s.Groups != nil {
		names := s.Groups.GroupNames(ctx)
		for id, n := range names {
			names[id] = textutil.StripEmoji(n)
		}
		return names
	}
	known := s.Store.KnownChatNames(ctx)
	out := map[string]string{}
	for _, id := range s.Cfg.ChannelIDs {
		if n := textutil.StripEmoji(known[id]); n != "" {
			out[id] = n
		} else {
			out[id] = id
		}
	}
	return out
}

// FmtName shows a raw WhatsApp id as "...12345678"; names pass through.
func FmtName(name string) string {
	name = textutil.StripEmoji(name)
	if name == "" {
		return "Sconosciuto"
	}
	if strings.Contains(name, "@") {
		num, _, _ := strings.Cut(name, "@")
		if len(num) > 8 {
			return "..." + num[len(num)-8:]
		}
		return num
	}
	return name
}

// FmtTS formats a unix timestamp as dd/mm/yyyy HH:MM (local time).
func FmtTS(ts int64) string { return time.Unix(ts, 0).Format("02/01/2006 15:04") }

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Money formats an amount with two decimals.
func Money(v float64) string { return fmt.Sprintf("%.2f", v) }

// Num mirrors Python's f"{v:g}".
func Num(v float64) string { return fmt.Sprintf("%g", v) }

// userNameSQL is the display name of a reply author: the users.name unless
// empty or itself an id, else the raw author id.
const userNameSQL = `COALESCE(NULLIF(CASE WHEN u.name LIKE '%@%' THEN NULL ELSE u.name END, ''), r.author_id)`

func nullStr(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}
