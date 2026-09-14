package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// openTestStore opens a migrated database in a temp directory.
func openTestStore(t *testing.T) Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestMCPServerRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	want := MCPServer{
		ID:        "github",
		Name:      "GitHub",
		Transport: MCPTransportStdio,
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-github"},
		Env:       []string{"GITHUB_TOKEN=abc", "LOG_LEVEL=debug"},
		Source:    SourceUser,
		Enabled:   true,
	}
	if err := st.UpsertMCPServer(ctx, want); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}

	got, err := st.GetMCPServer(ctx, "github")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if got.Name != want.Name || got.Command != want.Command || !got.Enabled {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if len(got.Args) != 2 || got.Args[1] != want.Args[1] {
		t.Errorf("Args = %v, want %v", got.Args, want.Args)
	}
	// The env list is stored verbatim: a token in it is the operator's own
	// definition, and the connection needs it unchanged.
	if len(got.Env) != 2 || got.Env[0] != "GITHUB_TOKEN=abc" {
		t.Errorf("Env = %v, want %v", got.Env, want.Env)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("timestamps were not set")
	}
}

func TestMCPServerRemoteTransportRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.UpsertMCPServer(ctx, MCPServer{
		ID: "remote", Name: "Remote", Transport: MCPTransportSSE,
		URL: "https://example.com/sse", Headers: []string{"Authorization: Bearer x"},
		Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	got, err := st.GetMCPServer(ctx, "remote")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if got.Transport != MCPTransportSSE || got.URL != "https://example.com/sse" {
		t.Errorf("got %+v", got)
	}
	if !got.Remote() {
		t.Error("Remote() = false for an sse server")
	}
	if len(got.Headers) != 1 || got.Headers[0] != "Authorization: Bearer x" {
		t.Errorf("Headers = %v", got.Headers)
	}
}

func TestMCPServerUpsertReplacesTheWholeRow(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	base := MCPServer{ID: "demo", Name: "Demo", Command: "node", Args: []string{"a.js"}, Enabled: true}
	if err := st.UpsertMCPServer(ctx, base); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Clearing the args must clear them: unlike a provider's API key, nothing
	// here is write-only, so a blank field means blank.
	base.Args = nil
	base.Command = "python3"
	if err := st.UpsertMCPServer(ctx, base); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, err := st.GetMCPServer(ctx, "demo")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if got.Command != "python3" {
		t.Errorf("Command = %q, want python3", got.Command)
	}
	if len(got.Args) != 0 {
		t.Errorf("Args = %v, want none", got.Args)
	}
}

func TestMCPServerDisabledFlagRoundTrips(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertMCPServer(ctx, MCPServer{ID: "off", Name: "Off", Command: "node", Enabled: false}); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	got, err := st.GetMCPServer(ctx, "off")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if got.Enabled {
		t.Error("Enabled = true, want false")
	}
}

func TestMCPServerListOrdersConfigFirst(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for _, row := range []MCPServer{
		{ID: "zzz-user", Name: "User", Command: "node", Source: SourceUser, Enabled: true},
		{ID: "aaa-config", Name: "Config", Command: "node", Source: SourceConfig, Enabled: true},
	} {
		if err := st.UpsertMCPServer(ctx, row); err != nil {
			t.Fatalf("UpsertMCPServer(%s): %v", row.ID, err)
		}
	}
	rows, err := st.ListMCPServers(ctx)
	if err != nil {
		t.Fatalf("ListMCPServers: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	// The read-only config rows come first, where the console can pin them.
	if rows[0].ID != "aaa-config" {
		t.Errorf("first row = %q, want the config-sourced one", rows[0].ID)
	}
}

func TestMCPServerDelete(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertMCPServer(ctx, MCPServer{ID: "gone", Name: "Gone", Command: "node", Enabled: true}); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	if err := st.DeleteMCPServer(ctx, "gone"); err != nil {
		t.Fatalf("DeleteMCPServer: %v", err)
	}
	if _, err := st.GetMCPServer(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetMCPServer after delete = %v, want ErrNotFound", err)
	}
	// Deleting an unknown id must report it: a silent success would let the UI
	// believe it removed something.
	if err := st.DeleteMCPServer(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

func TestMCPServerSetError(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertMCPServer(ctx, MCPServer{ID: "demo", Name: "Demo", Command: "node", Enabled: true}); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	if err := st.SetMCPServerError(ctx, "demo", "spawn failed"); err != nil {
		t.Fatalf("SetMCPServerError: %v", err)
	}
	got, err := st.GetMCPServer(ctx, "demo")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if got.LastError != "spawn failed" {
		t.Errorf("LastError = %q", got.LastError)
	}
}

func TestMCPServerRequiresID(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertMCPServer(context.Background(), MCPServer{Name: "NoId", Command: "node"}); err == nil {
		t.Fatal("expected an error for a missing id")
	}
}

func TestMCPServerValidate(t *testing.T) {
	cases := []struct {
		name string
		row  MCPServer
		want string
	}{
		{"missing id", MCPServer{Command: "node"}, "id is required"},
		{"bad transport", MCPServer{ID: "a", Transport: "smoke-signal"}, "unsupported transport"},
		{"stdio without command", MCPServer{ID: "a", Transport: MCPTransportStdio}, "command"},
		{"sse without url", MCPServer{ID: "a", Transport: MCPTransportSSE}, "url"},
		{"http without url", MCPServer{ID: "a", Transport: MCPTransportHTTP}, "url"},
		{"ok stdio", MCPServer{ID: "a", Command: "node"}, ""},
		{"ok sse", MCPServer{ID: "a", Transport: MCPTransportSSE, URL: "https://x/sse"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.row.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestMCPTransportVocabulary(t *testing.T) {
	if !ValidMCPTransport(MCPTransportStdio) || !ValidMCPTransport(MCPTransportSSE) || !ValidMCPTransport(MCPTransportHTTP) {
		t.Error("the three transports must all be valid")
	}
	if ValidMCPTransport("grpc") {
		t.Error("an unknown transport must be rejected")
	}
	if len(AllMCPTransports) != 3 {
		t.Errorf("AllMCPTransports = %v", AllMCPTransports)
	}
}

func TestDecodeStringListToleratesCorruption(t *testing.T) {
	// A hand-edited database row must not make the whole server unloadable.
	if got := decodeStringList("{not json"); got != nil {
		t.Errorf("decodeStringList = %v, want nil", got)
	}
	if got := decodeStringList(""); got != nil {
		t.Errorf("empty = %v, want nil", got)
	}
	if got := decodeStringList(`["a","b"]`); len(got) != 2 {
		t.Errorf("got %v", got)
	}
}
