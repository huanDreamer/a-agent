package claudecode

import (
	"strconv"
	"strings"

	"github.com/huan/huan-agent/internal/llm"
)

// The environment variables Claude Code reads to decide where a session runs
// and on which model. They are named here rather than spelled out at each use
// because the projection below is the one place that decides the precedence,
// and a second literal elsewhere would be a second precedence.
const (
	EnvBaseURL           = "ANTHROPIC_BASE_URL"
	EnvAuthToken         = "ANTHROPIC_AUTH_TOKEN"
	EnvAPIKey            = "ANTHROPIC_API_KEY"
	EnvModel             = "ANTHROPIC_MODEL"
	EnvDefaultOpusModel  = "ANTHROPIC_DEFAULT_OPUS_MODEL"
	EnvDefaultSonnet     = "ANTHROPIC_DEFAULT_SONNET_MODEL"
	EnvDefaultHaikuModel = "ANTHROPIC_DEFAULT_HAIKU_MODEL"
	EnvSmallFastModel    = "ANTHROPIC_SMALL_FAST_MODEL"
	EnvSubagentModel     = "CLAUDE_CODE_SUBAGENT_MODEL"
	EnvEffort            = "CLAUDE_CODE_EFFORT_LEVEL"
	EnvAutoCompactWindow = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	EnvMaxOutputTokens   = "CLAUDE_CODE_MAX_OUTPUT_TOKENS"
)

// DefaultBaseURL is the endpoint Claude Code uses when ANTHROPIC_BASE_URL is
// unset.
const DefaultBaseURL = "https://api.anthropic.com"

// ModelConfig is the `env` block of the settings file, projected into the
// model configuration this agent runs on.
//
// It mirrors Claude Code's own precedence rather than inventing one, because
// the point of the mode is that a conversation here behaves like a session
// there: ANTHROPIC_MODEL names the model, the DEFAULT_*_MODEL variables name
// the tiers, and the small/fast variable is the tier-3 fallback whose name
// changed between Claude Code versions (both spellings are honoured).
//
// Token is the credential in the clear. It is unexported-adjacent by
// convention — never marshalled, only ever read by the request signer and
// rendered through MaskedToken — because this struct is also what the console
// serialises.
type ModelConfig struct {
	BaseURL   string `json:"base_url"`
	AuthStyle string `json:"auth_style"`
	// AuthHeader is the header name the credential rides in, for display.
	AuthHeader string `json:"auth_header"`
	// Token is the resolved credential, in the clear.
	Token string `json:"-"`
	// MaskedToken is all a client ever sees of it.
	MaskedToken string `json:"token_masked"`
	HasToken    bool   `json:"has_token"`

	Model         string `json:"model"`
	OpusModel     string `json:"opus_model"`
	SonnetModel   string `json:"sonnet_model"`
	HaikuModel    string `json:"haiku_model"`
	SubagentModel string `json:"subagent_model"`

	Effort            string `json:"effort"`
	AutoCompactWindow string `json:"auto_compact_window"`
	MaxOutputTokens   int    `json:"max_output_tokens"`

	// EffectiveModel is what a turn actually runs on, after the precedence.
	EffectiveModel string `json:"effective_model"`
	// Kind is the wire protocol this endpoint speaks. The mode is the reason the
	// agent has an Anthropic Messages adapter at all: every ANTHROPIC_BASE_URL
	// in the wild is an Anthropic-shaped endpoint, whatever it proxies to.
	Kind string `json:"kind"`
	// Ready reports whether a conversation could actually run on this. Problem
	// says why not, in the operator's words.
	Ready   bool   `json:"ready"`
	Problem string `json:"problem"`
}

