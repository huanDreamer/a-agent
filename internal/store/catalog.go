package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Capability is what a model can do. The OpenAI-compatible /models endpoint
// does not report these — it returns ids and nothing else — so they are
// guessed from the id and then confirmed by the user. Anything relying on them
// must treat them as a hint, not as a guarantee.
type Capability string

const (
	// CapChat is a text chat model; every provider has at least one.
	CapChat Capability = "chat"
	// CapVision accepts images as input.
	CapVision Capability = "vision"
	// CapImageGen produces images from a prompt.
	CapImageGen Capability = "image_gen"
	// CapAudioTranscribe turns speech into text.
	CapAudioTranscribe Capability = "audio_transcribe"
	// CapAudioSpeech turns text into speech.
	CapAudioSpeech Capability = "audio_speech"
	// CapEmbedding produces embeddings.
	CapEmbedding Capability = "embedding"
)

// AllCapabilities is the set the UI offers, in display order.
var AllCapabilities = []Capability{
	CapChat, CapVision, CapImageGen, CapAudioTranscribe, CapAudioSpeech, CapEmbedding,
}

// ValidCapability reports whether c is a capability this build understands.
func ValidCapability(c Capability) bool {
	for _, known := range AllCapabilities {
		if c == known {
			return true
		}
	}
	return false
}

// Capabilities is a set of capabilities, stored as a comma-separated string.
type Capabilities []Capability

// ParseCapabilities decodes the stored form, dropping anything unknown.
func ParseCapabilities(s string) Capabilities {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	seen := map[Capability]bool{}
	var out Capabilities
	for _, part := range strings.Split(s, ",") {
		c := Capability(strings.TrimSpace(part))
		if c == "" || seen[c] || !ValidCapability(c) {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// String encodes the set for storage. Order follows AllCapabilities so the
// stored value is stable and diffs are readable.
func (c Capabilities) String() string {
	if len(c) == 0 {
		return ""
	}
	set := map[Capability]bool{}
	for _, x := range c {
		set[x] = true
	}
	var parts []string
	for _, known := range AllCapabilities {
		if set[known] {
			parts = append(parts, string(known))
		}
	}
	return strings.Join(parts, ",")
}

// Has reports whether the set contains want.
func (c Capabilities) Has(want Capability) bool {
	for _, x := range c {
		if x == want {
			return true
		}
	}
	return false
}

// ProviderSource distinguishes a provider the operator declared in the config
// from one added through the UI.
type ProviderSource string

const (
	// SourceConfig is re-synced from the config file at every start, so editing
	// it in the UI would be overwritten; the UI shows it as read-only.
	SourceConfig ProviderSource = "config"
	// SourceUser is owned by the UI.
	SourceUser ProviderSource = "user"
)

// Provider is a configured LLM endpoint.
type Provider struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	BaseURL string         `json:"base_url"`
	Kind    string         `json:"kind"`
	Source  ProviderSource `json:"source"`
	Enabled bool           `json:"enabled"`
	// APIKeyEnv names an environment variable holding the key. It is preferred
	// over APIKey, because then the secret never lands in the database.
	APIKeyEnv string `json:"api_key_env"`
	// HasAPIKey reports whether a key is available, without revealing it. The
	// key itself is never serialised to a client.
	HasAPIKey bool `json:"has_api_key"`
	// APIKeyHint is a masked tail such as "…ab12", for recognising which key is
	// in use. Never the whole key.
	APIKeyHint string `json:"api_key_hint"`
	// LastError is the most recent connection failure, so the UI can show why a
	// provider is broken without the user re-testing it.
	LastError string    `json:"last_error"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// apiKey carries the secret between the store and the caller that needs to
	// sign a request. It is unexported so it cannot be serialised by accident:
	// a JSON encoder silently skips it, and any new field must be opted in.
	apiKey string
}

// ResolveAPIKey returns the key to sign requests with, preferring the
// environment reference so a key in the environment always wins over a stale
// copy in the database.
func (p Provider) ResolveAPIKey(lookupEnv func(string) string) string {
	if p.APIKeyEnv != "" && lookupEnv != nil {
		if v := strings.TrimSpace(lookupEnv(p.APIKeyEnv)); v != "" {
			return v
		}
	}
	return p.apiKey
}

// Model is one model offered by a provider.
type Model struct {
	ProviderID   string       `json:"provider_id"`
	ModelID      string       `json:"model_id"`
	DisplayName  string       `json:"display_name"`
	Capabilities Capabilities `json:"capabilities"`
	Enabled      bool         `json:"enabled"`
	// Source is "fetched" for one discovered from the provider's /models
	// endpoint, or "user" for one added by hand (a provider that lists nothing,
	// or a model it omits).
	Source    string     `json:"source"`
	FetchedAt *time.Time `json:"fetched_at"`
}

// Has reports whether the model declares a capability.
func (m Model) Has(c Capability) bool { return m.Capabilities.Has(c) }

// Binding names the model a capability should use.
type Binding struct {
	Capability Capability `json:"capability"`
	ProviderID string     `json:"provider_id"`
	ModelID    string     `json:"model_id"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ---------------------------------------------------------------- providers --

const providerCols = `id, name, base_url, api_key, api_key_env, kind, source, enabled, last_error, created_at, updated_at`

// UpsertProvider inserts or updates a provider. A blank api_key leaves any
// stored key untouched, so the UI can save other fields without re-sending the
// secret it was never given.
func (s *sqliteStore) UpsertProvider(ctx context.Context, p Provider) error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("store: provider id is required")
	}
	if p.Source == "" {
		p.Source = SourceUser
	}
	if p.Kind == "" {
		p.Kind = "openai"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO llm_providers (`+providerCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			name        = excluded.name,
			base_url    = excluded.base_url,
			api_key_env = excluded.api_key_env,
			kind        = excluded.kind,
			source      = excluded.source,
			enabled     = excluded.enabled,
			api_key     = CASE WHEN excluded.api_key = '' THEN llm_providers.api_key ELSE excluded.api_key END,
			updated_at  = CURRENT_TIMESTAMP`,
		p.ID, p.Name, p.BaseURL, p.apiKey, p.APIKeyEnv, p.Kind, string(p.Source), boolToInt(p.Enabled), p.LastError)
	if err != nil {
		return fmt.Errorf("store: upsert provider: %w", err)
	}
	return nil
}

// GetProvider loads one provider by id.
func (s *sqliteStore) GetProvider(ctx context.Context, id string) (Provider, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+providerCols+` FROM llm_providers WHERE id = ?`, id)
	return scanProvider(row)
}

// ListProviders returns every provider, config-sourced first then by name.
func (s *sqliteStore) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+providerCols+` FROM llm_providers ORDER BY source ASC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list providers: %w", err)
	}
	defer rows.Close()

	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProviderKey stores an API key. An empty key clears it, which is how the UI
// removes a secret it can no longer show.
func (s *sqliteStore) SetProviderKey(ctx context.Context, id, key string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE llm_providers SET api_key = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, key, id)
	if err != nil {
		return fmt.Errorf("store: set provider key: %w", err)
	}
	// Zero rows means the provider does not exist; reporting success would let
	// the UI believe it saved a key that went nowhere.
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: provider %q: %w", id, ErrNotFound)
	}
	return nil
}

// SetProviderError records the outcome of the last connection attempt. It is
// not part of the editable fields, so it has its own small setter.
func (s *sqliteStore) SetProviderError(ctx context.Context, id, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE llm_providers SET last_error = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, msg, id)
	if err != nil {
		return fmt.Errorf("store: set provider error: %w", err)
	}
	return nil
}

// DeleteProvider removes a provider with its models, bindings and key.
func (s *sqliteStore) DeleteProvider(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete provider: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, q := range []string{
		`DELETE FROM llm_models WHERE provider_id = ?`,
		`DELETE FROM llm_bindings WHERE provider_id = ?`,
		`DELETE FROM llm_providers WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("store: delete provider: %w", err)
		}
	}
	return tx.Commit()
}

