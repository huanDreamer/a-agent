package lsp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// The subset of LSP payloads this package handles. They are declared here rather
// than generated because only four requests are ever made, and a generated
// binding for the whole protocol would be a large surface to keep current for no
// benefit.

// DiagnosticSeverity values, as LSP numbers them.
const (
	SeverityError       = 1
	SeverityWarning     = 2
	SeverityInformation = 3
	SeverityHint        = 4
)

// SeverityName renders a severity for a person, since "2" means nothing in a
// tool result.
func SeverityName(s int) string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInformation:
		return "info"
	case SeverityHint:
		return "hint"
	default:
		return "unknown"
	}
}

// Diagnostic is one problem the server reported for a file.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Code     string `json:"-"` // filled from the raw value, which may be a number
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
	// Related is how many related-information entries the server attached. The
	// notes themselves are dropped: they are usually a second location, and the
	// file and position already say where the problem is.
	Related int `json:"-"`
}

// rawDiagnostic is the wire shape. Code is decoded separately because the
// protocol allows a string or a number there, and a strict decode of a number
// into a string fails the whole message.
type rawDiagnostic struct {
	Range              Range           `json:"range"`
	Severity           int             `json:"severity"`
	Code               rawCode         `json:"code"`
	Source             string          `json:"source"`
	Message            string          `json:"message"`
	RelatedInformation []rawDiagnostic `json:"relatedInformation"`
}

type rawCode struct{ s string }

func (c *rawCode) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" {
		s = ""
	}
	c.s = s
	return nil
}

// DiagnosticParams is the payload of textDocument/publishDiagnostics.
type DiagnosticParams struct {
	URI         string          `json:"uri"`
	Version     int             `json:"version,omitempty"`
	Diagnostics []rawDiagnostic `json:"diagnostics"`
}

// Location is a place in a file, as LSP reports it.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// TextDocumentIdentifier names a document by URI.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// TextDocumentPositionParams is the common shape of definition/references.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// ReferenceParams adds the option to include the declaration itself.
type ReferenceParams struct {
	TextDocumentPositionParams
	Context ReferenceContext `json:"context"`
}

// ReferenceContext carries includeDeclaration, which decides whether the
// definition is part of the answer. It defaults to false here: a caller asking
// "who calls this" usually wants the callers.
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// SymbolKind names, as LSP numbers them (a subset; anything else is "symbol").
var symbolKindNames = map[int]string{
	1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class", 6: "method",
	7: "property", 8: "field", 9: "constructor", 10: "enum", 11: "interface",
	12: "function", 13: "variable", 14: "constant", 15: "string", 16: "number",
	17: "boolean", 18: "array", 19: "object", 20: "key", 21: "null", 22: "enum member",
	23: "struct", 24: "event", 25: "operator", 26: "type parameter",
}

// SymbolKindName renders a symbol kind for a person.
func SymbolKindName(k int) string {
	if name, ok := symbolKindNames[k]; ok {
		return name
	}
	return "symbol"
}

// Symbol is one entry of a workspace/symbol result.
//
// The protocol has two shapes here (`SymbolInformation` with a Location, and
// `WorkspaceSymbol` with a URI), and servers use both depending on the version
// they settled on. Only the name, kind and location are read, so the two shapes
// decode into one struct.
type Symbol struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
	// URI is the WorkspaceSymbol shape, where the location is built from
	// location.uri plus a range. The two shapes decode into one struct, so
	// callers take whichever field the server filled.
	URI string `json:"uri"`
}

// SymbolParams is the payload of workspace/symbol.
type SymbolParams struct {
	Query string `json:"query"`
}

// InitializeParams is what the client sends to open a session.
type InitializeParams struct {
	ProcessID             int                `json:"processId"`
	ClientInfo            *ClientInfo        `json:"clientInfo,omitempty"`
	RootURI               string             `json:"rootUri"`
	RootPath              string             `json:"rootPath,omitempty"`
	Capabilities          ClientCapabilities `json:"capabilities"`
	InitializationOptions any                `json:"initializationOptions,omitempty"`
	// WorkspaceFolders is the modern replacement for rootUri. Both are sent
	// because a server may read either.
	WorkspaceFolders []WorkspaceFolder `json:"workspaceFolders,omitempty"`
	Trace            string            `json:"trace,omitempty"`
}

