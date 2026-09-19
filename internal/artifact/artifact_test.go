package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T, maxBytes int64) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "artifacts"), Options{MaxBytes: maxBytes})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st
}

func TestSave_WritesUnderSessionAndReportsType(t *testing.T) {
	st := newTestStore(t, 0)

	res, err := st.Save(SaveInput{
		Title:     "Quarterly Report",
		SessionID: "sess-1",
		Name:      "q3.html",
	}, strings.NewReader("<h1>hi</h1>"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.MIME != "text/html; charset=utf-8" {
		t.Errorf("MIME = %q, want text/html", res.MIME)
	}
	if res.Kind != "html" {
		t.Errorf("Kind = %q, want html", res.Kind)
	}
	if res.Bytes != int64(len("<h1>hi</h1>")) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len("<h1>hi</h1>"))
	}
	// The path is what the serving route resolves, so it has to be the real
	// relative location and it has to be under the session's directory.
	if !strings.HasPrefix(res.Path, "sess-1/") {
		t.Errorf("Path = %q, want a sess-1/ prefix", res.Path)
	}
	if filepath.Ext(res.Path) != ".html" {
		t.Errorf("Path = %q, want a .html extension", res.Path)
	}
	if !strings.Contains(res.Path, "quarterly-report") {
		t.Errorf("Path = %q, want the title's slug in the name", res.Path)
	}

	body, err := os.ReadFile(filepath.Join(st.Root(), filepath.FromSlash(res.Path)))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != "<h1>hi</h1>" {
		t.Errorf("stored body = %q", body)
	}
}

// A Chinese title has no ASCII letters, so the slug is empty and the file is
// named by a random token. The important half is that the save still works and
// the path is still usable — an unnamed file is fine, a failed save is not.
func TestSave_ChineseTitleStillStores(t *testing.T) {
	st := newTestStore(t, 0)
	res, err := st.Save(SaveInput{Title: "季度报告", SessionID: "s1"}, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.Path == "" {
		t.Fatal("Path is empty")
	}
	if strings.ContainsAny(res.Path, " \t") {
		t.Errorf("Path = %q, want no whitespace", res.Path)
	}
}

func TestSave_DefaultsToHTML(t *testing.T) {
	st := newTestStore(t, 0)
	res, err := st.Save(SaveInput{Title: "page", SessionID: "s1"}, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if filepath.Ext(res.Path) != ".html" {
		t.Errorf("Path = %q, want .html when nothing else was said", res.Path)
	}
}

func TestSave_KindPicksExtension(t *testing.T) {
	cases := []struct {
		kind    string
		wantExt string
	}{
		{"document", ".md"},
		{"image", ".png"},
		{"html", ".html"},
		{"other", ".txt"},
	}
	for _, tc := range cases {
		st := newTestStore(t, 0)
		res, err := st.Save(SaveInput{Title: "t", SessionID: "s1", Kind: tc.kind}, strings.NewReader("x"))
		if err != nil {
			t.Fatalf("Save(%s): %v", tc.kind, err)
		}
		if got := filepath.Ext(res.Path); got != tc.wantExt {
			t.Errorf("kind %s -> %s, want %s", tc.kind, got, tc.wantExt)
		}
	}
}

func TestSave_NoSessionLandsInRoot(t *testing.T) {
	st := newTestStore(t, 0)
	res, err := st.Save(SaveInput{Title: "one shot"}, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if strings.Contains(res.Path, "/") {
		t.Errorf("Path = %q, want no directory for an unsessioned artifact", res.Path)
	}
}

func TestSave_RefusesOversize(t *testing.T) {
	st := newTestStore(t, 8)
	_, err := st.Save(SaveInput{Title: "big", SessionID: "s1"}, strings.NewReader(strings.Repeat("x", 9)))
	if err == nil {
		t.Fatal("Save accepted content over the cap")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %v, want it to name the limit", err)
	}
	// The refusal must not leave the file behind. The session directory is
	// created before the write is attempted (it is the destination, not the
	// artifact), so an empty directory is expected — a partial file is not, and
	// that is the thing this checks: the temp file must never survive.
	var names []string
	_ = filepath.WalkDir(st.Root(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		names = append(names, p)
		return nil
	})
	if len(names) != 0 {
		t.Errorf("a refused save left files behind: %v", names)
	}
}

func TestSave_RefusesUnsupportedType(t *testing.T) {
	st := newTestStore(t, 0)
	_, err := st.Save(SaveInput{Title: "t", SessionID: "s1", Name: "evil.svg"}, strings.NewReader("<svg/>"))
	if err == nil {
		t.Fatal("Save accepted .svg")
	}
	if !strings.Contains(err.Error(), "svg") {
		t.Errorf("error = %v, want it to name the extension", err)
	}
}

// The session id becomes a path component, so anything path-shaped in it is a
// way out of the root if it is not refused.
func TestSave_RejectsPathShapedSession(t *testing.T) {
	for _, bad := range []string{"../escape", "a/b", ".", "..", "sess\u0000", "sess id"} {
		st := newTestStore(t, 0)
		if _, err := st.Save(SaveInput{Title: "t", SessionID: bad}, strings.NewReader("x")); err == nil {
			t.Errorf("Save accepted session id %q", bad)
		}
	}
}

func TestResolve_RefusesEscape(t *testing.T) {
	st := newTestStore(t, 0)
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd", "", "..", "a\\..\\..\\x", "n\u0000ul"} {
		if _, err := st.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) was accepted", bad)
		} else if !errors.Is(err, ErrOutsideRoot) && !errors.Is(err, ErrDisabled) {
			t.Errorf("Resolve(%q) error = %v, want ErrOutsideRoot", bad, err)
		}
	}
}

