// Package web serves the HTMX dashboard.
package web

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"adesgo/internal/config"
	"adesgo/internal/db"
	"adesgo/internal/llm"
	"adesgo/internal/mail"
	"adesgo/internal/service"
	"adesgo/internal/wa"
	"adesgo/internal/web/views"
)

type Server struct {
	cfg     *config.Config
	store   *db.Store
	svc     *service.Service
	wa      *wa.Manager
	mail    *mail.Mailgun
	llm     *llm.Client
	titles  *service.TitleJob
	reviews *service.ReviewJob

	uploadsMu sync.Mutex
	uploads   map[string]*upload
}

func New(cfg *config.Config, store *db.Store, svc *service.Service, waMgr *wa.Manager, mailer *mail.Mailgun, llmClient *llm.Client, titles *service.TitleJob, reviews *service.ReviewJob) *Server {
	return &Server{cfg: cfg, store: store, svc: svc, wa: waMgr, mail: mailer, llm: llmClient, titles: titles, reviews: reviews, uploads: map[string]*upload{}}
}

func (s *Server) Handler(static fs.FS) http.Handler {
	views.AssetVersion = assetVersion(static)
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/inserzioni", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /inserzioni", s.inserzioni)
	mux.HandleFunc("GET /inserzioni/list", s.inserzioniList)
	mux.HandleFunc("GET /inserzioni/anno", s.inserzioniAnno)
	mux.HandleFunc("POST /inserzioni/rename", s.inserzioniRename)
	mux.HandleFunc("POST /inserzioni/consegna", s.inserzioniConsegna)
	mux.HandleFunc("GET /inserzioni/titles", s.titleJobStatus)
	mux.HandleFunc("POST /inserzioni/titles", s.titleJobStart)
	mux.HandleFunc("GET /inserzioni/review", s.reviewStatus)
	mux.HandleFunc("POST /inserzioni/review", s.reviewStart)
	mux.HandleFunc("POST /inserzioni/reply", s.addReply)
	mux.HandleFunc("POST /inserzioni/stock", s.initialStock)
	mux.HandleFunc("DELETE /replies/{id}", s.deleteReply)
	mux.HandleFunc("GET /replies/{id}/move", s.moveReplyForm)
	mux.HandleFunc("POST /replies/{id}/move", s.moveReply)
	mux.HandleFunc("GET /replies/{id}/order", s.replyOrderForm)
	mux.HandleFunc("POST /replies/{id}/order", s.replyOrder)

	mux.HandleFunc("GET /ordini", s.preventivi)
	mux.HandleFunc("GET /ordini/list", s.preventiviList)
	mux.HandleFunc("GET /ordini/recenti", s.preventiviRecenti)
	mux.HandleFunc("GET /ordini/anno", s.preventiviAnno)
	mux.HandleFunc("POST /ordini/import", s.preventiviImport)
	mux.HandleFunc("GET /ordini/view", s.preventivoView)
	mux.HandleFunc("POST /ordini/section", s.preventivoSection)
	mux.HandleFunc("POST /ordini/consegna", s.orderItemConsegna)
	mux.HandleFunc("POST /ordini/opzione", s.orderOption)
	mux.HandleFunc("POST /ordini/elimina", s.deleteOrder)
	mux.HandleFunc("GET /ordini/sposta", s.moveOrderForm)
	mux.HandleFunc("POST /ordini/sposta", s.moveOrder)
	mux.HandleFunc("GET /ordini/link", s.orderLinkForm)
	mux.HandleFunc("POST /ordini/link", s.orderLink)
	mux.HandleFunc("GET /ordini/nuovo", s.nuovoPreventivo)
	mux.HandleFunc("POST /ordini/nuovo", s.nuovoPreventivoSubmit)
	// "Preventivi" was renamed "Ordini": old links and bookmarks keep working
	mux.HandleFunc("GET /preventivi", redirectRenamed)
	mux.HandleFunc("GET /preventivi/{rest...}", redirectRenamed)

	mux.HandleFunc("GET /consegne", s.consegne)
	mux.HandleFunc("GET /consegne/result", s.consegneResult)
	mux.HandleFunc("GET /consegne/anno", s.consegneAnno)
	mux.HandleFunc("GET /consegne/show", s.consegneShow)
	mux.HandleFunc("GET /consegne/excel", s.consegneExcel)

	mux.HandleFunc("GET /clienti", s.clienti)
	mux.HandleFunc("GET /clienti/list", s.clientiList)
	mux.HandleFunc("GET /clienti/dettagli", s.clienteDettagli)
	mux.HandleFunc("POST /clienti/field", s.clienteField)
	mux.HandleFunc("POST /clienti/new", s.clienteNew)
	mux.HandleFunc("POST /clienti/delete", s.clienteDelete)
	mux.HandleFunc("POST /clienti/create", s.customerCreate)
	mux.HandleFunc("POST /prodotti/create", s.productCreate)
	mux.HandleFunc("GET /clienti/unisci", s.mergePage)
	mux.HandleFunc("POST /clienti/unisci", s.mergeCustomers)
	mux.HandleFunc("POST /clienti/separa", s.unmergeCustomer)
	mux.HandleFunc("GET /prodotti", s.prodotti)
	mux.HandleFunc("GET /prodotti/list", s.prodottiList)
	mux.HandleFunc("POST /prodotti/field", s.prodottoField)
	mux.HandleFunc("POST /prodotti/new", s.prodottoNew)
	mux.HandleFunc("POST /prodotti/delete", s.prodottoDelete)
	mux.HandleFunc("GET /statistiche", s.statistiche)
	mux.HandleFunc("GET /statistiche/attivi", s.statisticheAttivi)
	mux.HandleFunc("POST /statistiche/venditore", s.statisticheVenditore)

	mux.HandleFunc("GET /importa", s.importa)
	mux.HandleFunc("POST /importa", s.importaUpload)
	mux.HandleFunc("GET /importa/preview", s.importaPreview)
	mux.HandleFunc("POST /importa/run", s.importaRun)

	mux.HandleFunc("GET /whatsapp", s.whatsapp)
	mux.HandleFunc("GET /whatsapp/status", s.whatsappStatus)
	mux.HandleFunc("POST /whatsapp/pair", s.whatsappPair)
	mux.HandleFunc("POST /whatsapp/logout", s.whatsappLogout)
	mux.HandleFunc("POST /whatsapp/history", s.whatsappHistory)

	mux.HandleFunc("GET /notifiche/count", s.notificheCount)
	mux.HandleFunc("POST /notifiche/open", s.notificheOpen)

	return s.recoverer(s.logRequests(s.requireAuth(mux)))
}

