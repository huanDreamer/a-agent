package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/huan/huan-agent/internal/skill"
)

// skillInfo is one row of the skills API.
type skillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	// Tools is the skill's declared tool allow-list, shown in the UI so an
	// operator can see what enabling it exposes.
	Tools []string `json:"tools,omitempty"`
}

// skillState persists which skills are enabled. The overrides live in a small
// JSON file beside the database rather than in the skills directory, so
// disabling a skill never edits the user's markdown.
type skillState struct {
	mu       sync.Mutex
	dir      string
	statePth string
	// disabled records explicitly disabled skill names.
	disabled map[string]bool
	loader   *skill.Loader
}

// newSkillState builds the skill registry. dir may be empty, in which case
// there are simply no skills to list.
func newSkillState(dir, statePath string) *skillState {
	s := &skillState{
		dir:      dir,
		statePth: statePath,
		disabled: make(map[string]bool),
	}
	if dir != "" {
		s.loader = skill.NewLoader(dir)
	}
	s.load()
	return s
}

// StatePathFor derives the skill-override file path from the database path. It
// is exported so the CLI can point the server at the same file the agent uses.
func StatePathFor(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dbPath), "skills.json")
}

// load reads the override file, ignoring a missing file. A corrupt file is
// ignored too: skill toggles are a convenience, not a source of truth, so a
// bad file must not stop the admin server from starting.
func (s *skillState) load() {
	if s.statePth == "" {
		return
	}
	b, err := os.ReadFile(s.statePth)
	if err != nil {
		return
	}
	var disabled []string
	if err := json.Unmarshal(b, &disabled); err != nil {
		return
	}
	for _, n := range disabled {
		s.disabled[n] = true
	}
}

// save writes the override file atomically (temp file + rename) so a crash
// cannot leave a half-written file behind.
func (s *skillState) save() error {
	if s.statePth == "" {
		return nil
	}
	names := make([]string, 0, len(s.disabled))
	for n := range s.disabled {
		names = append(names, n)
	}
	sort.Strings(names)
	b, err := json.MarshalIndent(names, "", "  ")
	if err != nil {
		return fmt.Errorf("server: marshal skill state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.statePth), 0o755); err != nil {
		return fmt.Errorf("server: create skill state dir: %w", err)
	}
	tmp := s.statePth + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("server: write skill state: %w", err)
	}
	if err := os.Rename(tmp, s.statePth); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("server: replace skill state: %w", err)
	}
	return nil
}

// list returns every known skill with its effective enabled state, sorted by
// name.
func (s *skillState) list() []skillInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]skillInfo, 0, 8)
	if s.loader != nil {
		skills, err := s.loader.LoadAll()
		if err == nil {
			for _, sk := range skills {
				name := sk.Frontmatter.Name
				out = append(out, skillInfo{
					Name:        name,
					Description: sk.Frontmatter.Description,
					Enabled:     !s.disabled[name],
					Tools:       sk.Frontmatter.Tools,
				})
			}
		}
	}

	// A disabled skill whose file was deleted should still be visible, so an
	// operator can see (and clear) the stale override.
	known := make(map[string]bool, len(out))
	for _, it := range out {
		known[it.Name] = true
	}
	for name := range s.disabled {
		if !known[name] {
			out = append(out, skillInfo{Name: name, Enabled: false})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// setEnabled enables or disables a skill by name. It reports an error for an
// unknown skill so the API can answer 404 instead of silently accepting a typo.
func (s *skillState) setEnabled(name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.knownLocked(name) {
		return fmt.Errorf("unknown skill %q", name)
	}
	if enabled {
		delete(s.disabled, name)
	} else {
		s.disabled[name] = true
	}
	return s.save()
}

// knownLocked reports whether a skill exists on disk or in the override set.
// Caller holds mu.
func (s *skillState) knownLocked(name string) bool {
	if s.disabled[name] {
		return true
	}
	if s.loader == nil {
		return false
	}
	_, ok := s.loader.Get(name)
	return ok
}

// skills is the Server accessor used by the API.
func (s *Server) skills() []skillInfo { return s.skillState.list() }

// setSkillEnabled is the Server accessor used by the API.
func (s *Server) setSkillEnabled(name string, enabled bool) error {
	return s.skillState.setEnabled(name, enabled)
}

// errNoStore guards against a nil store at the API boundary.
var errNoStore = errors.New("server: store is not configured")
