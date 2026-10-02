package web

import (
	"net/http"
	"time"

	"adesgo/internal/web/views"
)

// notificheCount: the bell's badge, polled by the menu.
func (s *Server) notificheCount(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.UnseenOrders(r.Context(), s.userEmail(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.NotifCount(n.Count, false))
}

// notificheOpen: the preview of the unseen orders. Showing it marks them as
// seen, so the badge (swapped out-of-band) goes back to zero.
func (s *Server) notificheOpen(w http.ResponseWriter, r *http.Request) {
	email := s.userEmail(r)
	n, err := s.svc.UnseenOrders(r.Context(), email)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.MarkOrdersSeen(r.Context(), email, n.MaxID); err != nil {
		s.fail(w, r, err)
		return
	}
	render(w, r, views.NotifPanel(n, time.Now()))
}
