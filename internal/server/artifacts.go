package server

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/artifact"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// The console's artifact surface: the resources the agent produced (pages,
// reports, images), which live in a directory of their own on the server rather
// than in any workspace.
//
// Three things are worth knowing when reading it.
//
//   - A row is an index into the files, not the other way round. The store's
//     contents are the truth; Path is what the serving route resolves under the
//     configured root and nowhere else, and a file whose row is gone is simply
//     not listed.
//   - Artifact bytes are authored by the model, and the model is steered by
//     whatever text reached it. They are therefore served with a sandbox CSP and
//     nosniff (see serveArtifactFile): a page the agent wrote must not be able to
//     act as the logged-in console.
//   - Filing is not the workspace's business. An artifact is a deliverable, and
//     the console has to fetch it after the workspace it was made in is gone.
const (
	// artifactsURLPrefix is where artifact files are served from. artifact.URL
	// builds every link from this constant, so the links the model is handed and
	// the route that serves them cannot drift apart.
	artifactsURLPrefix = "/api/artifacts/files"
)

// ArtifactSettings is what the server needs to store and serve artifacts.
type ArtifactSettings struct {
	// Enable turns the routes on. With it off, every endpoint answers
	// `enabled: false` with the reason rather than a 404, which a client cannot
	// tell apart from a broken deployment.
	Enable bool
	// Root is the directory artifacts are written to and read from. Required
	// when Enable is set.
	Root string
	// PublicURLs serves artifact files without a session. Off by default; see
	// config.ArtifactsConfig.PublicURLs for why that default is the safe one.
	PublicURLs bool
	// MaxBytes caps one artifact. Zero uses artifact.DefaultMaxBytes.
	MaxBytes int64
}

// registerArtifactRoutes wires the artifact endpoints.
//
// The listing and deletion routes sit behind the session because they read the
// store's index. The file route is registered on the open group and checks the
// session itself, because whether it requires one is a configuration decision
// (artifacts.public_urls) that a route-level middleware cannot express without
// registering the same path twice.
func (s *Server) registerArtifactRoutes(api, authed *route.RouterGroup) {
	api.GET("/artifacts/files/*path", s.handleArtifactFile)
	authed.GET("/artifacts", s.handleListArtifacts)
	authed.GET("/artifacts/:id", s.handleGetArtifact)
	authed.DELETE("/artifacts/:id", s.handleDeleteArtifact)
}

// artifactsDisabledResponse is what every artifact endpoint says when the feature
// is off. It is one function so the wording cannot vary by endpoint: a client
// showing three different explanations of the same missing feature is a bug.
func artifactsDisabledResponse(c *app.RequestContext) {
	c.JSON(http.StatusOK, map[string]any{
		"ok":      true,
		"enabled": false,
		"message": "产物功能未启用：请设置 tools.artifacts.enable=true 并确保 tools.artifacts.root（或数据库同级目录）可写",
	})
}

// handleListArtifacts lists artifacts: one session's, or every one in the store.
//
// `?session=` narrows it to that conversation, which is what the console's 产物
// drawer asks for; omitting it is what 产物中心 asks for. The two are one endpoint
// because they are one question — "which artifacts" — asked with a narrower
// answer in one case.
func (s *Server) handleListArtifacts(ctx context.Context, c *app.RequestContext) {
	if s.artifacts == nil {
		artifactsDisabledResponse(c)
		return
	}
	sessionID := strings.TrimSpace(c.Query("session"))
	rows, err := s.store.ListArtifacts(ctx, sessionID)
	if err != nil {
		s.fail(c, "list artifacts", err)
		return
	}

	// Titles for 产物中心, which lists every session's artifacts and has to say
	// which conversation each came from. One extra query rather than a join in
	// the store: the artifact store is not the session store's business, and a
	// missing title is answered with the id rather than with a wrong one.
	titles := s.sessionTitles(ctx, rows)

	out := make([]artifactInfo, 0, len(rows))
	for _, a := range rows {
		out = append(out, s.artifactInfo(a, titles[a.SessionID]))
	}
	c.JSON(http.StatusOK, map[string]any{
		"ok":        true,
		"enabled":   true,
		"artifacts": out,
		"count":     len(out),
		// Echoed back so a client can label the drawer without keeping its own
		// copy of what it asked for.
		"session": sessionID,
	})
}

