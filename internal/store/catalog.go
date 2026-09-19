package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
	// CapTools calls functions/tools (function calling). It is what lets a
	// model drive the agent's whole tool set, so a model without it is a chat
	// partner rather than an agent.
	CapTools Capability = "tools"
	// CapAudioTranscribe turns speech into text.
	CapAudioTranscribe Capability = "audio_transcribe"
	// CapAudioSpeech turns text into speech.
	CapAudioSpeech Capability = "audio_speech"
	// CapEmbedding produces embeddings.
	CapEmbedding Capability = "embedding"
)

// AllCapabilities is the set the UI offers, in display order.
var AllCapabilities = []Capability{
	CapChat, CapVision, CapTools, CapImageGen, CapAudioTranscribe, CapAudioSpeech, CapEmbedding,
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
	// ContextWindow is the model's context window in tokens, 0 when nobody has
	// said. It is what an agent turn sizes its in-loop window from, so it is
	// recorded rather than recomputed: see migration 16.
	ContextWindow int `json:"context_window"`
	// ContextWindowSource says who answered: ModelWindowSourceAPI (the
	// provider's /models listing), ModelWindowSourceAsked (the model itself,
	// asked directly) or ModelWindowSourceUser (typed in the console). Empty
	// when ContextWindow is 0.
	ContextWindowSource string `json:"context_window_source"`
	// ContextWindowCheckedAt is when that answer was recorded, so the console
	// can show how stale it is.
	ContextWindowCheckedAt *time.Time `json:"context_window_checked_at,omitempty"`
	// CapabilitiesSource says where the capability set came from: a provider's
	// listing, the model's own answer, the model-name heuristics, or an operator.
	// See ModelCapabilitySource*.
	CapabilitiesSource string `json:"capabilities_source,omitempty"`
	// CapabilitiesCheckedAt is when a probe last asked. It is what keeps a model
	// that answered "I do not know" from being asked on every pass.
	CapabilitiesCheckedAt *time.Time `json:"capabilities_checked_at,omitempty"`
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

const modelCols = `provider_id, model_id, display_name, capabilities, enabled, source, fetched_at, ` +
	`context_window, context_window_source, context_window_checked_at, capabilities_source, capabilities_checked_at`

// Where a model's context window came from. The distinction is not decoration:
// a number the provider published is authoritative for that deployment, while
// one the model answered about itself is a hint that may be a different
// generation's window.
const (
	// ModelWindowSourceAPI is the provider's own /models listing.
	ModelWindowSourceAPI = "api"
	// ModelWindowSourceAsked is the model's answer to being asked directly.
	ModelWindowSourceAsked = "asked"
	// ModelWindowSourceUser is a value the operator set.
	ModelWindowSourceUser = "user"
)

// Where a model's capability set came from, strongest first.
//
// The order is the order of authority, and it is why a probe never overwrites an
// operator's own toggle: a person who has tested a model knows more about it than
// the model does.
const (
	// ModelCapabilitySourceUser is an operator's decision (the console's chips).
	ModelCapabilitySourceUser = "user"
	// ModelCapabilitySourceAPI is what the provider's listing says, e.g. the
	// endpoints a model is served on.
	ModelCapabilitySourceAPI = "api"
	// ModelCapabilitySourceAsked is the model's answer about itself.
	ModelCapabilitySourceAsked = "asked"
	// ModelCapabilitySourceInferred is the model-name heuristics.
	ModelCapabilitySourceInferred = "inferred"
)

// Automatic capability writes never override an operator's own set.
//
// Between the automatic sources there is deliberately no pecking order. They
// answer *different* questions rather than competing ones: a provider's listing
// says which endpoint a model is served on (that is where "chat" comes from, and
// it is hard evidence), while the model's own answer says what modalities it has
// (vision, tools, audio) — which no endpoint list mentions. Ranking them would
// mean the process of learning anything new stopped the moment the other source
// had said anything at all, and a gateway that lists "/chat/completions" for
// every model would never get a vision flag.
func automaticCapabilityWriteAllowed(stored, incoming string) bool {
	return stored != ModelCapabilitySourceUser || incoming == ModelCapabilitySourceUser
}

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
			VALUES (?, ?, ?, ?, ?, 'fetched', CURRENT_TIMESTAMP, ?, ?, ?, ?, ?)
			ON CONFLICT(provider_id, model_id) DO UPDATE SET
				display_name = CASE WHEN excluded.display_name = '' THEN llm_models.display_name ELSE excluded.display_name END,
				fetched_at   = CURRENT_TIMESTAMP,
				context_window = CASE WHEN excluded.context_window > 0 THEN excluded.context_window ELSE llm_models.context_window END,
				context_window_source = CASE WHEN excluded.context_window > 0 THEN excluded.context_window_source ELSE llm_models.context_window_source END,
				context_window_checked_at = CASE WHEN excluded.context_window > 0 THEN CURRENT_TIMESTAMP ELSE llm_models.context_window_checked_at END
			WHERE llm_models.source = 'fetched'`,
			providerID, m.ModelID, m.DisplayName, m.Capabilities.String(), boolToInt(m.Enabled),
			m.ContextWindow, m.ContextWindowSource, checkedAtValue(m.ContextWindow),
			// A fetch never marks capabilities as *checked*. What a listing says
			// about a model ("it is served on /chat/completions") is a starting
			// point, not an answer: it can say a model chats, and cannot say
			// whether it sees images. Only a probe or an operator finishes that
			// question, and only those may set capabilities_checked_at.
			m.CapabilitiesSource, nil); err != nil {
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
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, model_id) DO UPDATE SET
			display_name = excluded.display_name,
			capabilities = excluded.capabilities,
			enabled      = excluded.enabled,
			source       = CASE WHEN llm_models.source = 'fetched' AND excluded.source = 'fetched' THEN llm_models.source ELSE excluded.source END`,
		m.ProviderID, m.ModelID, m.DisplayName, m.Capabilities.String(), boolToInt(m.Enabled), m.Source,
		m.ContextWindow, m.ContextWindowSource, checkedAtValue(m.ContextWindow),
		m.CapabilitiesSource, nil)
	if err != nil {
		return fmt.Errorf("store: upsert model: %w", err)
	}
	return nil
}

