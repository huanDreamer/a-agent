package artifact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// Saver stores one turn's produced resources: the bytes through a Store, the row
// through the database, and it hands back the URL the console serves.
//
// It is the only implementation of tool.ArtifactSaver, and it lives here rather
// than in internal/server because two surfaces produce artifacts and must file
// them identically: the web console (per turn, under a chat session) and the CLI
// (per REPL, under whatever session id that process minted). A second
// implementation would be a second set of rules about name collisions, orphaned
// files and URLs.
//
// It is built per owner rather than once, because the session an artifact belongs
// to is what prefixes its path.
type Saver struct {
	files *Store
	rows  store.Store
	// owner is the session id — or any opaque token — the artifacts are filed
	// under. Empty files them at the store's root, which is what a one-shot run
	// with no session id produces.
	owner string
	// urlPrefix is where the console serves artifact files. Empty means no
	// console is serving them, and the result carries no URL (see
	// tool.ArtifactResult.URL).
	urlPrefix string
	logger    Logger
}

// Logger is the little bit of logging this package needs. It is an interface so
// the package does not depend on zap, and so a test can stay silent.
type Logger interface {
	Warn(msg string, kv ...any)
}

// SaverOptions configures a Saver.
type SaverOptions struct {
	// Owner is the session (or run) the artifacts belong to.
	Owner string
	// URLPrefix is where the console serves artifact files, e.g.
	// "/api/artifacts/files". Empty disables URL reporting.
	URLPrefix string
	// Logger is optional.
	Logger Logger
}

// NewSaver returns the saver for one owner.
func NewSaver(files *Store, rows store.Store, opts SaverOptions) (*Saver, error) {
	if files == nil {
		return nil, ErrDisabled
	}
	if rows == nil {
		return nil, errors.New("artifact: a database is required to index artifacts")
	}
	return &Saver{
		files:     files,
		rows:      rows,
		owner:     strings.TrimSpace(opts.Owner),
		urlPrefix: strings.TrimSpace(opts.URLPrefix),
		logger:    opts.Logger,
	}, nil
}

// SaveArtifact stores one resource the agent produced and returns where it can be
// read.
//
// The bytes go down first and the row second, and the failure handling follows
// from that order: a file with no row is bytes nothing lists (harmless, and
// visible to an operator with `ls`), while a row with no file is a listing entry
// that 404s. So a failed insert removes the file it just wrote rather than
// leaving a phantom behind.
func (s *Saver) SaveArtifact(ctx context.Context, in tool.ArtifactInput) (tool.ArtifactResult, error) {
	if s == nil {
		return tool.ArtifactResult{}, ErrDisabled
	}
	res, err := s.files.Save(SaveInput{
		Title:     in.Title,
		SessionID: s.owner,
		Name:      in.Name,
		Kind:      in.Kind,
	}, in.Content)
	if err != nil {
		return tool.ArtifactResult{}, err
	}

	row := store.Artifact{
		ID:        uuid.NewString(),
		SessionID: s.owner,
		Title:     truncateRunes(strings.TrimSpace(in.Title), maxTitle),
		Kind:      res.Kind,
		Path:      res.Path,
		MIME:      res.MIME,
		Bytes:     res.Bytes,
		Source:    in.Source,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.rows.CreateArtifact(ctx, row); err != nil {
		s.warn("artifact stored but not indexed; removing the file",
			"path", res.Path, "error", err)
		if rerr := s.files.Remove(res.Path); rerr != nil {
			s.warn("artifact cleanup failed", "path", res.Path, "error", rerr)
		}
		return tool.ArtifactResult{}, fmt.Errorf("index artifact: %w", err)
	}

	out := tool.ArtifactResult{
		Path:  res.Path,
		Bytes: res.Bytes,
		MIME:  res.MIME,
	}
	if s.urlPrefix != "" {
		out.URL = URL(s.urlPrefix, res.Path)
	}
	return out, nil
}

func (s *Saver) warn(msg string, kv ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Warn(msg, kv...)
}