// ------------------------------------------------------------------- models --

const modelCols = `provider_id, model_id, display_name, capabilities, enabled, source, fetched_at`

// ReplaceFetchedModels records the result of a /models call. Models the user
// added or edited by hand are preserved: a refresh must not silently discard
// the capability flags the user set, nor a model the provider's list omits.
func (s *sqliteStore) ReplaceFetchedModels(ctx context.Context, providerID string, models []Model) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace models: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, m := range models {
		if strings.TrimSpace(m.ModelID) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO llm_models (`+modelCols+`)
			VALUES (?, ?, ?, ?, ?, 'fetched', CURRENT_TIMESTAMP)
			ON CONFLICT(provider_id, model_id) DO UPDATE SET
				display_name = CASE WHEN excluded.display_name = '' THEN llm_models.display_name ELSE excluded.display_name END,
				fetched_at   = CURRENT_TIMESTAMP
			WHERE llm_models.source = 'fetched'`,
			providerID, m.ModelID, m.DisplayName, m.Capabilities.String(), boolToInt(m.Enabled)); err != nil {
			return fmt.Errorf("store: upsert fetched model: %w", err)
		}
	}
	return tx.Commit()
}

// UpsertModel inserts or updates a single model, including its capabilities.
func (s *sqliteStore) UpsertModel(ctx context.Context, m Model) error {
	if strings.TrimSpace(m.ProviderID) == "" || strings.TrimSpace(m.ModelID) == "" {
		return fmt.Errorf("store: model needs a provider and an id")
	}
	if m.Source == "" {
		m.Source = "user"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO llm_models (`+modelCols+`)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(provider_id, model_id) DO UPDATE SET
			display_name = excluded.display_name,
			capabilities = excluded.capabilities,
			enabled      = excluded.enabled,
			source       = CASE WHEN llm_models.source = 'fetched' AND excluded.source = 'fetched' THEN llm_models.source ELSE excluded.source END`,
		m.ProviderID, m.ModelID, m.DisplayName, m.Capabilities.String(), boolToInt(m.Enabled), m.Source)
	if err != nil {
		return fmt.Errorf("store: upsert model: %w", err)
	}
	return nil
}