// checkedAtValue is the timestamp to record beside a window: now when there is
// one, and nothing when there is not — a checked_at with no number would make
// the console say "asked, no answer" forever.
func checkedAtValue(tokens int) any {
	if tokens <= 0 {
		return nil
	}
	return time.Now().UTC()
}

// SetModelContextWindow records what a model's context window is, and who said
// so. A zero or negative value clears it, which is what a refresh that can no
// longer get an answer should do — the built-in table is then used instead of a
// stale number.
//
// A number the *provider published* is not overwritten by one a model answered
// about itself, and an operator's value is not overwritten at all. The published
// number is the limit the provider will enforce; a self-report is a hint, and a
// noisy one — a real run had the same model claim 200000 in one pass and 128000
// in the next, and another claim 32768 for a model its gateway publishes as
// 1000000. Filling an empty slot is what a hint is for; taking a filled one is
// how a working 1M window becomes a 32k one.
func (s *sqliteStore) SetModelContextWindow(ctx context.Context, providerID, modelID string, tokens int, source string) error {
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(modelID) == "" {
		return errors.New("store: set model window: provider and model are required")
	}
	if tokens < 0 {
		tokens, source = 0, ""
	}
	if tokens == 0 {
		source = ""
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE llm_models SET context_window = ?, context_window_source = ?, context_window_checked_at = ?
		WHERE provider_id = ? AND model_id = ?
		  AND NOT (? = ? AND context_window_source IN (?, ?))`,
		tokens, source, checkedAtValue(tokens), providerID, modelID,
		source, ModelWindowSourceAsked, ModelWindowSourceAPI, ModelWindowSourceUser)
	if err != nil {
		return fmt.Errorf("store: set model window: %w", err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		// Either there is no such row, or a stronger source already answered.
		// The caller treats both as "nothing to do"; the console shows which.
		if _, gerr := s.GetModel(ctx, providerID, modelID); gerr != nil {
			return gerr
		}
	}
	return nil
}

// ModelContextWindows returns the recorded window of every model that has one,
// keyed by model id.
//
// The key is the model id alone because that is what the agent has at the point
// it needs the number (a session stores the model, not the provider), and a
// model id is effectively unique across the providers a deployment configures.
// When two providers disagree, the first enabled one in a deterministic order
// wins, and context.model_windows is the documented way to settle it.
func (s *sqliteStore) ModelContextWindows(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT model_id, context_window FROM llm_models
		WHERE context_window > 0 AND enabled = 1
		ORDER BY provider_id, model_id`)
	if err != nil {
		return nil, fmt.Errorf("store: model windows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int, 32)
	for rows.Next() {
		var id string
		var window int
		if err := rows.Scan(&id, &window); err != nil {
			return nil, fmt.Errorf("store: scan model window: %w", err)
		}
		if _, seen := out[id]; !seen {
			out[id] = window
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: model windows: %w", err)
	}
	return out, nil
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
	var checkedAt, capsCheckedAt sql.NullTime
	if err := sc.Scan(&m.ProviderID, &m.ModelID, &m.DisplayName, &caps, &enabled, &source, &fetchedAt,
		&m.ContextWindow, &m.ContextWindowSource, &checkedAt, &m.CapabilitiesSource, &capsCheckedAt); err != nil {
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
	if checkedAt.Valid {
		t := checkedAt.Time
		m.ContextWindowCheckedAt = &t
	}
	if capsCheckedAt.Valid {
		t := capsCheckedAt.Time
		m.CapabilitiesCheckedAt = &t
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

// LatestSessionModel returns the provider and model of the most recently used
// conversation that has one, so a new conversation can start where the last one
// left off instead of snapping back to the configured default.
//
// Only non-empty rows are considered: a conversation created without a model
// records nothing, and returning it would answer "nothing" as if it were an
// answer.
func (s *sqliteStore) LatestSessionModel(ctx context.Context) (string, string, bool) {
	var provider, model string
	err := s.db.QueryRowContext(ctx, `
		SELECT provider, model FROM chat_sessions
		WHERE TRIM(provider) <> '' OR TRIM(model) <> ''
		ORDER BY updated_at DESC, rowid DESC
		LIMIT 1`).Scan(&provider, &model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		return "", "", false
	}
	return provider, model, true
}

// SetModelCapabilities records a capability set and where it came from.
//
// It refuses to let a weaker source overwrite a stronger one (see
// ModelCapabilityRank): an operator's decision stands until an operator changes
// it, and a provider's listing stands over a probe. "Overwrite" here is
// deliberately conservative — the caller passes the *whole* set it believes, and
// a refusal leaves the stored one alone.
func (s *sqliteStore) SetModelCapabilities(ctx context.Context, providerID, modelID string,
	caps Capabilities, source string) error {

	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(modelID) == "" {
		return errors.New("store: set model capabilities: provider and model are required")
	}
	// The guard lives in the WHERE clause rather than in Go so it holds under
	// concurrent writers (a refresh and a probe can overlap).
	res, err := s.db.ExecContext(ctx, `
		UPDATE llm_models
		SET capabilities = ?, capabilities_source = ?, capabilities_checked_at = CURRENT_TIMESTAMP
		WHERE provider_id = ? AND model_id = ?
		  AND (? = 'user' OR capabilities_source <> 'user')`,
		caps.String(), source, providerID, modelID, source)
	if err != nil {
		return fmt.Errorf("store: set model capabilities: %w", err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		// Nothing was written: either the row is missing or an operator set this
		// model's capabilities by hand. Both are "leave it as it is", which is
		// what the caller does.
		_ = n
	}
	return nil
}

// MarkModelWindowChecked records that a model was asked about its window and did
// not give one.
//
// It exists so a model that never answers is not asked on every pass — the fill
// retries it after a while (a model that says nothing today may know tomorrow;
// a real pair did exactly that), but a refresh loop should not spend a call per
// model per pass on the same silence.
func (s *sqliteStore) MarkModelWindowChecked(ctx context.Context, providerID, modelID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE llm_models SET context_window_checked_at = CURRENT_TIMESTAMP
		WHERE provider_id = ? AND model_id = ? AND context_window <= 0`, providerID, modelID)
	if err != nil {
		return fmt.Errorf("store: mark window checked: %w", err)
	}
	return nil
}

// MarkModelCapabilitiesChecked records that a model was asked, whether or not it
// answered anything. It is separate from SetModelCapabilities because a model
// that said "I do not know" must not be asked again on every pass, and the fact
// that it was asked is not a capability.
func (s *sqliteStore) MarkModelCapabilitiesChecked(ctx context.Context, providerID, modelID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE llm_models SET capabilities_checked_at = CURRENT_TIMESTAMP
		WHERE provider_id = ? AND model_id = ?`, providerID, modelID)
	if err != nil {
		return fmt.Errorf("store: mark capabilities checked: %w", err)
	}
	return nil
}

// SetModelCapabilitiesFromProbe applies a probe's *definite* answers to the
// stored set, leaving unanswered capabilities exactly as they were.
//
// That is the difference between this and SetModelCapabilities: a probe reports a
// patch (true/false/unknown per capability), and "unknown" must not become
// "false". A model that answers three of the seven fields is believed about
// those three and about nothing else.
func (s *sqliteStore) SetModelCapabilitiesFromProbe(ctx context.Context, providerID, modelID string,
	answers map[Capability]bool, source string) error {

	m, err := s.GetModel(ctx, providerID, modelID)
	if err != nil {
		return err
	}
	if !automaticCapabilityWriteAllowed(m.CapabilitiesSource, source) {
		// An operator has decided what this model can do. A probe does not get to
		// overrule that; the console says where each set came from, so the
		// operator can still change their mind by hand.
		return nil
	}
	next := make(Capabilities, 0, len(m.Capabilities)+len(answers))
	for _, c := range m.Capabilities {
		if v, answered := answers[c]; answered && !v {
			continue
		}
		next = append(next, c)
	}
	for c, v := range answers {
		if !v || next.Contains(c) {
			continue
		}
		next = append(next, c)
	}
	sort.SliceStable(next, func(i, j int) bool {
		return capabilityOrder(next[i]) < capabilityOrder(next[j])
	})
	return s.SetModelCapabilities(ctx, providerID, modelID, next, source)
}

// Contains reports whether the set has a capability.
func (c Capabilities) Contains(want Capability) bool {
	for _, have := range c {
		if have == want {
			return true
		}
	}
	return false
}

// capabilityOrder is the display order of a capability, or a large number for
// anything unknown to this build.
func capabilityOrder(c Capability) int {
	for i, known := range AllCapabilities {
		if known == c {
			return i
		}
	}
	return len(AllCapabilities)
}