// handleGetArtifact returns one artifact's row.
func (s *Server) handleGetArtifact(ctx context.Context, c *app.RequestContext) {
	if s.artifacts == nil {
		artifactsDisabledResponse(c)
		return
	}
	a, err := s.store.GetArtifact(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "artifact not found"})
			return
		}
		s.fail(c, "get artifact", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"ok":       true,
		"enabled":  true,
		"artifact": s.artifactInfo(a, ""),
	})
}

// handleDeleteArtifact removes one artifact's row and its bytes.
//
// The row goes first: a row without a file is a listing entry that 404s, while a
// file without a row is bytes nothing will ever list or delete. Deleting the
// row is also not reversible, so a file that refuses to go is reported rather
// than turning into a failed request the client would retry against a row that
// is already gone.
func (s *Server) handleDeleteArtifact(ctx context.Context, c *app.RequestContext) {
	if s.artifacts == nil {
		artifactsDisabledResponse(c)
		return
	}
	id := c.Param("id")
	a, err := s.store.GetArtifact(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "artifact not found"})
			return
		}
		s.fail(c, "get artifact", err)
		return
	}
	if err := s.store.DeleteArtifact(ctx, id); err != nil {
		s.fail(c, "delete artifact", err)
		return
	}
	removed := true
	if err := s.artifacts.Remove(a.Path); err != nil {
		removed = false
		s.logger.Warn("artifact row deleted but its file was not removed",
			zapString("id", id), zapString("path", a.Path), zapError(err))
	}
	c.JSON(http.StatusOK, map[string]any{
		"ok":           true,
		"deleted":      id,
		"file_removed": removed,
		"path":         a.Path,
		"session_id":   a.SessionID,
	})
}

// handleArtifactFile serves one artifact's bytes.
//
// It is registered on the open group and asks for a session here, unless the
// deployment opted into public URLs. Even then the file is not served as an
// ordinary same-origin document: the sandbox CSP below puts it in an opaque
// origin, so a page the model wrote cannot read the console's cookie or call its
// API as the logged-in user — which is the difference between "the agent can
// show me a page" and "the agent can hand itself my session".
func (s *Server) handleArtifactFile(_ context.Context, c *app.RequestContext) {
	if s.artifacts == nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": "artifacts are not enabled"})
		return
	}
	if !s.cfg.Artifacts.PublicURLs && !s.auth.allows(c) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// The route's wildcard carries a leading slash. The rest is data, not a
	// path: it is resolved through the artifact store, which refuses anything
	// that would leave its root (absolute paths, traversal, symlink escapes).
	rel := strings.TrimPrefix(c.Param("path"), "/")
	f, size, err := s.artifacts.Open(rel)
	if err != nil {
		if errors.Is(err, artifact.ErrOutsideRoot) {
			s.logger.Warn("artifact path rejected",
				zapString("path", rel), zapError(err))
		}
		c.JSON(http.StatusNotFound, map[string]string{"error": "artifact file is not available"})
		return
	}

	mime := artifactContentType(rel)
	c.Response.Header.Set("Content-Type", mime)
	c.Response.Header.Set("Content-Disposition", contentDisposition(path.Base(rel)))
	// Artifact bytes are chosen by the model, so a browser must not re-sniff
	// them into something else.
	c.Response.Header.Set("X-Content-Type-Options", "nosniff")
	if isHTMLType(mime) {
		c.Response.Header.Set("Content-Security-Policy", artifactSandboxCSP)
	}
	// A saved artifact is immutable: nothing rewrites a stored file, and its URL
	// carries no version, so re-fetching it on every render of the drawer is
	// pure waste. `private` keeps a shared proxy from holding on to it.
	c.Response.Header.Set("Cache-Control", "private, max-age=300")
	// The body is streamed straight from the file, so a 32 MiB artifact is not
	// read into memory to be served. The engine closes the stream once the body
	// is written — which happens after this handler returns — so the file must
	// not be closed here.
	c.SetBodyStream(f, int(size))
}

// artifactSandboxCSP confines a served artifact to an opaque origin.
//
// `sandbox` without `allow-same-origin` is the whole point: the document cannot
// read cookies, localStorage or the console's API, so an HTML artifact cannot
// act as the admin who opened it. The allowances are what keep it useful rather
// than inert — a generated dashboard needs its scripts, its forms and its
// downloads.
const artifactSandboxCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads"