// Model projects the settings file's env block into a model configuration.
//
// The precedence is Claude Code's: ANTHROPIC_MODEL wins, then the Sonnet tier,
// then Opus, then Haiku. Sonnet before Opus is not a mistake — it is the tier
// Claude Code itself falls back to when nothing names a model, and a machine
// that sets only ANTHROPIC_DEFAULT_SONNET_MODEL (the common way to pin a
// proxied deployment) must land on it rather than on a tier nobody chose.
func (s Settings) Model() ModelConfig {
	m := ModelConfig{
		BaseURL:           strings.TrimRight(s.Get(EnvBaseURL), "/"),
		Model:             s.Get(EnvModel),
		OpusModel:         s.Get(EnvDefaultOpusModel),
		SonnetModel:       s.Get(EnvDefaultSonnet),
		HaikuModel:        s.Get(EnvDefaultHaikuModel),
		Effort:            s.Get(EnvEffort),
		AutoCompactWindow: s.Get(EnvAutoCompactWindow),
		Kind:              llm.KindAnthropicMessages,
	}

	// The credential: an auth token is a bearer token, an API key is an x-api-key
	// header. That is exactly the distinction Claude Code makes, and it matters —
	// a gateway that expects one usually rejects the other, and the failure looks
	// like an invalid key rather than a wrong header.
	if token := s.Get(EnvAuthToken); token != "" {
		m.Token = token
		m.AuthStyle = llm.AuthStyleBearer
		m.AuthHeader = "Authorization: Bearer"
	} else if key := s.Get(EnvAPIKey); key != "" {
		m.Token = key
		m.AuthStyle = llm.AuthStyleAPIKey
		m.AuthHeader = "x-api-key"
	}
	m.HasToken = m.Token != ""
	m.MaskedToken = Mask(m.Token)

	if s.Get(EnvSmallFastModel) != "" && m.HaikuModel == "" {
		m.HaikuModel = s.Get(EnvSmallFastModel)
	}
	// The subagent model is the Haiku tier unless it says otherwise, which is
	// how Claude Code delegates: cheap and fast for the nested work.
	m.SubagentModel = s.Get(EnvSubagentModel)
	if m.SubagentModel == "" {
		m.SubagentModel = m.HaikuModel
	}

	if n, err := strconv.Atoi(s.Get(EnvMaxOutputTokens)); err == nil && n > 0 {
		m.MaxOutputTokens = n
	}

	m.EffectiveModel = firstNonEmpty(m.Model, m.SonnetModel, m.OpusModel, m.HaikuModel)
	if m.BaseURL == "" {
		m.BaseURL = DefaultBaseURL
	}
	m.Ready, m.Problem = m.readiness()
	return m
}

// readiness says whether a conversation could run on this configuration, and
// why not when it could not.
//
// The three checks are the three things a request needs. They are reported one
// at a time, in the order a request would fail, so the console's message is the
// first thing to fix rather than a list to work through.
func (m ModelConfig) readiness() (bool, string) {
	switch {
	case m.BaseURL == "":
		return false, "settings.json 里没有 " + EnvBaseURL + "，无法确定模型端点"
	case m.Token == "":
		return false, "settings.json 里既没有 " + EnvAuthToken + " 也没有 " + EnvAPIKey + "，无法鉴权"
	case m.EffectiveModel == "":
		return false, "settings.json 里没有 " + EnvModel + " 或任何 ANTHROPIC_DEFAULT_*_MODEL，无法确定模型"
	}
	return true, ""
}

// Provider builds the llm.Provider this mode runs on.
func (m ModelConfig) LLMProvider() llm.Provider {
	return llm.Provider{
		Name:            ProviderID,
		BaseURL:         m.BaseURL,
		APIKey:          m.Token,
		Model:           m.EffectiveModel,
		Kind:            llm.KindAnthropicMessages,
		AuthStyle:       m.AuthStyle,
		MaxOutputTokens: m.MaxOutputTokens,
	}
}

// Mask renders a secret as something safe to display: enough of both ends to
// recognise which key it is, and never enough to use.
//
// A short value is masked whole rather than partly revealed — a 12-character
// token that shows six characters is half a credential, and the operator who
// needs to tell two keys apart can tell two fully-masked ones apart by their
// length and the last character.
func Mask(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= 12 {
		return strings.Repeat("•", len(secret))
	}
	head, tail := secret[:6], secret[len(secret)-4:]
	return head + "…" + tail
}

// firstNonEmpty returns the first non-empty value.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
