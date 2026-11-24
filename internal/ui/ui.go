// Package ui embeds the viewer's static assets.
//
// There is no npm build step and no CDN: the browser gets exactly what is compiled
// into the binary, which keeps the image self-contained and the deployment free of
// a second supply chain. uPlot is vendored under static/vendor with its MIT licence.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed static
var embedded embed.FS

// FS returns the asset filesystem rooted at static/.
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		// Only possible if the embed directive itself is wrong, which is a build
		// error rather than a runtime condition.
		panic("ui: embedded assets missing: " + err.Error())
	}
	return sub
}
