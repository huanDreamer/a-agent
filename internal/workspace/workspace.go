// Package workspace confines filesystem and command tools to a single root
// directory, so exposing "read a file" or "run a command" to an LLM cannot
// reach the rest of the machine.
//
// The confinement is deliberately enforced at one place — Resolve — rather than
// by each tool, because a tool that forgets to check is exactly how an agent
// ends up reading /etc/shadow or writing outside its project.
//
// Platform notes: path handling uses filepath, so Windows would need its own
// review of the separator and case-sensitivity assumptions. Nothing here is
// macOS-specific, so Linux works as-is; only the shell differs (see the bash
// tool, which picks its interpreter per GOOS).
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Errors returned by Resolve. They are distinct so a tool can produce a message
// the model can act on ("that path is outside the workspace") rather than a
// generic failure.
var (
	// ErrOutsideWorkspace means the path escapes the root.
	ErrOutsideWorkspace = errors.New("path is outside the workspace")
	// ErrInvalidPath means the path is empty or unusable.
	ErrInvalidPath = errors.New("invalid path")
	// ErrReadOnly means the workspace forbids writes.
	ErrReadOnly = errors.New("workspace is read-only")
)

// Defaults for the limits. They exist to stop one tool call from filling memory
// or flooding the model's context.
const (
	// DefaultMaxReadBytes caps a single read.
	DefaultMaxReadBytes int64 = 512 << 10 // 512 KiB
	// DefaultMaxWriteBytes caps a single write.
	DefaultMaxWriteBytes int64 = 4 << 20 // 4 MiB
	// DefaultMaxListEntries caps a directory listing.
	DefaultMaxListEntries = 500
	// binarySniffBytes is how much of a file is inspected for binary content.
	binarySniffBytes = 8192
)

// Limits bound what a single operation may return or change.
type Limits struct {
	MaxReadBytes   int64
	MaxWriteBytes  int64
	MaxListEntries int
}

// Options configures a Workspace.
type Options struct {
	// ReadOnly forbids write_file, edit_file and command execution.
	ReadOnly bool
	// Limits overrides the defaults; zero fields take the default.
	Limits Limits
}

// Workspace is a rooted, optionally read-only view of the filesystem.
type Workspace struct {
	root     string
	readOnly bool
	limits   Limits
	// writes counts successful mutations, exposed for tests and metrics.
	writes atomic.Int64
}

// New resolves root once and returns a Workspace over it. The root does not
// have to exist yet for a read-only workspace, but a writable one requires a
// directory that can be created.
func New(root string, opts Options) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("workspace: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root %q: %w", root, err)
	}
	// Evaluate symlinks on the root itself so a root that is a symlink is
	// compared in its resolved form; otherwise every later containment check
	// would compare a resolved child against an unresolved root and reject it.
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	} else if !os.IsNotExist(rerr) {
		return nil, fmt.Errorf("workspace: resolve root symlinks: %w", rerr)
	}

	if !opts.ReadOnly {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return nil, fmt.Errorf("workspace: create root: %w", err)
		}
	}

	lim := opts.Limits
	if lim.MaxReadBytes <= 0 {
		lim.MaxReadBytes = DefaultMaxReadBytes
	}
	if lim.MaxWriteBytes <= 0 {
		lim.MaxWriteBytes = DefaultMaxWriteBytes
	}
	if lim.MaxListEntries <= 0 {
		lim.MaxListEntries = DefaultMaxListEntries
	}

	return &Workspace{root: abs, readOnly: opts.ReadOnly, limits: lim}, nil
}

// Root returns the resolved root directory.
func (w *Workspace) Root() string { return w.root }

// ReadOnly reports whether mutations are forbidden.
func (w *Workspace) ReadOnly() bool { return w.readOnly }

// Limits returns the configured limits.
func (w *Workspace) Limits() Limits { return w.limits }

// Writes returns how many mutations this workspace has performed.
func (w *Workspace) Writes() int64 { return w.writes.Load() }

// Resolve turns a caller-supplied path into an absolute path guaranteed to be
// inside the root.
//
// It rejects, in order: empty or NUL-containing paths, and any path that
// escapes the root — including escapes via "..", an absolute path pointing
// elsewhere, or a symlink whose target lies outside. Containment is checked
// after evaluating symlinks on the deepest part of the path that already
// exists, because checking the literal path alone would let
// `link -> /etc` pass and then be followed.
func (w *Workspace) Resolve(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: contains a NUL byte", ErrInvalidPath)
	}
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	// The root itself is a legitimate target ("list the workspace").
	if p == "." || p == "./" {
		return w.root, nil
	}

	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(w.root, p))
	}
	if !w.contains(abs) {
		return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, p)
	}

	// Resolve symlinks on the part that exists and re-check, so a symlink
	// inside the root cannot be used to step outside it.
	resolved, err := resolveExisting(abs)
	if err != nil {
		return "", err
	}
	if !w.contains(resolved) {
		return "", fmt.Errorf("%w: %s resolves outside via a symlink", ErrOutsideWorkspace, p)
	}
	return abs, nil
}

// ResolveForWrite is Resolve plus the read-only check and a write-size check.
func (w *Workspace) ResolveForWrite(p string, size int64) (string, error) {
	if w.readOnly {
		return "", fmt.Errorf("%w: %s", ErrReadOnly, p)
	}
	if size > w.limits.MaxWriteBytes {
		return "", fmt.Errorf("content is %d bytes, over the %d byte write limit",
			size, w.limits.MaxWriteBytes)
	}
	return w.Resolve(p)
}

// contains reports whether abs is the root or inside it.
func (w *Workspace) contains(abs string) bool {
	if abs == w.root {
		return true
	}
	rel, err := filepath.Rel(w.root, abs)
	if err != nil {
		return false
	}
	// A relative path escaping the root starts with "..".
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// resolveExisting evaluates symlinks on the longest existing prefix of abs and
// re-appends the non-existent remainder. For a path that does not exist yet
// (a write target), the parent is what matters: if the parent resolves inside
// the root, creating a file there is safe.
func resolveExisting(abs string) (string, error) {
	rest := ""
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("workspace: resolve %q: %w", abs, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root with nothing existing; nothing to
			// resolve, and containment was already checked on the literal path.
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Rel returns the path relative to the root, for display. It is what a tool
// should show the model, so output stays stable regardless of where the
// workspace lives.
func (w *Workspace) Rel(abs string) string {
	rel, err := filepath.Rel(w.root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// IsBinary reports whether a byte slice looks binary, judged by a NUL byte in
// the sniff window. Text tools use it to refuse to dump a binary into the
// model's context.
func IsBinary(b []byte) bool {
	if len(b) > binarySniffBytes {
		b = b[:binarySniffBytes]
	}
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
