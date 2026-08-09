// Package webassets embeds the built frontend so the harness ships as one
// binary with no runtime assets (docs/DESIGN.md §4.8). dist/ is web/'s Vite
// build output, written there by web/vite.config.ts's build.outDir, and is
// not in git beyond a .gitkeep. Run `npm --prefix web run build` before
// `go build`, or the binary compiles and serves nothing.
package webassets

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var files embed.FS

// Dist returns the built frontend rooted at its own top level, stripping
// the "dist" prefix embed.FS otherwise carries.
func Dist() (fs.FS, error) {
	return fs.Sub(files, "dist")
}
