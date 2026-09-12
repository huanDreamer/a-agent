package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Media asset kinds. The kind is derived from the sniffed content type when the
// file is uploaded, never from what the client claimed, and it decides how a
// later turn can present the file to the model: an image may be inlined into a
// vision request, audio never can.
const (
	// MediaKindImage is a picture (PNG/JPEG/GIF/WebP).
	MediaKindImage = "image"
	// MediaKindAudio is a recording (MP3/M4A/WAV/WebM/OGG/FLAC).
	MediaKindAudio = "audio"
)

// MediaAsset is one file uploaded for a chat session.
//
// The bytes themselves live in the workspace and are reached through Path, which
// is workspace-relative. The row exists so a message can reference its
// attachment by id and so the file can be served back after a reload.
type MediaAsset struct {
	ID string `json:"id"`
	// SessionID owns the attachment. A message may only reference assets of its
	// own session, which is what stops an id from being replayed into another
	// conversation.
	SessionID string `json:"session_id,omitempty"`
	// Kind is MediaKindImage or MediaKindAudio.
	Kind string `json:"kind"`
	// Path is the file's path relative to the workspace root.
	Path string `json:"path"`
	// MIME is the canonical content type this build accepted, which is what the
	// download endpoint serves back.
	MIME string `json:"mime"`
	// Bytes is the stored file's size.
	Bytes int64 `json:"bytes"`
	// SHA256 is the hex digest of the stored bytes, so an identical re-upload is
	// recognisable instead of being stored twice.
	SHA256    string    `json:"sha256,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// mediaAssetCols is the column list every query shares.
const mediaAssetCols = `id, session_id, kind, path, mime, bytes, sha256, created_at`

// CreateMediaAsset records an uploaded file. The bytes are already on disk by
// the time this is called, so a failure here leaves an orphan file the caller is
// expected to remove.
func (s *sqliteStore) CreateMediaAsset(ctx context.Context, a MediaAsset) error {
	if strings.TrimSpace(a.ID) == "" {
		return errors.New("store: media asset id is required")
	}
	if strings.TrimSpace(a.Path) == "" {
		return errors.New("store: media asset path is required")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO media_assets (id, session_id, kind, path, mime, bytes, sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.SessionID, a.Kind, a.Path, a.MIME, a.Bytes, a.SHA256, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert media asset: %w", err)
	}
	return nil
}

// GetMediaAsset loads one asset by id. It returns ErrNotFound when absent.
func (s *sqliteStore) GetMediaAsset(ctx context.Context, id string) (MediaAsset, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mediaAssetCols+` FROM media_assets WHERE id = ?`, id)
	return scanMediaAsset(row)
}

// ListMediaAssets returns a session's assets, oldest first.
func (s *sqliteStore) ListMediaAssets(ctx context.Context, sessionID string) ([]MediaAsset, error) {
	q := `SELECT ` + mediaAssetCols + ` FROM media_assets`
	var args []any
	if sessionID != "" {
		q += ` WHERE session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY created_at ASC, id ASC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list media assets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]MediaAsset, 0, 8)
	for rows.Next() {
		a, err := scanMediaAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate media assets: %w", err)
	}
	return out, nil
}

// FindMediaAssetBySHA256 returns the most recent asset of a session whose bytes
// hash to digest. It is how an identical re-upload is recognised; a miss is
// ErrNotFound, so the caller stores the file it already has.
func (s *sqliteStore) FindMediaAssetBySHA256(ctx context.Context, sessionID, digest string) (MediaAsset, error) {
	if strings.TrimSpace(digest) == "" {
		return MediaAsset{}, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+mediaAssetCols+` FROM media_assets
		 WHERE session_id = ? AND sha256 = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, sessionID, digest)
	return scanMediaAsset(row)
}

// scanMediaAsset reads one asset row.
func scanMediaAsset(sc rowScanner) (MediaAsset, error) {
	var a MediaAsset
	var created sql.NullTime
	if err := sc.Scan(&a.ID, &a.SessionID, &a.Kind, &a.Path, &a.MIME, &a.Bytes, &a.SHA256, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MediaAsset{}, ErrNotFound
		}
		return MediaAsset{}, fmt.Errorf("store: scan media asset: %w", err)
	}
	a.CreatedAt = nullTime(created)
	return a, nil
}
