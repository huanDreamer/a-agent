package artifact

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// relative location: under the session's directory, in today's date
	// directory, named after the title.
	if !strings.HasPrefix(res.Path, "sess-1/") {
		t.Errorf("Path = %q, want a sess-1/ prefix", res.Path)
	}
	if filepath.Ext(res.Path) != ".html" {
		t.Errorf("Path = %q, want a .html extension", res.Path)
	}
	if want := "sess-1/" + time.Now().Format(dateLayout) + "/"; !strings.HasPrefix(res.Path, want) {
		t.Errorf("Path = %q, want a %s prefix", res.Path, want)
	}
	// The name is the title, not a timestamp or a token. This is the whole
	// point of the change: a reader with `ls` can tell what a file is.
	if got := filepath.Base(res.Path); got != "quarterly-report.html" {
		t.Errorf("base name = %q, want quarterly-report.html", got)
	}

	body, err := os.ReadFile(filepath.Join(st.Root(), filepath.FromSlash(res.Path)))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != "<h1>hi</h1>" {
		t.Errorf("stored body = %q", body)
	}
}

// A Chinese title keeps its Chinese characters in the file name. That is the
// point of the naming rule: the store is meant to be readable, and a hex token
// tells a reader nothing about what the file holds.
func TestSave_ChineseTitleNamesTheFile(t *testing.T) {
	st := newTestStore(t, 0)
	res, err := st.Save(SaveInput{Title: "季度报告", SessionID: "s1"}, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := filepath.Base(res.Path); got != "季度报告.html" {
		t.Errorf("base name = %q, want 季度报告.html", got)
	}
	if strings.ContainsAny(res.Path, " \t") {
		t.Errorf("Path = %q, want no whitespace", res.Path)
	}
	// The part of the path before the name is the session and a date, so the
	// file is still addressable from the row alone.
	if dir := filepath.Dir(res.Path); dir != "s1/"+time.Now().Format(dateLayout) {
		t.Errorf("dir = %q, want s1/<today>", dir)
	}
}

// A title with nothing nameable in it must still produce a file: the save is the
// job, and the name is a convenience. It is named for what it is rather than by a
// random token, and a second one does not overwrite the first.
func TestSave_UnnameableTitleStillStores(t *testing.T) {
	st := newTestStore(t, 0)
	first, err := st.Save(SaveInput{Title: "***", SessionID: "s1"}, strings.NewReader("a"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	second, err := st.Save(SaveInput{Title: "???", SessionID: "s1"}, strings.NewReader("b"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := filepath.Base(first.Path); got != "artifact.html" {
		t.Errorf("first base name = %q, want artifact.html", got)
	}
	if got := filepath.Base(second.Path); got != "artifact-2.html" {
		t.Errorf("second base name = %q, want artifact-2.html", got)
	}
	body, err := os.ReadFile(filepath.Join(st.Root(), filepath.FromSlash(first.Path)))
	if err != nil || string(body) != "a" {
		t.Errorf("first artifact was overwritten: body = %q, err = %v", body, err)
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

// With no session the date directory is the top level of the store, so an
// unsessioned artifact is still filed by day rather than dropped in the root.
func TestSave_NoSessionLandsInDateDirectory(t *testing.T) {
	st := newTestStore(t, 0)
	res, err := st.Save(SaveInput{Title: "one shot"}, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if strings.Count(res.Path, "/") != 1 {
		t.Errorf("Path = %q, want exactly one directory (the date)", res.Path)
	}
	want := time.Now().Format(dateLayout) + "/one-shot.html"
	if res.Path != want {
		t.Errorf("Path = %q, want %q", res.Path, want)
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
		"季度报告":                   "季度报告",
		"Q3 用量报告":                "q3-用量报告",
		"Report_v2 Final!":       "report-v2-final",
		strings.Repeat("x", 100): strings.Repeat("x", maxStem),
		// The bound counts runes: a Chinese title of 60 characters is cut at 48
		// characters, not at 16 (which is what a byte bound would do).
		strings.Repeat("报", 60): strings.Repeat("报", maxStem),
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// A name is a second chance at a stem, and only its stem is read — a name that
// looks like a path must not be able to steer where the file lands.
func TestArtifactStem(t *testing.T) {
	cases := []struct {
		title, name, want string
	}{
		{"季度报告", "", "季度报告"},
		{"", "Q3 Report.html", "q3-report"},
		{"", "", unnamedStem},
		{"***", "../../../etc/passwd", "passwd"},
		{"/absolute/path/report.md", "", "absolute-path-report-md"},
	}
	for _, c := range cases {
		if got := artifactStem(c.title, c.name); got != c.want {
			t.Errorf("artifactStem(%q, %q) = %q, want %q", c.title, c.name, got, c.want)
		}
	}
}

// Saving the same title twice in one day must not overwrite the first file.
func TestSave_DeduplicatesNames(t *testing.T) {
	st := newTestStore(t, 0)
	bodies := []string{"first", "second", "third"}
	var paths []string
	for _, body := range bodies {
		res, err := st.Save(SaveInput{Title: "季度报告", SessionID: "s1"}, strings.NewReader(body))
		if err != nil {
			t.Fatalf("Save(%s): %v", body, err)
		}
		paths = append(paths, res.Path)
	}
	want := []string{"季度报告.html", "季度报告-2.html", "季度报告-3.html"}
	for i, p := range paths {
		if got := filepath.Base(p); got != want[i] {
			t.Errorf("base name %d = %q, want %q", i, got, want[i])
		}
		body, err := os.ReadFile(filepath.Join(st.Root(), filepath.FromSlash(p)))
		if err != nil || string(body) != bodies[i] {
			t.Errorf("artifact %d body = %q (err %v), want %q", i, body, err, bodies[i])
		}
	}
}

// Two sessions saving the same title do not share a file, because the session is
// a path prefix.
func TestSave_SessionsDoNotCollide(t *testing.T) {
	st := newTestStore(t, 0)
	a, err := st.Save(SaveInput{Title: "报告", SessionID: "s1"}, strings.NewReader("a"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := st.Save(SaveInput{Title: "报告", SessionID: "s2"}, strings.NewReader("b"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := filepath.Base(b.Path); got != "报告.html" {
		t.Errorf("second session's base name = %q, want 报告.html", got)
	}
	if a.Path == b.Path {
		t.Errorf("both sessions stored to %q", a.Path)
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
	// A Chinese name is escaped per segment and nothing else changes: the link
	// has to survive a round trip through c.Param on the serving route.
	got = URL("/api/artifacts/files", "s1/2026-02-14/季度报告.html")
	if got != "/api/artifacts/files/s1/2026-02-14/%E5%AD%A3%E5%BA%A6%E6%8A%A5%E5%91%8A.html" {
		t.Errorf("URL with a Chinese name = %q", got)
	}
	decoded, err := url.PathUnescape(strings.TrimPrefix(got, "/api/artifacts/files/"))
	if err != nil || decoded != "s1/2026-02-14/季度报告.html" {
		t.Errorf("round trip = %q, err = %v", decoded, err)
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
