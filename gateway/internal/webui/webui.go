// Package webui serves the self-hosted PWA static assets (manifest, service
// worker, setup and call pages, icons) from the gateway process itself, so
// deployment is the gateway binary alone — no extra web root to stage. All
// assets are same-origin; nothing references third-party hosts.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed pwa
var pwaAssets embed.FS

// PWAHandler serves /pwa/* with the headers the platform requires:
//   - sw.js gets Service-Allowed scope /pwa/ and no-store so updates always
//     revalidate (a stale service worker would break push handling);
//   - HTML/manifest are no-store (tiny, security-relevant);
//   - icons may be cached for a day;
//   - /pwa/call maps to the call page shell (the handoff token travels in the
//     fragment, which never reaches this handler);
//   - nosniff + no-referrer defense in depth behind the Caddy edge headers.
func PWAHandler() http.Handler {
	assets, err := fs.Sub(pwaAssets, "pwa")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		switch {
		case path == "/pwa" || path == "/pwa/":
			serveSetup(w, r, files, "/")
			return
		case path == "/pwa/call" || path == "/pwa/call/":
			serveSetup(w, r, files, "/call.html")
			return
		case path == "/pwa/sw.js":
			w.Header().Set("Service-Worker-Allowed", "/pwa/")
			w.Header().Set("Cache-Control", "no-store")
		case strings.HasSuffix(path, ".html") || path == "/pwa/manifest.webmanifest":
			w.Header().Set("Cache-Control", "no-store")
			if strings.HasSuffix(path, ".webmanifest") {
				w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
			}
		case strings.HasSuffix(path, ".png"):
			w.Header().Set("Cache-Control", "public, max-age=86400")
		default:
			w.Header().Set("Cache-Control", "no-store")
		}
		http.StripPrefix("/pwa/", files).ServeHTTP(w, r)
	})
}

// serveSetup renders one of the two HTML shells with no-store; the fragment
// carrying the bind/handoff token never reaches the server.
func serveSetup(w http.ResponseWriter, r *http.Request, files http.Handler, name string) {
	w.Header().Set("Cache-Control", "no-store")
	r.URL.Path = name
	files.ServeHTTP(w, r)
}
