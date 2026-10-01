// Package mail sends emails through Mailgun (EU region): magic login links
// and the weekly database backup.
package mail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

type Mailgun struct {
	APIKey string
	Domain string
	http   *http.Client
}

func New(apiKey, domain string) *Mailgun {
	return &Mailgun{APIKey: apiKey, Domain: domain, http: &http.Client{Timeout: 60 * time.Second}}
}

func (m *Mailgun) Configured() bool { return m.APIKey != "" && m.Domain != "" }

type Attachment struct {
	Name string
	Data []byte
}

func (m *Mailgun) Send(ctx context.Context, to, subject, text string, attachments ...Attachment) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("from", fmt.Sprintf("ADE Wine Club <noreply@%s>", m.Domain))
	w.WriteField("to", to)
	w.WriteField("subject", subject)
	w.WriteField("text", text)
	for _, a := range attachments {
		part, err := w.CreateFormFile("attachment", a.Name)
		if err != nil {
			return err
		}
		if _, err := part.Write(a.Data); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.eu.mailgun.net/v3/%s/messages", m.Domain), &body)
	if err != nil {
		return err
	}
	req.SetBasicAuth("api", m.APIKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("mailgun HTTP %d: %s", resp.StatusCode, msg)
	}
	return nil
}
