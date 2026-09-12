package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newWS creates a workspace over a temp dir with some content.
func newWS(t *testing.T, opts Options) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	// macOS hands out /var -> /private/var symlinks; resolving keeps the
	// comparisons in this test honest rather than accidentally passing.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("eval root: %v", err)
	}
	ws, err := New(root, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ws, root
}

func TestNew_RequiresRoot(t *testing.T) {
	if _, err := New("", Options{}); err == nil {
		t.Fatal("want an error for an empty root")
	}
	if _, err := New("   ", Options{}); err == nil {
		t.Fatal("want an error for a blank root")
	}
}

func TestNew_CreatesWritableRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "nested", "project")
	ws, err := New(root, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat(ws.Root())
	if err != nil {
		t.Fatalf("root was not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("root is not a directory")
	}
}

func TestNew_ReadOnlyDoesNotCreateRoot(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "does-not-exist")
	if _, err := New(missing, Options{ReadOnly: true}); err != nil {
		t.Fatalf("a read-only workspace should tolerate a missing root: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("a read-only workspace must not create the root")
	}
}

func TestResolve_InsideRoot(t *testing.T) {
	ws, root := newWS(t, Options{})

	tests := []struct{ in, want string }{
		{"a.txt", filepath.Join(root, "a.txt")},
		{"./a.txt", filepath.Join(root, "a.txt")},
		{"sub/dir/b.go", filepath.Join(root, "sub", "dir", "b.go")},
		{"sub/../c.go", filepath.Join(root, "c.go")},
		{".", root},
		{filepath.Join(root, "abs.go"), filepath.Join(root, "abs.go")},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ws.Resolve(tc.in)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolve_RejectsEscapes(t *testing.T) {
	ws, root := newWS(t, Options{})

	escapes := []string{
		"../outside.txt",
		"../../etc/passwd",
		"sub/../../outside.txt",
		filepath.Join(filepath.Dir(root), "sibling.txt"),
		"/etc/passwd",
		"/",
		filepath.Join(root, "..", "..", "etc", "passwd"),
	}
	for _, p := range escapes {
		t.Run(p, func(t *testing.T) {
			_, err := ws.Resolve(p)
			if !errors.Is(err, ErrOutsideWorkspace) {
				t.Fatalf("Resolve(%q) err = %v, want ErrOutsideWorkspace", p, err)
			}
		})
	}
}

func TestResolve_RejectsInvalid(t *testing.T) {
	ws, _ := newWS(t, Options{})
	for _, p := range []string{"", "   ", "a\x00b"} {
		if _, err := ws.Resolve(p); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Resolve(%q) err = %v, want ErrInvalidPath", p, err)
		}
	}
}

// TestResolve_RejectsSymlinkEscape is the important one: checking the literal
// path is not enough, because a symlink inside the workspace can point outside.
func TestResolve_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	ws, root := newWS(t, Options{})

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// A symlink to a file outside, and one to a directory outside.
	if err := os.Symlink(secret, filepath.Join(root, "file-link")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir-link")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	for _, p := range []string{"file-link", "dir-link", filepath.Join("dir-link", "secret.txt")} {
		t.Run(p, func(t *testing.T) {
			_, err := ws.Resolve(p)
			if !errors.Is(err, ErrOutsideWorkspace) {
				t.Fatalf("Resolve(%q) err = %v, want ErrOutsideWorkspace", p, err)
			}
		})
	}

	// A write through the directory symlink must be refused too: the target
	// does not exist yet, so this exercises the parent-resolution path.
	t.Run("write through a dir symlink", func(t *testing.T) {
		if _, err := ws.ResolveForWrite(filepath.Join("dir-link", "new.txt"), 5); !errors.Is(err, ErrOutsideWorkspace) {
			t.Fatalf("err = %v, want ErrOutsideWorkspace", err)
		}
	})
}

func TestResolve_AllowsSymlinkInsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	ws, root := newWS(t, Options{})
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	got, err := ws.Resolve("link/file.txt")
	if err != nil {
		t.Fatalf("a symlink that stays inside must be allowed: %v", err)
	}
	if !strings.HasPrefix(got, root) {
		t.Errorf("resolved %q, want it inside %q", got, root)
	}
}

