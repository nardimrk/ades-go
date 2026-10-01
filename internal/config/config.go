// Package config loads the application settings from environment variables
// (optionally pre-populated from a local .env file).
package config

import (
	"bufio"
	"os"
	"strings"
)

type Config struct {
	DBPath   string
	HTTPAddr string

	// WhatsApp
	ChannelIDs []string // group JIDs to monitor (e.g. 120363xxx@g.us)
	OwnerID    string   // author whose messages become listings; empty = own messages (fromMe)

	// Dashboard auth (magic link via email)
	AuthEmails map[string]bool
	AppURL     string

	// Mailgun (magic links + weekly DB backup)
	MailgunAPIKey string
	MailgunDomain string
	BackupTo      string

	// OpenRouter LLM (optional)
	OpenRouterAPIKey string
	OpenRouterModel  string

	// Display name of the seller in WhatsApp .txt exports (Importa Chat page)
	OwnerDisplayName string

	// WADisabled skips connecting to WhatsApp (WA_DISABLED=1): for test
	// instances on a copy of the DB, which would otherwise reuse the linked
	// device's session and disconnect the real app ("stream replaced").
	WADisabled bool
}

// Load reads .env (if present, without overriding variables already set) and
// then builds the Config from the environment.
func Load() *Config {
	loadDotEnv(".env")

	c := &Config{
		DBPath:           env("DB_PATH", "data/wine.db"),
		HTTPAddr:         env("HTTP_ADDR", ":8080"),
		ChannelIDs:       splitList(os.Getenv("CHANNEL_ID")),
		OwnerID:          strings.TrimSpace(os.Getenv("OWNER_ID")),
		AuthEmails:       map[string]bool{},
		AppURL:           strings.TrimRight(env("APP_URL", "http://localhost:8080"), "/"),
		MailgunAPIKey:    os.Getenv("MAILGUN_API_KEY"),
		MailgunDomain:    os.Getenv("MAILGUN_DOMAIN"),
		BackupTo:         os.Getenv("BACKUP_TO"),
		OpenRouterAPIKey: os.Getenv("OPENROUTER_API_KEY"),
		OpenRouterModel:  os.Getenv("OPENROUTER_MODEL"),
		OwnerDisplayName: os.Getenv("OWNER_DISPLAY_NAME"),
		WADisabled:       os.Getenv("WA_DISABLED") == "1",
	}
	for _, e := range splitList(os.Getenv("AUTH_EMAILS")) {
		c.AuthEmails[strings.ToLower(e)] = true
	}
	return c
}

// IsChannel reports whether chatID is one of the monitored groups. With no
// CHANNEL_ID configured every chat is accepted (same as the Python collector).
func (c *Config) IsChannel(chatID string) bool {
	if len(c.ChannelIDs) == 0 {
		return true
	}
	for _, id := range c.ChannelIDs {
		if id == chatID {
			return true
		}
	}
	return false
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.TrimSpace(v)
		if i := strings.Index(v, " #"); i >= 0 && !strings.HasPrefix(v, `"`) {
			v = strings.TrimSpace(v[:i])
		}
		v = strings.Trim(v, `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}
