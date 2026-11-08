// Package webui embeds ducktel's static dashboard assets — the human-facing
// counterpart to the MCP tools, backed by the same internal/telemetry.Core via
// internal/webapi. There is no build step: this is plain HTML/CSS/vanilla JS,
// embedded directly into the binary so the deployed dashboard has no external
// asset dependency of any kind, matching ducktel's single-binary stance.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed dashboard/*
var files embed.FS

// FS returns the embedded dashboard assets rooted at "dashboard", so a caller
// sees index.html, app.js, style.css at the filesystem root rather than
// nested under a dashboard/ prefix.
func FS() fs.FS {
	sub, err := fs.Sub(files, "dashboard")
	if err != nil {
		// Only possible if the embed directive above stops matching this
		// directory's layout — a build-time programmer error, not a runtime one.
		panic(err)
	}
	return sub
}