// ── helpers ──────────────────────────────────────────────────────────────────

// redirectRenamed sends /preventivi/... to the same page under /ordini/...
func redirectRenamed(w http.ResponseWriter, r *http.Request) {
	u := *r.URL
	u.Path = "/ordini" + strings.TrimPrefix(r.URL.Path, "/preventivi")
	http.Redirect(w, r, u.RequestURI(), http.StatusMovedPermanently)
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		log.Printf("[web] render %s: %v", r.URL.Path, err)
	}
}

func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true"
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[web] %s %s: %v", r.Method, r.URL.Path, err)
	if isHTMX(r) {
		w.Header().Set("HX-Reswap", "none")
		w.WriteHeader(http.StatusOK)
		render(w, r, views.Toast("error", "Errore: "+err.Error()))
		return
	}
	http.Error(w, "Errore: "+err.Error(), http.StatusInternalServerError)
}

func toast(w http.ResponseWriter, r *http.Request, kind, msg string) {
	render(w, r, views.Toast(kind, msg))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if !strings.HasPrefix(r.URL.Path, "/static/") && r.URL.Path != "/whatsapp/status" && r.URL.Path != "/notifiche/count" {
			log.Printf("[web] %s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("[web] panic %s: %v", r.URL.Path, v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// cleanupUploads drops parsed chat exports older than an hour.
func (s *Server) CleanupLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.uploadsMu.Lock()
			for k, u := range s.uploads {
				if time.Since(u.created) > time.Hour {
					delete(s.uploads, k)
				}
			}
			s.uploadsMu.Unlock()
		}
	}
}

// assetVersion is a short hash of the CSS and JS: their links carry it
// ("/static/app.js?v=…"), so a browser fetches them again after an update
// instead of keeping an old cached copy.
func assetVersion(static fs.FS) string {
	h := sha1.New()
	for _, name := range []string{"app.css", "app.js"} {
		if b, err := fs.ReadFile(static, name); err == nil {
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}
