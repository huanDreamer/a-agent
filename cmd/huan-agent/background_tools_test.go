package main

// Tests for how the background tools are wired into a turn.
//
// Two properties matter here and nowhere else: that the operator's switches are
// honoured by withholding the tools rather than registering ones that refuse, and
// that a job started from a conversation records the workspace and the surface it
// came from — which is what the console shows and what the tools scope on.

import (
	"context"
	"encoding/json"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/tool/builtin"
	"github.com/huan/huan-agent/internal/workspace"
)

// backgroundToolNames is the set a turn may see when everything is switched on.
func backgroundToolNames() []string {
	return []string{
		builtin.BackgroundToolName,
		builtin.BackgroundListToolName,
		builtin.BackgroundOutputToolName,
		builtin.BackgroundStopToolName,
	}
}

// newBackgroundTestManager returns a supervisor for wiring tests.
func newBackgroundTestManager(t *testing.T) *jobs.Manager {
	t.Helper()
	mgr, err := jobs.New(jobs.Options{Dir: t.TempDir(), Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	t.Cleanup(mgr.Close)
	return mgr
}

// TestWorkspaceToolSet_WithholdsBackgroundToolsByPolicy covers the rule the
// repository follows everywhere: a model that cannot use a tool should not be
// offered it, because "the call was refused" is a worse answer than a tool that
// was never on the menu.
func TestWorkspaceToolSet_WithholdsBackgroundToolsByPolicy(t *testing.T) {
	cases := []struct {
		name       string
		readOnly   bool
		enableBash bool
		enableJob  bool
		manager    bool
		want       bool
	}{
		{name: "everything on", enableBash: true, enableJob: true, manager: true, want: true},
		{name: "background off", enableBash: true, enableJob: false, manager: true, want: false},
		{name: "bash off", enableBash: false, enableJob: true, manager: true, want: false},
		{name: "read-only workspace", readOnly: true, enableBash: true, enableJob: true, manager: true, want: false},
		{name: "no supervisor", enableBash: true, enableJob: true, manager: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Tools.ReadOnly = tc.readOnly
			cfg.Tools.EnableBash = tc.enableBash
			cfg.Tools.EnableBackground = tc.enableJob

			var mgr *jobs.Manager
			if tc.manager {
				mgr = newBackgroundTestManager(t)
			}
			set := newWorkspaceToolSet(cfg, newMediaTestStore(t), zap.NewNop(),
				toolSetOptions{Jobs: mgr, Surface: "web"})
			ws := newMediaTestWorkspace(t, workspace.Options{ReadOnly: tc.readOnly})
			cfg.Tools.Workspace = ws.Root()

			built, err := set.build(ws, "test")
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			names := map[string]bool{}
			for _, tl := range built {
				names[nameOf(tl)] = true
			}
			for _, name := range backgroundToolNames() {
				if names[name] != tc.want {
					t.Errorf("%s registered = %v, want %v (tools: %v)", name, names[name], tc.want, names)
				}
			}
			if !tc.want {
				return
			}
			// Registered names must also be listed as workspace-bound, or a
			// per-turn clone would keep a tool bound to another workspace.
			bound := map[string]bool{}
			for _, name := range workspaceBoundToolNames() {
				bound[name] = true
			}
			for _, name := range backgroundToolNames() {
				if !bound[name] {
					t.Errorf("%s is registered but not in workspaceBoundToolNames: a clone would not rebind it", name)
				}
			}
		})
	}
}

// TestWorkspaceToolSet_BackgroundJobRecordsItsWorkspaceAndSurface checks the
// plumbing from the tool options through to the job record the console reads.
func TestWorkspaceToolSet_BackgroundJobRecordsItsWorkspaceAndSurface(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.EnableBash = true
	cfg.Tools.EnableBackground = true
	mgr := newBackgroundTestManager(t)

	set := newWorkspaceToolSet(cfg, newMediaTestStore(t), zap.NewNop(),
		toolSetOptions{Jobs: mgr, Surface: "feishu"})
	ws := newMediaTestWorkspace(t, workspace.Options{})
	cfg.Tools.Workspace = ws.Root()

	built, err := set.build(ws, "blog")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var start *builtin.BackgroundStartOutput
	for _, tl := range built {
		if nameOf(tl) != builtin.BackgroundToolName {
			continue
		}
		raw, err := tl.InvokableRun(context.Background(), `{"command":"sleep 30","name":"watcher"}`)
		if err != nil {
			t.Fatalf("bash_background: %v", err)
		}
		var out builtin.BackgroundStartOutput
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode result %q: %v", raw, err)
		}
		start = &out
	}
	if start == nil {
		t.Fatal("bash_background was not registered")
	}

	job, err := mgr.Get(start.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", start.ID, err)
	}
	if job.Workspace != "blog" {
		t.Errorf("workspace = %q, want the label the turn was bound to", job.Workspace)
	}
	if job.WorkspaceRoot != ws.Root() {
		t.Errorf("workspace_root = %q, want %q", job.WorkspaceRoot, ws.Root())
	}
	if job.RequestedBy != "feishu" {
		t.Errorf("requested_by = %q, want the surface the tools were built for", job.RequestedBy)
	}
	if job.Name != "watcher" {
		t.Errorf("name = %q, want the label the model sent", job.Name)
	}
}
