package server

import (
	"context"
	"testing"

	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/workspaces"
)

// The session a job belongs to travels with it, and the list endpoint tells the
// caller which scope key that session's jobs are recorded under. Both halves
// matter to the console: the header of a conversation shows its own processes,
// and the browser is not supposed to know how a session id becomes a scope.
func TestJobsAPI_SessionScopeIsReportedAndCarriedByJobs(t *testing.T) {
	h, mgr := newJobsHarness(t, true)
	h.login(t)

	const sessionID = "11111111-2222-3333-4444-555555555555"
	scope := workspaces.WebScope(sessionID)
	cwd := t.TempDir()

	mine, err := mgr.Start(context.Background(), jobs.Spec{
		Command: "sleep 30", Shell: "/bin/sh", ShellArgs: []string{"-c"}, Cwd: cwd,
		Workspace: "test", WorkspaceRoot: cwd, RequestedBy: "web", Scope: scope,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// A job from another conversation in the same workspace: the console shows
	// it too, but not as this session's.
	if _, err := mgr.Start(context.Background(), jobs.Spec{
		Command: "sleep 30", Shell: "/bin/sh", ShellArgs: []string{"-c"}, Cwd: cwd,
		Workspace: "test", WorkspaceRoot: cwd, RequestedBy: "web",
		Scope: workspaces.WebScope("another-session"),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var body struct {
		jobsListResponse
		Scope string `json:"scope"`
	}
	resp, err := h.client.Get(h.base + "/api/jobs?session=" + sessionID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	decodeOK(t, resp, 200, &body)

	if body.Scope != scope {
		t.Errorf("scope = %q, want %q", body.Scope, scope)
	}
	if body.Total != 2 {
		t.Fatalf("total = %d, want both jobs listed (the client groups them)", body.Total)
	}

	var found bool
	for _, job := range body.Jobs {
		if job.ID != mine.ID {
			continue
		}
		found = true
		if job.Scope != scope {
			t.Errorf("job scope = %q, want %q", job.Scope, scope)
		}
	}
	if !found {
		t.Error("the started job is missing from the list")
	}

	// Without a session the answer carries no scope at all: an unset key and a
	// key that happens to be empty must not be the same thing to a client that
	// groups by it.
	plain, err := h.client.Get(h.base + "/api/jobs")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var raw map[string]any
	decodeOK(t, plain, 200, &raw)
	if _, present := raw["scope"]; present {
		t.Errorf("scope is present without ?session: %v", raw["scope"])
	}
}
