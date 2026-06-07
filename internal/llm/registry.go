package llm

import (
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/model"
)

// Known defaults for built-in providers. The user can override any of these
// in config — the table exists so that `huan-agent chat --provider deepseek`
// works without a fully specified `llm.providers.deepseek` block.
var knownProviders = map[string]Provider{
	"deepseek": {Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
	"qwen":     {Name: "qwen", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "qwen-plus"},
	"glm":      {Name: "glm", BaseURL: "https://open.bigmodel.cn/api/paas/v4", Model: "glm-4-plus"},
	"openai":   {Name: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini"},
	"ollama":   {Name: "ollama", BaseURL: "http://localhost:11434/v1", Model: "llama3.2"},
}

// Registry holds a list of configured providers, keyed by name.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	defaultN  string
}

// NewRegistry builds a registry from the configured providers. Entries that
// match a known default name will inherit BaseURL/Model defaults if those
// fields are empty in the user config.
func NewRegistry(cfg map[string]Provider, defaultName string) *Registry {
	r := &Registry{
		providers: make(map[string]Provider, len(cfg)),
		defaultN:  defaultName,
	}
	for name, p := range cfg {
		if p.Name == "" {
			p.Name = name
		}
		if base, ok := knownProviders[name]; ok {
			if p.BaseURL == "" {
				p.BaseURL = base.BaseURL
			}
			if p.Model == "" {
				p.Model = base.Model
			}
		}
		r.providers[p.Name] = p
	}
	if defaultName == "" {
		// fall back to the first one we know
		for name := range r.providers {
			r.defaultN = name
			break
		}
	}
	return r
}

// Names returns the names of all configured providers in unspecified order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	return out
}

// Get returns the provider with the given name.
func (r *Registry) Get(name string) (model.BaseChatModel, error) {
	r.mu.RLock()
	p, ok := r.providers[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: provider %q is not configured (known: %v)", name, r.Names())
	}
	return New(p)
}

// Default returns the default provider as configured.
func (r *Registry) Default() (model.BaseChatModel, error) {
	if r.defaultN == "" {
		return nil, fmt.Errorf("llm: no default provider configured")
	}
	return r.Get(r.defaultN)
}

// Provider returns the configuration for a name (without constructing the client).
func (r *Registry) Provider(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}