func TestResolveForWrite_ReadOnly(t *testing.T) {
	ws, _ := newWS(t, Options{ReadOnly: true})
	if !ws.ReadOnly() {
		t.Fatal("workspace should report read-only")
	}
	if _, err := ws.ResolveForWrite("a.txt", 1); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("err = %v, want ErrReadOnly", err)
	}
	// Reads must still work in a read-only workspace.
	if _, err := ws.Resolve("a.txt"); err != nil {
		t.Errorf("read resolution should work: %v", err)
	}
}

func TestResolveForWrite_SizeLimit(t *testing.T) {
	ws, _ := newWS(t, Options{Limits: Limits{MaxWriteBytes: 16}})
	if _, err := ws.ResolveForWrite("ok.txt", 16); err != nil {
		t.Errorf("a write at the limit should pass: %v", err)
	}
	if _, err := ws.ResolveForWrite("big.txt", 17); err == nil {
		t.Error("a write over the limit must be refused")
	}
}

func TestLimits_Defaults(t *testing.T) {
	ws, _ := newWS(t, Options{})
	lim := ws.Limits()
	if lim.MaxReadBytes != DefaultMaxReadBytes {
		t.Errorf("MaxReadBytes = %d, want %d", lim.MaxReadBytes, DefaultMaxReadBytes)
	}
	if lim.MaxWriteBytes != DefaultMaxWriteBytes {
		t.Errorf("MaxWriteBytes = %d, want %d", lim.MaxWriteBytes, DefaultMaxWriteBytes)
	}
	if lim.MaxListEntries != DefaultMaxListEntries {
		t.Errorf("MaxListEntries = %d, want %d", lim.MaxListEntries, DefaultMaxListEntries)
	}
}

func TestRel(t *testing.T) {
	ws, root := newWS(t, Options{})
	if got := ws.Rel(filepath.Join(root, "a", "b.go")); got != "a/b.go" {
		t.Errorf("Rel = %q, want a/b.go (slash-separated for stable output)", got)
	}
	if got := ws.Rel(root); got != "." {
		t.Errorf("Rel(root) = %q, want .", got)
	}
}

func TestIsBinary(t *testing.T) {
	if IsBinary([]byte("hello, world\n")) {
		t.Error("plain text was judged binary")
	}
	if !IsBinary([]byte{0x00, 0x01, 0x02}) {
		t.Error("a NUL byte should mark content binary")
	}
	if !IsBinary(append([]byte("prefix"), 0x00)) {
		t.Error("a NUL later in the window should mark content binary")
	}
	// A NUL beyond the sniff window is not inspected.
	long := append(make([]byte, binarySniffBytes+10), 0x00)
	for i := range long[:binarySniffBytes+10] {
		long[i] = 'a'
	}
	if IsBinary(long) {
		t.Error("a NUL past the sniff window should not be detected")
	}
}

func TestWrites_Counter(t *testing.T) {
	ws, _ := newWS(t, Options{})
	if ws.Writes() != 0 {
		t.Error("a fresh workspace should have no writes")
	}
}

func TestResolve_NestedMissingPathIsAllowed(t *testing.T) {
	// Writing a/b/c.txt where a/ does not exist yet must resolve, and the
	// containment check must not be fooled by the missing prefix.
	ws, root := newWS(t, Options{})
	got, err := ws.Resolve("a/b/c.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != filepath.Join(root, "a", "b", "c.txt") {
		t.Errorf("got %q", got)
	}
	// But a missing prefix that escapes is still refused.
	if _, err := ws.Resolve("a/../../../etc/passwd"); !errors.Is(err, ErrOutsideWorkspace) {
		t.Errorf("err = %v, want ErrOutsideWorkspace", err)
	}
}
