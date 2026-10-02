package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log"
	"net/http"
	"strings"

	"adesgo/internal/web/views"
)

// Magic-link login, compatible with the tokens issued by the Streamlit app
// (same auth_tokens table and cookie name): links never expire.

const cookieName = "wa_auth_token"

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) validToken(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}
	var one int
	err := s.store.DB.QueryRowContext(ctx,
		"SELECT 1 FROM auth_tokens WHERE token = ? AND expires_at > datetime('now')", token).Scan(&one)
	return err == nil
}

// userEmail is the login email of the request's session ("" if unknown).
func (s *Server) userEmail(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	var email string
	_ = s.store.DB.QueryRowContext(r.Context(), "SELECT email FROM auth_tokens WHERE token = ?", c.Value).Scan(&email)
	return email
}

func (s *Server) setAuthCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/",
		MaxAge:   100 * 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(s.cfg.AppURL, "https://"),
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/login" || p == "/healthz" || strings.HasPrefix(p, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		// A magic link: plant the cookie and drop the token from the URL.
		if tok := r.URL.Query().Get("token"); tok != "" {
			if s.validToken(r.Context(), tok) {
				s.setAuthCookie(w, tok)
				u := *r.URL
				qs := u.Query()
				qs.Del("token")
				u.RawQuery = qs.Encode()
				http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
				return
			}
		}
		if c, err := r.Cookie(cookieName); err == nil && s.validToken(r.Context(), c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, views.Login(false, ""))
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if !s.cfg.AuthEmails[email] {
		render(w, r, views.Login(false, "Email non autorizzata."))
		return
	}
	token := newToken()
	if _, err := s.store.DB.ExecContext(r.Context(),
		"INSERT INTO auth_tokens (token, email, expires_at) VALUES (?, ?, datetime('now', '+100 years'))", token, email); err != nil {
		s.fail(w, r, err)
		return
	}
	link := s.cfg.AppURL + "/?token=" + token
	if s.mail.Configured() {
		body := "Clicca il link per accedere ad ADES Wine Club (il link non scade mai):\n\n" + link
		if err := s.mail.Send(r.Context(), email, "ADE Wine Club - Link di accesso", body); err != nil {
			log.Printf("[auth] mailgun: %v", err)
			render(w, r, views.Login(false, "Invio email non riuscito, riprova più tardi."))
			return
		}
	} else {
		log.Printf("[auth] Mailgun non configurato — magic link per %s: %s", email, link)
	}
	render(w, r, views.Login(true, ""))
}

// logout only clears the cookie: the emailed link stays valid, as before.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