// isHTMLType reports whether a content type is rendered as a document by the
// browser, which is what the sandbox has to cover.
func isHTMLType(mime string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	return base == "text/html" || base == "application/xhtml+xml" || base == "image/svg+xml"
}

// artifactContentType resolves what a stored path is served as.
//
// It answers from the artifact package's table so the bytes a reader receives are
// typed by the same decision that accepted them, and falls back to
// application/octet-stream for anything the table does not know — a file the
// operator dropped into the directory by hand, say.
func artifactContentType(rel string) string {
	if mime, ok := artifact.MIMEForPath(rel); ok {
		return mime
	}
	return "application/octet-stream"
}

// artifactInfo is one artifact as the console sees it: the row, plus the two
// things it needs to render a link and a label.
type artifactInfo struct {
	store.Artifact
	// URL opens the artifact, relative to the console's origin. It is built by
	// the artifact package rather than assembled here, so the links the model is
	// told about and the links the console renders are the same string.
	URL string `json:"url"`
	// SessionTitle names the owning conversation for 产物中心. Empty when the
	// session is unknown (deleted, or a one-shot run that never had one).
	SessionTitle string `json:"session_title,omitempty"`
}

func (s *Server) artifactInfo(a store.Artifact, sessionTitle string) artifactInfo {
	return artifactInfo{
		Artifact:     a,
		URL:          artifact.URL(artifactsURLPrefix, a.Path),
		SessionTitle: sessionTitle,
	}
}

// sessionTitles maps the sessions the given artifacts belong to onto their
// titles. A session that is not found is simply absent from the map: the caller
// falls back to showing the id, which is a true label, rather than an empty one.
func (s *Server) sessionTitles(ctx context.Context, rows []store.Artifact) map[string]string {
	want := make(map[string]bool, len(rows))
	for _, a := range rows {
		if a.SessionID != "" {
			want[a.SessionID] = true
		}
	}
	out := make(map[string]string, len(want))
	if len(want) == 0 {
		return out
	}
	sessions, err := s.store.ListChatSessions(ctx, store.ChatSessionFilter{Limit: 1000})
	if err != nil {
		// A missing title is a cosmetic loss; the artifact list is not.
		s.logger.Warn("artifact list: could not read session titles", zapError(err))
		return out
	}
	for _, sess := range sessions {
		if want[sess.ID] {
			out[sess.ID] = sess.Title
		}
	}
	return out
}

// ------------------------------------------------------------- per-turn saver --

// withTurnArtifacts publishes the artifact store for one turn.
//
// The tool is built once at startup and knows nothing about sessions, so the
// turn publishes what it needs: which session owns what it stores, and where the
// console serves it. A server with no artifact store returns ctx unchanged, which
// is the state the save_artifact tool reports on rather than failing obscurely.
func (s *Server) withTurnArtifacts(ctx context.Context, sessionID string) context.Context {
	if s.artifacts == nil {
		return ctx
	}
	saver, err := artifact.NewSaver(s.artifacts, s.store, artifact.SaverOptions{
		Owner:     sessionID,
		URLPrefix: artifactsURLPrefix,
		Logger:    artifactLogger{s.logger},
	})
	if err != nil {
		s.logger.Warn("artifact store is not usable this turn", zapError(err))
		return ctx
	}
	return tool.WithArtifacts(ctx, saver)
}

// artifactLogger adapts *zap.Logger to the artifact package's interface, so the
// package that lays out the files does not have to depend on zap.
type artifactLogger struct{ l *zap.Logger }

func (a artifactLogger) Warn(msg string, kv ...any) {
	if a.l != nil {
		a.l.Sugar().Warnw(msg, kv...)
	}
}

// newArtifactStore builds the store, or returns nil when the feature is off.
//
// A configured root that cannot be created is an error rather than a warning:
// the feature was asked for, and starting without it would leave save_artifact
// failing for the rest of the process's life.
func newArtifactStore(settings ArtifactSettings) (*artifact.Store, error) {
	if !settings.Enable {
		return nil, nil
	}
	if strings.TrimSpace(settings.Root) == "" {
		return nil, errors.New("server: artifacts are enabled but no root directory was resolved")
	}
	return artifact.New(settings.Root, artifact.Options{MaxBytes: settings.MaxBytes})
}