// ClientInfo identifies this client in the server's log.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// WorkspaceFolder is one root of a session.
type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// ClientCapabilities declares what this client understands.
//
// Only what is needed is claimed. Claiming a capability the client does not
// actually implement is how a server starts sending notifications nobody
// handles, so this is deliberately minimal.
type ClientCapabilities struct {
	TextDocument TextDocumentClientCapabilities `json:"textDocument"`
	Workspace    WorkspaceClientCapabilities    `json:"workspace"`
	Window       WindowClientCapabilities       `json:"window"`
	General      *GeneralClientCapabilities     `json:"general,omitempty"`
}

type GeneralClientCapabilities struct {
	PositionEncodings []string `json:"positionEncodings,omitempty"`
}

type TextDocumentClientCapabilities struct {
	Synchronization    *SynchronizationCapability `json:"synchronization,omitempty"`
	PublishDiagnostics *struct {
		RelatedInformation bool `json:"relatedInformation,omitempty"`
	} `json:"publishDiagnostics,omitempty"`
	Definition     *struct{} `json:"definition,omitempty"`
	References     *struct{} `json:"references,omitempty"`
	DocumentSymbol *struct{} `json:"documentSymbol,omitempty"`
	// Hover is not exposed as a tool, so it is not advertised.
}

type SynchronizationCapability struct {
	// DynamicRegistration is false: this client registers nothing at runtime.
	DynamicRegistration bool `json:"dynamicRegistration"`
	// DidSave is false: the agent writes files itself and re-opens them, so it
	// never needs the server to re-read from disk.
	DidSave  bool `json:"didSave"`
	WillSave bool `json:"willSave"`
}

type WorkspaceClientCapabilities struct {
	WorkspaceFolders bool `json:"workspaceFolders"`
	Configuration    bool `json:"configuration"`
	Symbol           *struct {
		SymbolKind *struct {
			ValueSet []int `json:"valueSet,omitempty"`
		} `json:"symbolKind,omitempty"`
	} `json:"symbol,omitempty"`
}

type WindowClientCapabilities struct {
	WorkDoneProgress bool `json:"workDoneProgress"`
}

// InitializeResult is the part of the server's answer this client reads.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// ServerCapabilities decides which tools exist: a tool for a capability the
// server does not have is not registered at all, because a tool whose every call
// fails is worse than no tool.
type ServerCapabilities struct {
	DefinitionProvider bool `json:"-"`
	ReferencesProvider bool `json:"-"`
	RenameProvider     bool `json:"-"`
	WorkspaceSymbol    bool `json:"-"`
	// SyncKind is the textDocumentSync value: 1 = full, 2 = incremental. This
	// client always sends full content, which is valid for both.
	SyncKind int `json:"-"`
}

// UnmarshalJSON decodes the capabilities, whose three fields may each be a
// boolean or an options object.
func (c *ServerCapabilities) UnmarshalJSON(b []byte) error {
	var raw struct {
		TextDocumentSync   jsonOrBool `json:"textDocumentSync"`
		DefinitionProvider jsonOrBool `json:"definitionProvider"`
		ReferencesProvider jsonOrBool `json:"referencesProvider"`
		RenameProvider     jsonOrBool `json:"renameProvider"`
		WorkspaceSymbol    jsonOrBool `json:"workspaceSymbolProvider"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw.TextDocumentSync.present {
		if raw.TextDocumentSync.isBool {
			if raw.TextDocumentSync.b {
				c.SyncKind = 1
			}
		} else {
			c.SyncKind = raw.TextDocumentSync.kind
		}
	}
	c.DefinitionProvider = raw.DefinitionProvider.enabled()
	c.ReferencesProvider = raw.ReferencesProvider.enabled()
	c.RenameProvider = raw.RenameProvider.enabled()
	c.WorkspaceSymbol = raw.WorkspaceSymbol.enabled()
	return nil
}

// jsonOrBool decodes a field that the protocol allows to be either a boolean or
// an object. An object means "supported, here are its options".
type jsonOrBool struct {
	present bool
	isBool  bool
	b       bool
	kind    int
}

func (v *jsonOrBool) UnmarshalJSON(b []byte) error {
	v.present = true
	trimmed := strings.TrimSpace(string(b))
	switch {
	case trimmed == "null":
		return nil
	case trimmed == "true" || trimmed == "false":
		v.isBool = true
		v.b = trimmed == "true"
		return nil
	}

	// The protocol allows three shapes for these fields, and servers use all
	// three: a boolean, a plain number (textDocumentSync: 1 or 2 is what gopls
	// sends), or an options object carrying a `change` number. Handling only the
	// boolean and the object leaves SyncKind at 0 for the most common server,
	// which silently disables full-text sync.
	if n, err := strconv.Atoi(trimmed); err == nil {
		v.isBool = false
		v.kind = n
		return nil
	}
	v.isBool = false
	var obj struct {
		Change int `json:"change"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		v.kind = obj.Change
	}
	return nil
}

