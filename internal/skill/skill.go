// Package skill loads "skill" definitions from markdown files with
// YAML frontmatter, and exposes them to the agent loop as a system
// prompt + optional tool allow-list.
package skill

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter mirrors the YAML header at the top of a skill file.
// Only the fields we care about are declared; unknown keys are kept
// verbatim in Extra so callers can introspect them.
type Frontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tools       []string `yaml:"tools"`
	Extra       map[string]any
}

// Skill is one loaded skill: frontmatter + body markdown.
type Skill struct {
	Frontmatter Frontmatter
	Body        string
}

// LoadFile parses a single skill file (markdown with optional
// YAML frontmatter delimited by "---" on its own lines).
//
// Files without a frontmatter block are still accepted; Name is
// derived from the first H1 if present, otherwise from the file
// name (caller-provided).
func LoadFile(name, content string) (*Skill, error) {
	fm, body, err := splitFrontmatter(content)
	if err != nil {
		return nil, fmt.Errorf("skill %q: %w", name, err)
	}
	s := &Skill{Frontmatter: fm, Body: body}
	if s.Frontmatter.Name == "" {
		s.Frontmatter.Name = name
	}
	if s.Frontmatter.Name == "" {
		return nil, fmt.Errorf("skill: name is required (set in frontmatter or pass explicitly)")
	}
	return s, nil
}

// SystemPrompt renders the skill as a system message string. The
// description comes first (so the model sees intent), followed by
// the body.
func (s *Skill) SystemPrompt() string {
	var b strings.Builder
	if s.Frontmatter.Description != "" {
		b.WriteString(s.Frontmatter.Description)
		b.WriteString("\n\n")
	}
	b.WriteString(s.Body)
	return strings.TrimRight(b.String(), "\n")
}

// splitFrontmatter parses the leading "--- ... ---" block. If
// absent, Frontmatter is zero-valued and body == content.
func splitFrontmatter(content string) (Frontmatter, string, error) {
	var fm Frontmatter
	// Normalize line endings so the frontmatter delimiter check is
	// robust against CRLF files (Windows editors, git autocrlf).
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return fm, content, nil
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return fm, "", fmt.Errorf("frontmatter open delimiter found but no closing ---")
	}
	raw := rest[:end]
	body := rest[end+5:]

	// Decode YAML, then keep unknown fields in Extra so callers can
	// inspect them later (e.g. version, author).
	dec := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.KnownFields(false)
	if err := dec.Decode(&fm); err != nil {
		return fm, "", fmt.Errorf("parse frontmatter: %w", err)
	}
	// Second pass: re-decode into a generic map to capture extras.
	var generic map[string]any
	if err := yaml.Unmarshal([]byte(raw), &generic); err == nil {
		// Strip known keys.
		delete(generic, "name")
		delete(generic, "description")
		delete(generic, "tools")
		if len(generic) > 0 {
			fm.Extra = generic
		}
	}
	return fm, body, nil
}
