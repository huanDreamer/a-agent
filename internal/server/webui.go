package server

import (
	"context"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
)

// webuiFS holds the built admin UI. It is embedded so the binary is
// self-contained: no static files need to ship alongside it.
//
//go:embed all:webui/dist
var webuiFS embed.FS

// contentTypeFor resolves a content type from the file extension, defaulting to
// the SPA shell's type for extensionless paths.
func contentTypeFor(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// uiFS returns the UI rooted at the dist directory, or nil when the UI was not
// built.
func uiFS() fs.FS {
	sub, err := fs.Sub(webuiFS, "webui/dist")
	if err != nil {
		return nil
	}
	// A dist directory without index.html means "not built"; report that
	// clearly rather than serving 404s.
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}

// registerUI serves the embedded single-page app. Unknown paths fall back to
// index.html so client-side routing works.
//
// It is registered on the root group, so it must not shadow /api or the
// metrics path: Hertz matches the more specific registered routes first, and
// the catch-all here only handles what nothing else claimed.
func (s *Server) registerUI(h *server.Hertz) {
	ui := uiFS()
	if ui == nil {
		s.logger.Warn("admin UI is not built; only the JSON API and metrics are available",
			zapString("hint", "run `make web` or `cd web && npm run build`"))
		return
	}
	s.serveUI(h, ui)
}

// serveUI wires the read-only file handlers for the embedded UI.
func (s *Server) serveUI(h *server.Hertz, ui fs.FS) {
	// index.html is served without caching so a rebuilt UI is picked up.
	index, err := fs.ReadFile(ui, "index.html")
	if err != nil {
		s.logger.Error("read embedded index.html", zapError(err))
		return
	}

	// The shell is served for the root and for any unmatched GET (below), so
	// client-side routes survive a full page load.
	serveShell := func(_ context.Context, c *app.RequestContext) {
		c.Response.Header.Set("Cache-Control", "no-cache")
		c.Response.Header.Set("X-Content-Type-Options", "nosniff")
		c.Data(http.StatusOK, "text/html; charset=utf-8", index)
	}
	h.GET("/", serveShell)
	h.GET("/index.html", serveShell)

	// Assets are content-hashed by Vite, so they are safe to cache hard.
	h.GET("/assets/*filepath", func(_ context.Context, c *app.RequestContext) {
		name := "assets/" + strings.TrimPrefix(c.Param("filepath"), "/")
		data, err := fs.ReadFile(ui, name)
		if err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Response.Header.Set("Cache-Control", "public, max-age=31536000, immutable")
		c.Response.Header.Set("X-Content-Type-Options", "nosniff")
		c.Data(http.StatusOK, contentTypeFor(name), data)
	})

	// Any other GET returns the SPA shell so client-side routes work on a
	// full page load.
	h.NoRoute(func(_ context.Context, c *app.RequestContext) {
		if string(c.Method()) != http.MethodGet {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		// Never mask an unknown API path with the UI shell: an API client
		// should see a JSON 404, not HTML.
		if p := string(c.Path()); strings.HasPrefix(p, "/api/") {
			c.JSON(http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		serveShell(context.Background(), c)
	})
}
