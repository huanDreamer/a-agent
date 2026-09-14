// Package workspaces manages the set of directories the agent can be pointed at,
// and which one each scope (a web conversation, a Feishu user) is currently in.
//
// A workspace is one thing: a directory. It carries no policy — whether the
// agent may write files or run commands is process-wide configuration
// (`tools.read_only` / `tools.enable_bash`) — because "where the agent works" and
// "what the agent may do" are different questions, and a workspace that answered
// both would need a settings page for each one.
//
// It sits above internal/workspace, which stays what it was: the confinement of
// ONE root, enforced in one place. This package never resolves a caller path
// itself; it decides which sandbox a turn gets. Keeping the split means every
// guarantee about escapes, symlinks and limits still holds per workspace,
// unchanged, and this layer cannot weaken them.
//
// Two things are deliberately not stored here:
//
//   - Files. A workspace is a registration: deleting one removes the
//     registration and never the bytes, because that is a decision about the
//     operator's disk and not about what the agent may do.
//   - The conversation-to-directory mapping beyond the binding row. A
//     conversation always belongs to a workspace, and the SQL layer joins it in,
//     so there is no second place for that answer to live.
package workspaces

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/workspace"
)

// Scope keys. A selection is remembered per scope, and the prefix is what keeps
// a web conversation and a Feishu user from colliding on the same row: both
// identify themselves with an opaque id, and an id alone does not say which
// surface it came from.
const (
	webScopePrefix    = "web:"
	feishuScopePrefix = "feishu:"
)

// WebScope returns the scope key for a web chat session.
func WebScope(sessionID string) string {
	return webScopePrefix + strings.TrimSpace(sessionID)
}

// FeishuScope returns the scope key for a Feishu user.
//
// The open_id rather than the chat id: in a group every sender keeps their own
// directory, so one person switching projects does not move everybody's.
func FeishuScope(openID string) string {
	return feishuScopePrefix + strings.TrimSpace(openID)
}

// Limits on a workspace name. The name is a label the sidebar shows and a person
// types into Feishu, so it is bounded, but it is NOT a path component and is
// therefore not constrained by filesystem rules.
const (
	maxNameRunes = 60
	// maxBrowseEntries bounds a directory listing, so opening a huge directory
	// cannot produce a response nobody can read.
	maxBrowseEntries = 500
)