func (v jsonOrBool) enabled() bool {
	if !v.present {
		return false
	}
	if v.isBool {
		return v.b
	}
	return true // an options object means the capability is offered
}

// PathToURI converts an absolute filesystem path to a file:// URI.
func PathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.ToSlash(abs)
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	// Windows drive letters need the extra slash (file:///C:/...) and a
	// colon that is not a port separator.
	u := url.URL{Scheme: "file", Path: abs}
	if runtime.GOOS == "windows" {
		u.Path = "/" + abs
	}
	return u.String()
}

// URIToPath converts a file:// URI back to a filesystem path.
func URIToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("lsp: parse uri %q: %w", uri, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("lsp: uri %q is not a file:// uri", uri)
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) > 2 && p[0] == '/' && p[2] == ':' {
		p = p[1:] // strip the leading slash from /C:/...
	}
	if p == "" {
		return "", fmt.Errorf("lsp: uri %q has no path", uri)
	}
	return filepath.FromSlash(p), nil
}

// --- document synchronisation payloads ---

// didOpenParams is the payload of textDocument/didOpen.
type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

// textDocumentItem is one opened document.
type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// didChangeParams is the payload of textDocument/didChange.
type didChangeParams struct {
	TextDocument   versionedTextDocumentIdentifier `json:"textDocument"`
	ContentChanges []textDocumentContentChange     `json:"contentChanges"`
}

// versionedTextDocumentIdentifier names a document at a version.
type versionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// textDocumentContentChange is one change. Only the full-replacement form is
// sent, so only Text is set.
type textDocumentContentChange struct {
	Text string `json:"text"`
}

// languageIDs maps a file extension to the language identifier a server expects.
//
// The value is a label from the protocol's registry, not a guess: servers reject
// or ignore an unknown languageId, and "the file was sent with the wrong
// language" looks exactly like "the server does not understand this project".
var languageIDs = map[string]string{
	".go":    "go",
	".py":    "python",
	".js":    "javascript",
	".jsx":   "javascriptreact",
	".ts":    "typescript",
	".tsx":   "typescriptreact",
	".rs":    "rust",
	".c":     "c",
	".h":     "c",
	".cc":    "cpp",
	".cpp":   "cpp",
	".hpp":   "cpp",
	".java":  "java",
	".rb":    "ruby",
	".php":   "php",
	".sh":    "shellscript",
	".sql":   "sql",
	".yaml":  "yaml",
	".yml":   "yaml",
	".json":  "json",
	".toml":  "toml",
	".md":    "markdown",
	".lua":   "lua",
	".kt":    "kotlin",
	".swift": "swift",
}

// languageIDFor returns the protocol's language identifier for a path, or
// "plaintext" when the extension is unknown — which is what the protocol defines
// for unlabelled text.
func languageIDFor(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if id, ok := languageIDs[ext]; ok {
		return id
	}
	return "plaintext"
}

// LanguageIDFor is languageIDFor, exported for the server-matching logic in
// config.go and for tests.
func LanguageIDFor(path string) string { return languageIDFor(path) }
