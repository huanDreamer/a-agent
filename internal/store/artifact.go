package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Artifact kinds. The kind is a label for the console's icon and grouping, not a
// security boundary: what a file may be served as is decided by its recorded MIME
// and its extension (see internal/artifact), never by this column.
const (
	// ArtifactKindHTML is a page the agent generated (an HTML report, a demo).
	ArtifactKindHTML = "html"
	// ArtifactKindDocument is prose: Markdown, plain text, CSV, JSON.
	ArtifactKindDocument = "document"
	// ArtifactKindImage is a picture.
	ArtifactKindImage = "image"
	// ArtifactKindOther is anything that fitted no other row.
	ArtifactKindOther = "other"
)

// Artifact is one resource the agent produced while working.
//
// It is deliberately not a MediaAsset. An attachment is something a person
// uploaded, its bytes live in the workspace, and the workspace is what makes it
// reachable. An artifact is something the agent produced, its bytes live in the
// artifact store on the server (artifacts.root), and Path is what addresses it:
// the serving route resolves exactly this value under that root and nowhere else.
type Artifact struct {
	ID string `json:"id"`
	// SessionID owns the artifact, which is what 会话的产物 lists. It is empty
	// for an artifact produced by a surface with no session (a one-shot
	// `huan-agent run`); such a row still belongs in 产物中心, because "who made
	// it" being unknown is not a reason to lose it.
	SessionID string `json:"session_id,omitempty"`
	// Title is the human label the console shows: what this artifact says it is.
	Title string `json:"title,omitempty"`
	// Kind is one of the ArtifactKind values.
	Kind string `json:"kind"`
	// Path is the artifact's location relative to the store root. It is both the
	// address (the serving route takes exactly this string) and the identity
	// (the store's contents are the truth; the row is an index into them).
	Path string `json:"path"`
	// MIME is the content type the serving endpoint answers with.
	MIME string `json:"mime"`
	// Bytes is the stored file's size.
	Bytes int64 `json:"bytes"`
	// Source is free text recording what produced it, e.g. "save_artifact" or a
	// tool name. It is for a reader of 产物中心, not for control flow.
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// artifactCols is the column list every query shares.
const artifactCols = `id, session_id, title, kind, path, mime, bytes, source, created_at`

// CreateArtifact records a produced file. The bytes are already on disk by the
// time this is called, so a failure here leaves an orphan file the caller is
// expected to remove.
func (s *sqliteStore) CreateArtifact(ctx context.Context, a Artifact) error {
	if strings.TrimSpace(a.ID) == "" {
		return errors.New("store: artifact id is required")
	}
	if strings.TrimSpace(a.Path) == "" {
		return errors.New("store: artifact path is required")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artifacts (id, session_id, title, kind, path, mime, bytes, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.SessionID, a.Title, a.Kind, a.Path, a.MIME, a.Bytes, a.Source, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert artifact: %w", err)
	}
	return nil
}

// GetArtifact loads one artifact by id. It returns ErrNotFound when absent.
func (s *sqliteStore) GetArtifact(ctx context.Context, id string) (Artifact, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id = ?`, id)
	return scanArtifact(row)
}

// ListArtifacts returns artifacts newest first: a session's own when sessionID
// is set, and every one in the store when it is empty.
//
// Newest first rather than oldest, unlike ListMediaAssets: an attachment list is
// the files a message refers to and reads in the order they were sent, while this
// is a gallery, and the thing just produced is what the reader came for.
func (s *sqliteStore) ListArtifacts(ctx context.Context, sessionID string) ([]Artifact, error) {
	q := `SELECT ` + artifactCols + ` FROM artifacts`
	var args []any
	if strings.TrimSpace(sessionID) != "" {
		q += ` WHERE session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY created_at DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Artifact, 0, 8)
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artifacts: %w", err)
	}
	return out, nil
}

// DeleteArtifact removes one artifact's row. It returns ErrNotFound when the id
// is unknown, so deleting twice is reported rather than silently succeeding.
//
// The bytes are the caller's business: this package does not know the store root
// (see internal/artifact for why), so the file is removed by whoever owns both.
func (s *sqliteStore) DeleteArtifact(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete artifact: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete artifact: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanArtifact reads one artifact row.
func scanArtifact(sc rowScanner) (Artifact, error) {
	var a Artifact
	var created sql.NullTime
	if err := sc.Scan(&a.ID, &a.SessionID, &a.Title, &a.Kind, &a.Path,
		&a.MIME, &a.Bytes, &a.Source, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Artifact{}, ErrNotFound
		}
		return Artifact{}, fmt.Errorf("store: scan artifact: %w", err)
	}
	a.CreatedAt = nullTime(created)
	return a, nil
}
