// Package switchboard is the module root; it embeds the app's static assets (ADR-001) so the binary
// is self-contained. Web templates are embedded within internal/web.
package switchboard

import "embed"

// StaticFS holds the static assets served at /static/ (the charm-web tokens.css + component CSS,
// the split JS modules, fonts, and icons — ADR-0018).
//
//go:embed static
var StaticFS embed.FS
