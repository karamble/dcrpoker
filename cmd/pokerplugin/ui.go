package main

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// The interface, baked in.
//
// It ships inside this binary rather than beside it, so "one signed static
// binary per platform" stays true and the page a player looks at is covered by
// the same signature as the code that moves their money. A UI fetched at
// runtime would be the one part of this nobody verified.
//
// It is served without the bearer token, unlike everything else here. The
// bundle is the same public bytes in every copy of the binary and contains no
// secret and no configuration; guarding it would force a token into the frame's
// src attribute, where it lands in history, referrers and every proxy log
// between here and the browser. The bundle is public. The API is not, and that
// is where the guard belongs.

// Two things are embedded, at two paths, and they must stay at two paths.
//
// ui/dist is the build's output and is never committed - it is gitignored down
// to the .gitignore that keeps the directory present, because //go:embed fails
// outright on one that is missing. ui/placeholder.html is committed and is what
// gets served when nothing was built. Putting both at one path would mean a
// build silently overwriting a tracked file, and a checkout that looked dirty
// for having been built.
//
//go:embed all:ui/dist
//go:embed ui/placeholder.html
var uiFS embed.FS

// uiIndex is the built bundle: one self-contained document, because the page is
// framed at an opaque origin where a separate module script would be a
// cross-origin fetch. See ui/vite.config.ts.
const uiIndex = "ui/dist/index.html"

// uiBuilt reports whether a real bundle was baked in.
//
// A placeholder is committed so `go build ./...` works for somebody who has
// never run npm, which keeps the Go side buildable and testable on its own and
// is worth more than the alternative. But a release that shipped the
// placeholder would serve a page explaining itself, through a proxy, inside a
// frame - where it reads as every layer in between being broken. So it is
// something the binary can be asked, and the release script asks.
func uiBuilt() bool {
	f, err := uiFS.Open(uiIndex)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// handleUI serves the bundle.
//
// No single-page fallback. There is no client-side router in this UI, so a path
// that names nothing is a missing asset, and answering it with the index would
// turn a build mistake into a blank page rather than a 404.
func (p *plugin) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == "" {
		name = "index.html"
	}
	// Nothing here composes a path from what the caller sent - the file is
	// opened from the embedded tree, which contains only what was built, so
	// there is nowhere to escape to. The check is against a name that would
	// not be found anyway.
	if strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}

	within := "ui/dist/" + name
	if name == "index.html" && !uiBuilt() {
		within = "ui/placeholder.html"
	}
	body, err := fs.ReadFile(uiFS, within)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Set explicitly. This runs in an image built from scratch, with no
	// /etc/mime.types and nothing else in it; Go's built-in table does
	// cover these, and depending on that implicitly in a distroless image
	// is how a thing fails once, in production, on one platform.
	switch ext := strings.ToLower(path.Ext(name)); ext {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".json":
		w.Header().Set("Content-Type", "application/json")
	default:
		if ct := mime.TypeByExtension(ext); ct != "" {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
	}

	if name == "index.html" {
		// A new signed binary is a new interface, and the document is
		// what pulls the rest in.
		w.Header().Set("Cache-Control", "no-store")
	} else {
		// Vite hashes these into their names, so a given path is one
		// exact set of bytes forever.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	// Deliberately no X-Frame-Options. This is meant to be framed, by the
	// host that proxies it; the modern control is frame-ancestors, and in
	// older browsers the legacy header would override it and break the one
	// arrangement this page exists for. The host sets the policy, because
	// only the host knows the origin it is served on.
	if _, err := w.Write(body); err != nil {
		return
	}
}
