package textutil

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Parsing of WhatsApp ".txt" chat exports (Importa Chat page).

type ExportMessage struct {
	DT     *time.Time // nil when the timestamp couldn't be parsed
	Author string
	Body   string
}

var (
	msgWithAuthorRe = pyRe(`^\[(?P<date>\d{1,2}/\d{1,2}/\d{2,4}),\s*(?P<time>\d{1,2}:\d{2}(?::\d{2})?(?:\s?[APap][Mm])?)\]\s*(?P<author>[^:]+):\s(?P<body>.*)$`)
	msgNoAuthorRe   = pyRe(`^\[(?P<date>\d{1,2}/\d{1,2}/\d{2,4}),\s*(?P<time>\d{1,2}:\d{2}(?::\d{2})?(?:\s?[APap][Mm])?)\]\s*(?P<body>.*)$`)
	deletedRe       = pyRe(`(?i)messaggio.{0,20}eliminat|this message was deleted|you deleted this message`)
	waTimeRe        = pyRe(`^(\d{1,2}):(\d{2})(?::(\d{2}))?\s*([APap][Mm])?$`)
)

func isBidi(r rune) bool {
	return r == 0x200E || r == 0x200F || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

func stripBidi(s string) string {
	return strings.Map(func(r rune) rune {
		if isBidi(r) {
			return -1
		}
		return r
	}, s)
}

// fixMojibake reverses UTF-8 text that was mis-decoded as Latin-1 somewhere
// along the way ("Ã¨" → "è", "â¬" → "€"). Port of
// text.encode("latin-1").decode("utf-8") with fallback to the original.
func fixMojibake(text string) string {
	buf := make([]byte, 0, len(text))
	for _, r := range text {
		if r > 0xFF {
			return text
		}
		buf = append(buf, byte(r))
	}
	if !utf8.Valid(buf) {
		return text
	}
	return string(buf)
}

// DecodeExport mirrors bytes.decode("utf-8-sig", errors="replace").
func DecodeExport(raw []byte) string {
	s := strings.ToValidUTF8(string(raw), "�")
	return strings.TrimPrefix(s, string(rune(0xFEFF)))
}

func parseWADateTime(dateStr, timeStr string) *time.Time {
	m := waTimeRe.FindStringSubmatch(strings.TrimSpace(timeStr))
	if m == nil {
		return nil
	}
	hh, _ := strconv.Atoi(m[1])
	mm, _ := strconv.Atoi(m[2])
	ss := 0
	if m[3] != "" {
		ss, _ = strconv.Atoi(m[3])
	}
	if ampm := strings.ToUpper(m[4]); ampm != "" {
		if ampm == "PM" && hh != 12 {
			hh += 12
		}
		if ampm == "AM" && hh == 12 {
			hh = 0
		}
	}
	parts := strings.Split(dateStr, "/")
	if len(parts) != 3 {
		return nil
	}
	d, err1 := strconv.Atoi(parts[0])
	mo, err2 := strconv.Atoi(parts[1])
	y, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil
	}
	if y < 100 {
		y += 2000
	}
	if mo < 1 || mo > 12 || d < 1 || hh > 23 || mm > 59 || ss > 59 {
		return nil
	}
	t := time.Date(y, time.Month(mo), d, hh, mm, ss, 0, time.Local)
	if t.Day() != d { // e.g. 31/02 overflowed into March: invalid date
		return nil
	}
	return &t
}

// ParseWhatsAppExport splits an export into one message per entry, merging
// continuation lines and dropping system notices and deleted messages.
func ParseWhatsAppExport(text string) []ExportMessage {
	text = fixMojibake(text)
	var msgs []ExportMessage
	var cur *ExportMessage
	flush := func() {
		if cur != nil {
			msgs = append(msgs, *cur)
		}
	}
	for _, raw := range splitLines(text) {
		line := stripBidi(raw)
		if m := msgWithAuthorRe.FindStringSubmatch(line); m != nil && len([]rune(strings.TrimSpace(m[3]))) <= 60 {
			flush()
			cur = &ExportMessage{DT: parseWADateTime(m[1], m[2]), Author: strings.TrimSpace(m[3]), Body: m[4]}
			continue
		}
		if msgNoAuthorRe.MatchString(line) {
			// system notice (group created, member added, …): not a message
			flush()
			cur = nil
			continue
		}
		if cur != nil {
			cur.Body += "\n" + line
		}
	}
	flush()

	out := msgs[:0]
	for _, m := range msgs {
		if strings.TrimSpace(m.Body) != "" && !deletedRe.MatchString(m.Body) {
			out = append(out, m)
		}
	}
	return out
}

// splitLines mirrors str.splitlines() for the separators that occur in exports.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, " ", "\n")
	s = strings.ReplaceAll(s, " ", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
