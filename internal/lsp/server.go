package lsp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// One configured language server, and how a file is matched to one.

// ServerSpec is one language server the operator declared.
type ServerSpec struct {
	// Name is the label used in logs and error messages.
	Name string `mapstructure:"name" json:"name"`
	// Command is the binary to run. It must be on PATH.
	Command string `mapstructure:"command" json:"command"`
	// Args are passed to the command.
	Args []string `mapstructure:"args" json:"args"`
	// Env entries are "KEY=value" and are merged onto the process environment.
	Env []string `mapstructure:"env" json:"env"`
	// Languages are the file extensions or language ids this server handles:
	// ".go", "go", ".py", "python". Both spellings are accepted because the
	// config is written by hand and "go" is what a person types.
	Languages []string `mapstructure:"languages" json:"languages"`
	// RootMarkers are file names that mark the root of the project this server
	// should be started for: go.mod, go.work, package.json. The nearest ancestor
	// containing one becomes the server's root, because that is what decides
	// which module gets indexed.
	RootMarkers []string `mapstructure:"root_markers" json:"root_markers"`
	// InitOptions is passed through as initializationOptions.
	InitOptions map[string]any `mapstructure:"init_options" json:"init_options"`
	// Enabled defaults to true when absent, matching every other server list in
	// this project.
	Enabled *bool `mapstructure:"enabled" json:"enabled,omitempty"`
}

// IsEnabled reports whether the server should be started, treating an absent
// flag as enabled.
func (s ServerSpec) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// Covers reports whether this server handles a path.
//
// A language may be written either way in the config, because it is written by
// hand: ".go" is the extension and "go" is the protocol's language identifier,
// and a person typing a server list will use whichever comes to mind.
func (s ServerSpec) Covers(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return false
	}
	lang := languageIDFor(path)
	bare := strings.TrimPrefix(ext, ".")

	for _, l := range s.Languages {
		l = strings.ToLower(strings.TrimSpace(l))
		switch {
		case l == "":
			continue
		case strings.HasPrefix(l, "."):
			if l == ext {
				return true
			}
		default:
			if l == lang || l == bare {
				return true
			}
		}
	}
	return false
}

// RootFor returns the directory a server should be started for, given a file.
//
// The nearest ancestor holding one of the markers wins, and when there is none
// the ceiling (the workspace root) is used. This is not a detail: a language
// server indexes the project its root names, so starting gopls at a subdirectory
// of a module gives it a view of one package, and "no references found" becomes
// a wrong answer rather than a missing one.
func RootFor(file string, markers []string, ceiling string) string {
	dir := file
	if info, err := os.Stat(file); err != nil || !info.IsDir() {
		dir = filepath.Dir(file)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ceiling
	}
	if ceiling != "" {
		if abs, cerr := filepath.Abs(ceiling); cerr == nil {
			ceiling = abs
		}
	}

	for {
		for _, marker := range markers {
			marker = strings.TrimSpace(marker)
			if marker == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // filesystem root
		}
		// Do not climb past the workspace: a marker outside it would start a
		// server over a tree the agent is not confined to.
		if ceiling != "" && !strings.HasPrefix(parent, ceiling) {
			break
		}
		dir = parent
	}
	if ceiling != "" {
		return ceiling
	}
	return dir
}

// DefaultServers is the built-in table used when the config declares none.
//
// It is data, not policy: an operator replaces it by declaring their own list,
// and a test asserts it stays in step with the commented example in
// configs/config.example.yaml. Keeping the default here rather than in the
// config file means `tools.lsp.enable: true` with no server list still does the
// obvious thing for a Go repository.
func DefaultServers() []ServerSpec {
	return []ServerSpec{
		{
			Name:        "gopls",
			Command:     "gopls",
			Args:        []string{"-mode=stdio"},
			Languages:   []string{"go"},
			RootMarkers: []string{"go.mod", "go.work"},
		},
	}
}

// installHint is what to tell an operator whose configured server is missing.
//
// A tool that silently does not exist is a mystery; a tool that is missing with
// the install command in the log is a task.
func installHint(command string) string {
	switch command {
	case "gopls":
		return "install it with: go install golang.org/x/tools/gopls@latest"
	case "typescript-language-server":
		return "install it with: npm install -g typescript-language-server typescript"
	case "pyright-langserver":
		return "install it with: npm install -g pyright"
	case "rust-analyzer":
		return "install it with: rustup component add rust-analyzer"
	default:
		return "install " + command + " and make sure it is on PATH"
	}
}

// errServerMissing means the configured command is not installed.
var errServerMissing = errors.New("language server is not installed")

// startProcess launches a language server and returns the LSP stream.
//
// stderr is captured rather than discarded: a server that exits immediately
// explains itself there ("unknown flag", "module requires Go 1.24"), and that
// message is the entire difference between a fixable problem and a mystery.
func startProcess(ctx context.Context, spec ServerSpec, root string) (io.ReadWriteCloser, *exec.Cmd, *stderrTail, error) {
	path, err := exec.LookPath(spec.Command)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %q (%s)", errServerMissing, spec.Command, installHint(spec.Command))
	}

	cmd := exec.Command(path, spec.Args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.SysProcAttr = detachAttr()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("lsp: %s: stdin pipe: %w", spec.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("lsp: %s: stdout pipe: %w", spec.Name, err)
	}
	tail := newStderrTail(8 << 10)
	cmd.Stderr = tail

	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("lsp: %s: start %s: %w", spec.Name, spec.Command, err)
	}

	return &processStream{stdin: stdin, stdout: stdout}, cmd, tail, nil
}

// processStream is the language server's stdio as one duplex stream.
type processStream struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	once   sync.Once
}

func (p *processStream) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *processStream) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *processStream) Close() error {
	var err error
	p.once.Do(func() {
		// Closing stdin is how a server is told the conversation is over, so it
		// goes first; the pipes themselves are closed after.
		_ = p.stdin.Close()
		err = p.stdout.Close()
	})
	return err
}

// stderrTail keeps the last N bytes a server wrote to stderr, so a startup
// failure can say what the server said rather than only that it died.
type stderrTail struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func newStderrTail(max int) *stderrTail { return &stderrTail{max: max} }

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > t.max {
		// Keep the tail: the last lines are where a crash message is.
		all := t.buf.Bytes()
		keep := all[len(all)-t.max:]
		t.buf.Reset()
		t.buf.Write(keep)
	}
	return len(p), nil
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(t.buf.String())
}