// ListModels returns a provider's models; an empty providerID lists all.
func (s *sqliteStore) ListModels(ctx context.Context, providerID string) ([]Model, error) {
	q := `SELECT ` + modelCols + ` FROM llm_models`
	var args []any
	if providerID != "" {
		q += ` WHERE provider_id = ?`
		args = append(args, providerID)
	}
	q += ` ORDER BY provider_id, model_id`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list models: %w", err)
	}
	defer rows.Close()

	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetModel loads one model row.
func (s *sqliteStore) GetModel(ctx context.Context, providerID, modelID string) (Model, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+modelCols+` FROM llm_models WHERE provider_id = ? AND model_id = ?`, providerID, modelID)
	return scanModel(row)
}

// DeleteModel removes one model and any binding that pointed at it, so a
// capability cannot be left bound to a model that no longer exists.
func (s *sqliteStore) DeleteModel(ctx context.Context, providerID, modelID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete model: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM llm_models WHERE provider_id = ? AND model_id = ?`, providerID, modelID); err != nil {
		return fmt.Errorf("store: delete model: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM llm_bindings WHERE provider_id = ? AND model_id = ?`, providerID, modelID); err != nil {
		return fmt.Errorf("store: delete model: %w", err)
	}
	return tx.Commit()
}

// ----------------------------------------------------------------- bindings --

// SetBinding points a capability at a model. A blank model clears the binding,
// which is how a tool is switched off.
func (s *sqliteStore) SetBinding(ctx context.Context, b Binding) error {
	if strings.TrimSpace(b.ModelID) == "" || strings.TrimSpace(b.ProviderID) == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM llm_bindings WHERE capability = ?`, string(b.Capability))
		if err != nil {
			return fmt.Errorf("store: clear binding: %w", err)
		}
		return nil
	}
	if !ValidCapability(b.Capability) {
		return fmt.Errorf("store: unknown capability %q", b.Capability)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO llm_bindings (capability, provider_id, model_id, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(capability) DO UPDATE SET
			provider_id = excluded.provider_id,
			model_id    = excluded.model_id,
			updated_at  = CURRENT_TIMESTAMP`,
		string(b.Capability), b.ProviderID, b.ModelID)
	if err != nil {
		return fmt.Errorf("store: set binding: %w", err)
	}
	return nil
}

// ListBindings returns every capability binding.
func (s *sqliteStore) ListBindings(ctx context.Context) ([]Binding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT capability, provider_id, model_id, updated_at FROM llm_bindings ORDER BY capability`)
	if err != nil {
		return nil, fmt.Errorf("store: list bindings: %w", err)
	}
	defer rows.Close()

	var out []Binding
	for rows.Next() {
		var b Binding
		var cap string
		if err := rows.Scan(&cap, &b.ProviderID, &b.ModelID, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan binding: %w", err)
		}
		b.Capability = Capability(cap)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ helpers --

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanProvider(sc rowScanner) (Provider, error) {
	var p Provider
	var key, source string
	var enabled int
	if err := sc.Scan(&p.ID, &p.Name, &p.BaseURL, &key, &p.APIKeyEnv, &p.Kind, &source,
		&enabled, &p.LastError, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Provider{}, ErrNotFound
		}
		return Provider{}, fmt.Errorf("store: scan provider: %w", err)
	}
	p.apiKey = key
	p.HasAPIKey = strings.TrimSpace(key) != "" || strings.TrimSpace(p.APIKeyEnv) != ""
	p.APIKeyHint = maskKey(key)
	p.Source = ProviderSource(source)
	p.Enabled = enabled != 0
	return p, nil
}

func scanModel(sc rowScanner) (Model, error) {
	var m Model
	var caps, source string
	var enabled int
	var fetchedAt sql.NullTime
	if err := sc.Scan(&m.ProviderID, &m.ModelID, &m.DisplayName, &caps, &enabled, &source, &fetchedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Model{}, ErrNotFound
		}
		return Model{}, fmt.Errorf("store: scan model: %w", err)
	}
	m.Capabilities = ParseCapabilities(caps)
	m.Enabled = enabled != 0
	m.Source = source
	if fetchedAt.Valid {
		t := fetchedAt.Time
		m.FetchedAt = &t
	}
	return m, nil
}

// maskKey renders a recognisable but non-recoverable hint. Showing the last
// few characters lets an operator tell two keys apart without exposing either.
func maskKey(key string) string {
	k := strings.TrimSpace(key)
	if k == "" {
		return ""
	}
	if len(k) <= 4 {
		return "…"
	}
	return "…" + k[len(k)-4:]
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
