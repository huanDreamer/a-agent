package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

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
	// File is the skill's file name (not the whole path: the console runs on the
	// same machine as the server, but an absolute path is still needless detail
	// to render). Empty for a stale override whose file is gone.
	File string `json:"file,omitempty"`
	// Bytes is the size of the skill file, so the list can show something about
	// a skill the operator is about to open.
	Bytes int `json:"bytes,omitempty"`
}

// skillDetail is one skill with its markdown body: what the editor loads.
type skillDetail struct {
	skillInfo
	Body string `json:"body"`
}

// skillDraft is a skill file's editable parts, independent of where it lives.
type skillDraft struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tools       []string `json:"tools"`
	Body        string   `json:"body"`
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
	// Read the directory once at construction. Loader.Get only answers for a
	// skill that has been loaded, and the write/toggle endpoints need to resolve
	// a name before any list request has happened.
	if s.loader != nil {
		_, _ = s.loader.LoadAll()
	}
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
	for _, sk := range s.loadedLocked() {
		name := sk.Frontmatter.Name
		info := skillInfo{
			Name:        name,
			Description: sk.Frontmatter.Description,
			Enabled:     !s.disabled[name],
			Tools:       sk.Frontmatter.Tools,
		}
		info.File, info.Bytes = s.fileInfoLocked(name)
		out = append(out, info)
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

// loadedLocked re-reads the skills directory and returns the skills it found.
// Caller holds mu.
//
// Every accessor re-reads rather than caching: the console writes skill files,
// so a cached list would show a stale body right after a save — the one moment
// the operator is looking at it.
func (s *skillState) loadedLocked() []*skill.Skill {
	if s.loader == nil {
		return nil
	}
	skills, _ := s.loader.LoadAll()
	return skills
}

// fileInfoLocked returns a skill's file name and size. Caller holds mu.
func (s *skillState) fileInfoLocked(name string) (string, int) {
	path, ok := s.pathLocked(name)
	if !ok {
		return "", 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return filepath.Base(path), 0
	}
	return filepath.Base(path), int(info.Size())
}

// pathLocked resolves the file a skill lives in: the file it was read from when
// it exists, otherwise the conventional <dir>/<name>.md a new skill would use.
// Caller holds mu.
func (s *skillState) pathLocked(name string) (string, bool) {
	if s.loader == nil {
		return "", false
	}
	if p, ok := s.loader.Path(name); ok {
		return p, true
	}
	if !skillNamePattern.MatchString(name) {
		return "", false
	}
	return filepath.Join(s.dir, name+".md"), true
}

// detail returns one skill with its markdown body.
func (s *skillState) detail(name string) (skillDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sk := range s.loadedLocked() {
		if sk.Frontmatter.Name != name {
			continue
		}
		info := skillInfo{
			Name:        name,
			Description: sk.Frontmatter.Description,
			Enabled:     !s.disabled[name],
			Tools:       sk.Frontmatter.Tools,
		}
		info.File, info.Bytes = s.fileInfoLocked(name)
		return skillDetail{skillInfo: info, Body: sk.Body}, nil
	}
	// A stale override: the file is gone but the disable entry remains. Report
	// it so the console can offer to clear it rather than 404 in a way that
	// hides why the row is listed at all.
	if s.disabled[name] {
		return skillDetail{skillInfo: skillInfo{Name: name, Enabled: false}}, nil
	}
	return skillDetail{}, fmt.Errorf("unknown skill %q", name)
}

// write creates or replaces a skill file.
//
// The file's frontmatter name is always the name in the URL: a save must not
// rename the skill, because the enabled/disabled override is keyed by name and
// a silent rename would leave the old key behind and lose the setting.
func (s *skillState) write(name string, draft skillDraft) (skillDetail, error) {
	if s.loader == nil || strings.TrimSpace(s.dir) == "" {
		return skillDetail{}, errors.New("no skills directory is configured (skills.dir)")
	}
	if !skillNamePattern.MatchString(name) {
		return skillDetail{}, fmt.Errorf(
			"skill name must be a slug of at most 64 characters: lowercase letters, digits, dot, dash or underscore, starting with a letter or digit")
	}
	if strings.TrimSpace(draft.Body) == "" {
		return skillDetail{}, errors.New("body is required: a skill with no instructions does nothing")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// An existing skill keeps its file, so a skill whose file name differs from
	// its frontmatter name is edited in place rather than duplicated.
	path, ok := s.pathLocked(name)
	if !ok {
		return skillDetail{}, fmt.Errorf("skill name %q cannot be used as a file name", name)
	}
	content, err := renderSkillFile(name, draft)
	if err != nil {
		return skillDetail{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return skillDetail{}, fmt.Errorf("create skills directory: %w", err)
	}
	// Write to a temp file and rename: a crash mid-save must not leave a
	// truncated skill that the loader then reports as broken.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return skillDetail{}, fmt.Errorf("write skill file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return skillDetail{}, fmt.Errorf("replace skill file: %w", err)
	}
	// Saving a skill makes it usable again: a name that was disabled and is then
	// rewritten is almost certainly being brought back.
	if s.disabled[name] {
		delete(s.disabled, name)
		if err := s.save(); err != nil {
			return skillDetail{}, err
		}
	}

	sk, perr := skill.LoadFile(name, string(content))
	if perr != nil {
		return skillDetail{}, fmt.Errorf("the saved skill does not parse: %w", perr)
	}
	return skillDetail{
		skillInfo: skillInfo{
			Name:        name,
			Description: sk.Frontmatter.Description,
			Enabled:     true,
			Tools:       sk.Frontmatter.Tools,
			File:        filepath.Base(path),
			Bytes:       len(content),
		},
		Body: sk.Body,
	}, nil
}

// remove deletes a skill file and its override entry.
func (s *skillState) remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, ok := s.pathLocked(name)
	if !ok {
		return fmt.Errorf("unknown skill %q", name)
	}
	err := os.Remove(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		// Nothing on disk; the override (if any) is still worth clearing.
		if !s.disabled[name] {
			return fmt.Errorf("unknown skill %q", name)
		}
	default:
		return fmt.Errorf("delete skill file: %w", err)
	}
	if s.disabled[name] {
		delete(s.disabled, name)
		return s.save()
	}
	return nil
}

// enabledSkills returns the loaded, enabled skills in name order — what the
// chat offers the model.
func (s *skillState) enabledSkills() []*skill.Skill {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*skill.Skill
	for _, sk := range s.loadedLocked() {
		if s.disabled[sk.Frontmatter.Name] {
			continue
		}
		out = append(out, sk)
	}
	return out
}

// enabledSkill returns one enabled skill by name.
func (s *skillState) enabledSkill(name string) (*skill.Skill, bool) {
	for _, sk := range s.enabledSkills() {
		if sk.Frontmatter.Name == name {
			return sk, true
		}
	}
	return nil, false
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

// skillNamePattern keeps a skill name usable both as a URL segment and as a
// file name. It is also the traversal guard: a name that matches it can never
// contain a path separator or start with a dot.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// renderSkillFile renders a skill as markdown with YAML frontmatter, in the
// exact shape internal/skill parses back.
func renderSkillFile(name string, draft skillDraft) ([]byte, error) {
	fm := struct {
		Name        string   `yaml:"name"`
		Description string   `yaml:"description,omitempty"`
		Tools       []string `yaml:"tools,omitempty"`
	}{Name: name, Description: strings.TrimSpace(draft.Description), Tools: draft.Tools}

	header, err := yaml.Marshal(fm)
	if err != nil {
		return nil, fmt.Errorf("render frontmatter: %w", err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.Write(header)
	b.WriteString("---\n")
	b.WriteString(strings.TrimLeft(draft.Body, "\n"))
	if !strings.HasSuffix(draft.Body, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// skills is the Server accessor used by the API.
func (s *Server) skills() []skillInfo { return s.skillState.list() }

// setSkillEnabled is the Server accessor used by the API.
func (s *Server) setSkillEnabled(name string, enabled bool) error {
	return s.skillState.setEnabled(name, enabled)
}

// errNoStore guards against a nil store at the API boundary.
var errNoStore = errors.New("server: store is not configured")
