package tool

import (
	"context"
	"io"
)

// Where a produced resource goes.
//
// The vocabulary lives here, next to the Tool interface, for the reason the
// Planner's does (see plan.go): two packages have to agree on it and neither owns
// the other. The `save_artifact` tool (internal/tool/builtin) is built once, at
// startup, and is told nothing about where artifacts live — while the store that
// actually writes them (internal/artifact), the row that indexes them (the
// database) and the session that owns them are all known only to whoever owns the
// turn (internal/server). So the turn publishes a Saver on its context and the
// tool asks for it, exactly as plan_* asks for a Planner.
//
// A surface with no saver is a surface with no artifact store configured. The tool
// is registered anyway (it is a property of the process, not of one turn) and
// answers with a message the model can act on rather than failing obscurely.
type ArtifactSaver interface {
	// SaveArtifact stores one produced resource and returns where it can be
	// read — a URL path the console serves, which is the whole point of the
	// feature: a page the agent built has to be openable, not just saved.
	//
	// The returned URL is relative to the console's origin (e.g.
	// /api/artifacts/files/<session>/<file>.html). An empty URL with a nil error
	// means the bytes were stored but no console is serving them, which the tool
	// reports honestly instead of inventing an address.
	SaveArtifact(ctx context.Context, in ArtifactInput) (ArtifactResult, error)
}

// ArtifactInput is what the model asked to save.
//
// Content is a reader rather than a string because an artifact can be large
// (a generated page with inlined images) and the cap that bounds it is enforced
// while reading, not after the whole thing is in memory.
type ArtifactInput struct {
	// Title is the human label: what this artifact is, in the author's words.
	Title string
	// Content is the bytes to store. Required.
	Content io.Reader
	// Name suggests a file name; its extension decides the stored type. Empty
	// lets the kind (or the default) decide.
	Name string
	// Kind is "html", "document", "image" or "other". Empty is inferred.
	Kind string
	// Source records what produced it, for 产物中心 to show.
	Source string
}

// ArtifactResult is where a saved artifact ended up.
type ArtifactResult struct {
	// URL is how the artifact is opened, relative to the console's origin.
	URL string
	// Path is the artifact's location relative to the artifact store's root.
	Path string
	// Bytes is the stored size.
	Bytes int64
	// MIME is the content type it is served as.
	MIME string
}

type artifactsKey struct{}

// WithArtifacts returns ctx carrying s, the artifact store for this turn.
//
// A nil Saver returns ctx unchanged: "this process has no artifact store" is the
// absence of the value, not a value every caller has to nil-check.
func WithArtifacts(ctx context.Context, s ArtifactSaver) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, artifactsKey{}, s)
}

// ArtifactsFrom reads the artifact saver the current turn published, if any.
func ArtifactsFrom(ctx context.Context) (ArtifactSaver, bool) {
	if ctx == nil {
		return nil, false
	}
	s, ok := ctx.Value(artifactsKey{}).(ArtifactSaver)
	return s, ok && s != nil
}
