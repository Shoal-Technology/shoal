// Package portal holds the web portal's static files (HTML, CSS,
// JavaScript), embedded in the binary. The API behind them is in
// internal/server.
package portal

import "embed"

// Static is the portal's files, under static/.
//
//go:embed static
var Static embed.FS
