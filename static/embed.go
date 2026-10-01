// Package static embeds the CSS/JS assets served under /static/.
package static

import "embed"

//go:embed *.css *.js
var FS embed.FS