func TestResolve_AllowsPathInside(t *testing.T) {
	st := newTestStore(t, 0)
	got, err := st.Resolve("s1/20260101-120000-page.html")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasPrefix(got, st.Root()) {
		t.Errorf("Resolve = %q, want it under %q", got, st.Root())
	}
}

// A symlink inside the store must not become a read of something outside it.
func TestResolve_RefusesSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "artifacts"), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outside := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(st.Root(), "link.txt")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := st.Resolve("link.txt"); err == nil {
		t.Fatal("Resolve followed a symlink out of the root")
	}
}

func TestOpen_RefusesDirectory(t *testing.T) {
	st := newTestStore(t, 0)
	if err := os.MkdirAll(filepath.Join(st.Root(), "s1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, _, err := st.Open("s1"); err == nil {
		t.Fatal("Open accepted a directory")
	}
}

func TestRemove_MissingFileIsFine(t *testing.T) {
	st := newTestStore(t, 0)
	if err := st.Remove("s1/nope.html"); err != nil {
		t.Errorf("Remove(missing) = %v, want nil", err)
	}
}

func TestRemove_DeletesFile(t *testing.T) {
	st := newTestStore(t, 0)
	res, err := st.Save(SaveInput{Title: "t", SessionID: "s1"}, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.Remove(res.Path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.Root(), filepath.FromSlash(res.Path))); !os.IsNotExist(err) {
		t.Errorf("file still there: %v", err)
	}
}

func TestNew_RefusesEmptyRoot(t *testing.T) {
	if _, err := New("  ", Options{}); !errors.Is(err, ErrDisabled) {
		t.Errorf("New(\"\") = %v, want ErrDisabled", err)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Quarterly Report":       "quarterly-report",
		"a/b\\c":                 "a-b-c",
		"  Leading  ":            "leading",
		"***":                    "",
		"季度报告":                   "",
		"Report_v2 Final!":       "report_v2-final",
		strings.Repeat("x", 100): strings.Repeat("x", maxSlug),
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("季度报告", 2); got != "季度" {
		t.Errorf("truncateRunes = %q, want 季度", got)
	}
	if got := truncateRunes("abc", 5); got != "abc" {
		t.Errorf("truncateRunes = %q, want abc", got)
	}
}

func TestURL_EscapesSegments(t *testing.T) {
	got := URL("/api/artifacts/files", "s1/a b.html")
	if got != "/api/artifacts/files/s1/a%20b.html" {
		t.Errorf("URL = %q", got)
	}
	if got := URL("/p/", "s1/x.html"); got != "/p/s1/x.html" {
		t.Errorf("URL with trailing slash = %q", got)
	}
}

func TestMIMEForPath(t *testing.T) {
	if mime, ok := MIMEForPath("s1/x.md"); !ok || !strings.HasPrefix(mime, "text/markdown") {
		t.Errorf("MIMEForPath(.md) = %q, %v", mime, ok)
	}
	if _, ok := MIMEForPath("s1/x.svg"); ok {
		t.Error("MIMEForPath(.svg) reported a supported type")
	}
}
