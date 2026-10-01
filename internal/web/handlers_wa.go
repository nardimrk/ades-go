package web

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"adesgo/internal/service"
	"adesgo/internal/textutil"
	"adesgo/internal/web/views"
)

// ── Importa Chat ─────────────────────────────────────────────────────────────

type upload struct {
	name    string
	msgs    []textutil.ExportMessage
	created time.Time
	done    string
}

func (s *Server) getUpload(key string) *upload {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()
	return s.uploads[key]
}

func (s *Server) importaBase(r *http.Request) views.ImportaData {
	names := s.svc.GroupNames(r.Context())
	return views.ImportaData{OwnerDisplayName: s.cfg.OwnerDisplayName, Chats: s.cfg.ChannelIDs, ChatNames: names}
}

func (s *Server) importa(w http.ResponseWriter, r *http.Request) {
	d := s.importaBase(r)
	if key := r.URL.Query().Get("k"); key != "" {
		if up := s.getUpload(key); up != nil {
			s.fillPreview(&d, key, up, r.URL.Query().Get("owner"), r.URL.Query().Get("chat"))
		}
	}
	render(w, r, views.ImportaPage(d))
}

func (s *Server) importaUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		d := s.importaBase(r)
		d.Error = "File non valido: " + err.Error()
		render(w, r, views.ImportaPage(d))
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	key := newToken()[:16]
	s.uploadsMu.Lock()
	s.uploads[key] = &upload{name: hdr.Filename, msgs: textutil.ParseWhatsAppExport(textutil.DecodeExport(raw)), created: time.Now()}
	s.uploadsMu.Unlock()
	http.Redirect(w, r, "/importa?k="+key, http.StatusSeeOther)
}

func (s *Server) fillPreview(d *views.ImportaData, key string, up *upload, owner, chat string) {
	d.Key, d.FileName, d.Done = key, up.name, up.done
	d.Authors = service.TopAuthors(up.msgs)
	if owner == "" {
		// default: the configured seller name, else the most active author
		for _, a := range d.Authors {
			if strings.EqualFold(strings.TrimSpace(a.Author), strings.TrimSpace(s.cfg.OwnerDisplayName)) {
				owner = a.Author
			}
		}
		if owner == "" && len(d.Authors) > 0 {
			owner = d.Authors[0].Author
		}
	}
	d.Owner = owner
	d.Chat = chat
	if d.Chat == "" && len(d.Chats) > 0 {
		d.Chat = d.Chats[0]
	}
	items := service.ClassifyExport(up.msgs, strings.ToLower(strings.TrimSpace(owner)))
	d.Groups = service.MergeSimilarListings(items)
	d.Total = len(items)
	for _, it := range items {
		if it.IsListing {
			d.ListingsRaw++
		}
	}
	d.Replies = d.Total - d.ListingsRaw
}

func (s *Server) importaPreview(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("k")
	up := s.getUpload(key)
	if up == nil {
		w.Header().Set("HX-Redirect", "/importa")
		return
	}
	d := s.importaBase(r)
	s.fillPreview(&d, key, up, r.URL.Query().Get("owner"), r.URL.Query().Get("chat"))
	render(w, r, views.ImportaPreview(d))
}

func (s *Server) importaRun(w http.ResponseWriter, r *http.Request) {
	key := r.FormValue("k")
	up := s.getUpload(key)
	if up == nil {
		w.Header().Set("HX-Redirect", "/importa")
		return
	}
	d := s.importaBase(r)
	chat := r.FormValue("chat")
	if !s.cfg.IsChannel(chat) || chat == "" {
		s.fail(w, r, fmt.Errorf("chat di destinazione non valida"))
		return
	}
	s.fillPreview(&d, key, up, r.FormValue("owner"), chat)
	if up.done == "" {
		chatName := d.ChatNames[chat]
		nl, nr, err := s.svc.ImportGroups(r.Context(), d.Groups, chat, chatName)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.uploadsMu.Lock()
		up.done = fmt.Sprintf("Importate %d inserzioni e %d risposte in “%s”.", nl, nr, chatName)
		s.uploadsMu.Unlock()
	}
	d.Done = up.done
	render(w, r, views.ImportaPreview(d))
}

// ── WhatsApp ─────────────────────────────────────────────────────────────────

func (s *Server) whatsappData(r *http.Request) views.WhatsAppData {
	st := s.wa.Status()
	d := views.WhatsAppData{Status: st, Channels: s.cfg.ChannelIDs, OwnerID: s.cfg.OwnerID, LLMEnabled: s.llm.Enabled()}
	if st.QR != "" && !st.LoggedIn {
		if png, err := qrcode.Encode(st.QR, qrcode.Medium, 320); err == nil {
			d.QRDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		}
	}
	return d
}

func (s *Server) whatsapp(w http.ResponseWriter, r *http.Request) {
	d := s.whatsappData(r)
	if d.Status.LoggedIn {
		d.ChatNames = s.svc.GroupNames(r.Context())
		groups, err := s.wa.JoinedGroups(r.Context())
		if err != nil {
			d.GroupsErr = err.Error()
		}
		d.Groups = groups
	}
	render(w, r, views.WhatsAppPage(d))
}

func (s *Server) whatsappStatus(w http.ResponseWriter, r *http.Request) {
	d := s.whatsappData(r)
	if d.Status.LoggedIn {
		// paired: reload the whole page to show groups and settings
		w.Header().Set("HX-Refresh", "true")
	}
	render(w, r, views.WhatsAppStatus(d))
}

func (s *Server) whatsappPair(w http.ResponseWriter, r *http.Request) {
	if err := s.wa.RestartPairing(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	time.Sleep(1500 * time.Millisecond) // give the first QR a moment to arrive
	render(w, r, views.WhatsAppStatus(s.whatsappData(r)))
}

func (s *Server) whatsappLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.wa.Logout(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) whatsappHistory(w http.ResponseWriter, r *http.Request) {
	chat := r.FormValue("chat")
	if err := s.wa.RequestHistory(r.Context(), chat, 50); err != nil {
		render(w, r, views.Alert("warning", "Richiesta non inviata: "+err.Error()))
		return
	}
	render(w, r, views.Alert("success", "Richiesta inviata al telefono: i messaggi più vecchi arriveranno a breve e verranno importati automaticamente."))
}
