package textutil

import "strings"

// Base64 prefixes of the image formats WhatsApp sends as thumbnails
// (JPEG, PNG, WebP, GIF).
var mediaPrefixes = []string{"/9j/", "iVBORw0KGgo", "UklGR", "R0lGOD"}

// IsMediaBlob reports whether a message body is an inline base64 image
// rather than text. The old whatsapp-web.js bridge stored the thumbnail of
// captionless photos as the body; those must never reach the LLM or the UI.
func IsMediaBlob(body string) bool {
	s := strings.TrimSpace(body)
	if len(s) < 100 || strings.ContainsAny(s, " \n\t") {
		return false
	}
	for _, p := range mediaPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
