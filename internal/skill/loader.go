package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Loader scans a directory of *.md files and lazily loads them as
// Skills. LoadAll is safe to call repeatedly; subsequent calls
// re-read the directory.
type Loader struct {
	dir string
	mu  sync.RWMutex
	all map[string]*Skill
}

// NewLoader creates a Loader over dir. The directory MAY be empty
// or missing; LoadAll returns no skills and a nil error in that
// case.
func NewLoader(dir string) *Loader { return &Loader{dir: dir} }

// LoadAll scans dir for *.md files and parses each. Errors parsing
// individual files are accumulated into a multi-error returned to
// the caller; successfully parsed skills are still returned.
//
// The directory is read once per call; the loader is then considered
// "loaded" until the next call. Callers needing hot-reload should
// invoke LoadAll again.
func (l *Loader) LoadAll() ([]*Skill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.dir == "" {
		l.all = map[string]*Skill{}
		return nil, nil
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			l.all = map[string]*Skill{}
			return nil, nil
		}
		return nil, fmt.Errorf("skill: read dir %s: %w", l.dir, err)
	}

	loaded := map[string]*Skill{}
	var errs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(l.dir, name)
		raw, rErr := os.ReadFile(path)
		if rErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, rErr))
			continue
		}
		baseName := strings.TrimSuffix(name, filepath.Ext(name))
		s, pErr := LoadFile(baseName, string(raw))
		if pErr != nil {
			errs = append(errs, pErr.Error())
			continue
		}
		if _, dup := loaded[s.Frontmatter.Name]; dup {
			errs = append(errs, fmt.Sprintf("duplicate skill name %q (file %s)", s.Frontmatter.Name, name))
			continue
		}
		loaded[s.Frontmatter.Name] = s
	}
	l.all = loaded

	if len(errs) > 0 {
		return sortedSkills(loaded), fmt.Errorf("skill: %d file(s) failed: %s", len(errs), strings.Join(errs, "; "))
	}
	return sortedSkills(loaded), nil
}

// Get returns the named skill, if loaded.
func (l *Loader) Get(name string) (*Skill, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.all[name]
	return s, ok
}

// Names returns all loaded skill names in deterministic order.
func (l *Loader) Names() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.all))
	for n := range l.all {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func sortedSkills(m map[string]*Skill) []*Skill {
	out := make([]*Skill, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Frontmatter.Name < out[j].Frontmatter.Name })
	return out
}