// Spec is one workspace as the runtime and the console see it.
type Spec struct {
	// Name is the label shown in the sidebar and used by a scope to refer to the
	// workspace. It can be renamed.
	Name string `json:"name"`
	// Root is the absolute directory this workspace points at.
	Root string `json:"root"`
	// SessionCount is how many conversations are in it.
	SessionCount int `json:"session_count"`
	// Missing reports that the directory is no longer there (deleted or moved
	// since it was registered). The console shows it, so a broken workspace is
	// visible instead of failing on the next tool call.
	Missing bool `json:"missing,omitempty"`

	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// ValidationError marks a refusal caused by the caller's input: an unusable name,
// a path that is not an existing directory, or a request that cannot say which
// conversation it means.
//
// It is a type rather than a sentinel so the message stays user-facing (the
// console shows it verbatim) while the HTTP layer can map it to 400 by asking
// what kind of error it is, instead of pattern-matching on text written for a
// person to read.
type ValidationError struct {
	msg string
}

// Error implements error.
func (e *ValidationError) Error() string { return e.msg }

// invalidf builds a ValidationError.
func invalidf(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// ValidateName checks a workspace label.
//
// The only hard rules are the ones that would break something: an empty name, a
// name long enough to wreck the layout, a path separator (which would make the
// label read like a path in the sidebar and in a Feishu message), and control
// characters (which would corrupt a log line or a card). Everything else —
// spaces, punctuation, Chinese — is allowed, because this is a label.
func ValidateName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return invalidf("工作区名字不能为空")
	}
	if trimmed != name {
		return invalidf("工作区名字首尾不能有空白字符")
	}
	if len([]rune(name)) > maxNameRunes {
		return invalidf("工作区名字最长 %d 个字符，当前 %d 个", maxNameRunes, len([]rune(name)))
	}
	if strings.ContainsAny(name, `/\`) {
		return invalidf(`工作区名字不能包含 / 或 \`)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return invalidf("工作区名字不能包含控制字符")
		}
	}
	return nil
}

// ResolveDir validates a directory the operator picked and returns its resolved
// absolute path.
//
// "Picked" is the whole contract: the directory must already exist, so a typo is
// reported while the operator is looking at the form rather than becoming an
// empty folder the agent then works in. Symlinks are resolved so the stored root
// is the same path the sandbox compares against — a root that is a symlink would
// otherwise reject its own children.
func ResolveDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", invalidf("请选择目录")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", invalidf("目录 %q 无法解析: %v", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", invalidf("目录不存在: %s", abs)
		}
		return "", invalidf("目录无法访问: %s（%v）", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", invalidf("目录无法访问: %s（%v）", resolved, err)
	}
	if !info.IsDir() {
		return "", invalidf("%s 不是目录", resolved)
	}
	if resolved == string(filepath.Separator) {
		// The agent's file tools are confined to the workspace, but a command is
		// not: pointing a workspace at the filesystem root would hand `bash` the
		// whole machine while the UI called it "a project". Refusing it is the one
		// place a deliberate choice is overruled, and the message says why.
		return "", invalidf("不能把文件系统根目录 %s 作为工作区，请选择具体的项目目录", resolved)
	}
	return resolved, nil
}

// DefaultNameFor derives a label from a directory, for the common case where the
// operator does not type one.
func DefaultNameFor(root string) string {
	base := filepath.Base(filepath.Clean(root))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "工作区"
	}
	return base
}

// sandbox builds the confinement for a spec.
func sandbox(spec Spec, readOnly bool, limits workspace.Limits) (*workspace.Workspace, error) {
	ws, err := workspace.New(spec.Root, workspace.Options{ReadOnly: readOnly, Limits: limits})
	if err != nil {
		return nil, fmt.Errorf("workspaces: 打开工作区 %q（%s）失败: %w", spec.Name, spec.Root, err)
	}
	return ws, nil
}

// specFromRow converts a stored row into a Spec, checking whether the directory
// is still there.
func specFromRow(r store.Workspace) Spec {
	missing := false
	if info, err := os.Stat(r.Root); err != nil || !info.IsDir() {
		missing = true
	}
	return Spec{
		Name:         r.Name,
		Root:         r.Root,
		SessionCount: r.SessionCount,
		Missing:      missing,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
}

// ---------------------------------------------------------------- browsing --

// DirEntry is one selectable directory in a listing.
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Link reports that the entry is a symlink to a directory, so the UI can mark
	// it — a link is fine to pick, but knowing it is one explains a path that
	// looks nothing like the directory's name.
	Link bool `json:"link,omitempty"`
}

// DirListing is one directory as the picker needs it.
type DirListing struct {
	// Path is the directory being listed (symlinks resolved).
	Path string `json:"path"`
	// Parent is the directory above it; empty at the filesystem root.
	Parent string `json:"parent,omitempty"`
	// Home is the user's home directory, so the picker can always offer a way
	// back to a familiar place.
	Home string `json:"home,omitempty"`
	// Entries are the subdirectories, by name. Files are not listed: a workspace
	// is a directory, and listing files would invite picking one.
	Entries []DirEntry `json:"entries"`
	// Truncated reports that the directory held more subdirectories than the
	// listing cap.
	Truncated bool `json:"truncated,omitempty"`
	// RootTooHigh reports that this is the filesystem root, which cannot be
	// chosen as a workspace (see ResolveDir).
	RootTooHigh bool `json:"root_too_high,omitempty"`
}

// Browse lists the subdirectories of a path, for the console's directory picker.
//
// It is the only way a browser can offer "choose a directory on the server": a
// file input yields no server-side path, and this console is served by the
// process whose filesystem is being chosen. Files are not listed and nothing is
// modified — the call reads one directory and answers.
func (m *Manager) Browse(path string) (DirListing, error) {
	target := strings.TrimSpace(path)
	if target == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return DirListing{}, invalidf("无法确定起始目录，请填写一个绝对路径")
		}
		target = home
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return DirListing{}, invalidf("路径无法解析: %s", target)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return DirListing{}, invalidf("目录不存在: %s", abs)
		}
		return DirListing{}, invalidf("目录无法访问: %s（%v）", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return DirListing{}, invalidf("%s 不是目录", resolved)
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return DirListing{}, invalidf("无法读取目录 %s: %v", resolved, err)
	}

	out := DirListing{
		Path:    resolved,
		Home:    homeOrEmpty(),
		Entries: make([]DirEntry, 0, len(entries)),
	}
	if resolved != string(filepath.Separator) {
		out.Parent = filepath.Dir(resolved)
	} else {
		out.RootTooHigh = true
	}

	for _, entry := range entries {
		if len(out.Entries) >= maxBrowseEntries {
			out.Truncated = true
			break
		}
		child := filepath.Join(resolved, entry.Name())
		switch {
		case entry.IsDir():
			out.Entries = append(out.Entries, DirEntry{Name: entry.Name(), Path: child})
		case entry.Type()&os.ModeSymlink != 0:
			// A symlinked project directory is common enough (and a symlinked
			// ~/code even more so) that one stat is worth it to tell whether the
			// link points at a directory.
			if target, serr := os.Stat(child); serr == nil && target.IsDir() {
				out.Entries = append(out.Entries, DirEntry{Name: entry.Name(), Path: child, Link: true})
			}
		}
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	return out, nil
}

// homeOrEmpty returns the home directory, or "" when it cannot be determined.
func homeOrEmpty() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
