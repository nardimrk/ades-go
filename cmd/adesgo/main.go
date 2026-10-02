// Command adesgo is the ADES Wine Club app: a single binary that links to
// WhatsApp as a companion device (whatsmeow), classifies the wine group's
// messages into listings and replies, stores them in SQLite and serves the
// HTMX dashboard.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"adesgo/internal/collector"
	"adesgo/internal/config"
	"adesgo/internal/db"
	"adesgo/internal/llm"
	"adesgo/internal/mail"
	"adesgo/internal/service"
	"adesgo/internal/wa"
	"adesgo/internal/web"
	"adesgo/static"
)

func main() {
	log.SetFlags(log.LstdFlags)
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer store.Close()
	// phone numbers WhatsApp already knows (LID → phone map), for customers
	// who have none yet; numbers typed in Clienti are kept
	if n, err := store.FillPhones(ctx); err != nil {
		log.Printf("[clienti] phone numbers: %v", err)
	} else if n > 0 {
		log.Printf("[clienti] %d phone numbers filled from WhatsApp", n)
	}

	llmClient := llm.New(cfg.OpenRouterAPIKey, cfg.OpenRouterModel)
	mailer := mail.New(cfg.MailgunAPIKey, cfg.MailgunDomain)
	col := collector.New(cfg, store, llmClient)

	waMgr, err := wa.New(ctx, cfg, store, col)
	if err != nil {
		log.Fatalf("whatsapp: %v", err)
	}
	defer waMgr.Close()
	if cfg.WADisabled {
		log.Printf("[wa] WA_DISABLED=1: not connecting to WhatsApp")
	} else if err := waMgr.Start(ctx); err != nil {
		log.Printf("[wa] start: %v (retry from %s/whatsapp)", err, cfg.AppURL)
	}

	if mailer.Configured() && cfg.BackupTo != "" {
		go store.RunWeeklyBackup(ctx, func(ctx context.Context, name string, data []byte) error {
			text := "Backup settimanale del database ADE Wine Club.\n\nFile: " + name +
				"\nData: " + time.Now().Format("2006-01-02 15:04")
			return mailer.Send(ctx, cfg.BackupTo, "ADE Wine Club – Backup DB "+time.Now().Format("2006-01-02"), text,
				mail.Attachment{Name: name, Data: data})
		})
	} else {
		log.Printf("[backup] disabled (needs MAILGUN_API_KEY, MAILGUN_DOMAIN and BACKUP_TO)")
	}

	svc := service.New(store, cfg, waMgr)
	titles := service.NewTitleJob(svc, llmClient)
	go titles.Loop(ctx, 30*time.Minute) // new listings get an LLM title too
	reviews := service.NewReviewJob(svc, llmClient)
	srv := web.New(cfg, store, svc, waMgr, mailer, llmClient, titles, reviews)
	go srv.CleanupLoop(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(static.FS),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("ADES Wine Club listening on %s (%s)", cfg.HTTPAddr, cfg.AppURL)
		if len(cfg.ChannelIDs) == 0 {
			log.Printf("warning: CHANNEL_ID not set — messages from every group will be collected")
		}
		if len(cfg.AuthEmails) == 0 {
			log.Printf("warning: AUTH_EMAILS not set — nobody can log in to the dashboard")
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdownCtx)
}
