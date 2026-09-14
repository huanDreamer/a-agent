package server

// Tests for the console's background-process surface: what the agent started must
// be visible, readable and stoppable from the console, because a dev server found
// only with ps is a leak with a nicer name.

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/jobs"
)

// newJobsHarness starts a console wired to a real job manager, and returns both.
func newJobsHarness(t *testing.T, withManager bool) (*harness, *jobs.Manager) {
	t.Helper()

	var mgr *jobs.Manager
	if withManager {
		m, err := jobs.New(jobs.Options{
			Dir:        t.TempDir(),
			StartGrace: 50 * time.Millisecond,
			StopGrace:  time.Second,
			Logger:     zap.NewNop(),
		})
		if err != nil {
			t.Fatalf("jobs.New: %v", err)
		}
		t.Cleanup(m.Close)
		mgr = m
	}
	return newJobsHarnessWith(t, mgr)
}

// newJobsHarnessWith starts a console over an existing manager. A nil manager is
// the deployment that turned background jobs off.
func newJobsHarnessWith(t *testing.T, mgr *jobs.Manager) (*harness, *jobs.Manager) {
	t.Helper()
	srv, st := buildServerWith(t, buildOpts{jobs: mgr})
	startHarness(t, srv)
	return &harness{
		base:   "http://" + srv.Addr(),
		client: newJar(t),
		srv:    srv,
		store:  st,
	}, mgr
}

// decodeOK checks a response's status and decodes its body in one step.
// requireStatus closes the body, so the two cannot be combined.
func decodeOK(t *testing.T, resp *http.Response, want int, v any) {
	t.Helper()
	if resp.StatusCode != want {
		defer func() { _ = resp.Body.Close() }()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, want, buf.String())
	}
	decode(t, resp, v)
}

// startJobForTest starts one job through the manager, which is what the model's
// bash_background tool does.
func startJobForTest(t *testing.T, mgr *jobs.Manager, command string) *jobs.Job {
	t.Helper()
	cwd := t.TempDir()
	job, err := mgr.Start(context.Background(), jobs.Spec{
		Command: command, Shell: "/bin/sh", ShellArgs: []string{"-c"}, Cwd: cwd,
		Workspace: "test", WorkspaceRoot: cwd, RequestedBy: "web",
	})
	if err != nil {
		t.Fatalf("Start(%q): %v", command, err)
	}
	return job
}

// jobsListResponse mirrors the shape the console panel consumes.
type jobsListResponse struct {
	Enabled bool       `json:"enabled"`
	Dir     string     `json:"dir"`
	MaxJobs int        `json:"max_jobs"`
	Running int        `json:"running"`
	Total   int        `json:"total"`
	Message string     `json:"message"`
	Jobs    []jobs.Job `json:"jobs"`
}

// TestJobsAPI_ListReadStopForget walks the whole surface the panel uses.
func TestJobsAPI_ListReadStopForget(t *testing.T) {
	h, mgr := newJobsHarness(t, true)
	h.login(t)

	running := startJobForTest(t, mgr, "echo listening; sleep 30")

	var list jobsListResponse
	h.getJSON(t, "/api/jobs", http.StatusOK, &list)
	if !list.Enabled {
		t.Fatal("the console reports background jobs as disabled")
	}
	if list.Dir == "" || list.MaxJobs <= 0 {
		t.Errorf("list = %+v, want the directory and the job limit", list)
	}
	if list.Total != 1 || list.Running != 1 || len(list.Jobs) != 1 {
		t.Fatalf("list = %+v, want one running job", list)
	}
	if got := list.Jobs[0]; got.ID != running.ID || got.Workspace != "test" || got.RequestedBy != "web" {
		t.Errorf("job = %+v, want the job that was started", got)
	}
	if list.Jobs[0].ExitCode != nil {
		t.Errorf("a running job reports exit code %v", *list.Jobs[0].ExitCode)
	}

	// The output window, read the way the log view reads it.
	var out struct {
		Job          jobs.Job `json:"job"`
		Output       string   `json:"output"`
		From         int64    `json:"from"`
		Next         int64    `json:"next"`
		TotalBytes   int64    `json:"total_bytes"`
		DroppedBytes int64    `json:"dropped_bytes"`
	}
	h.getJSON(t, "/api/jobs/"+running.ID, http.StatusOK, &out)
	if !strings.Contains(out.Output, "listening") {
		t.Fatalf("output = %q, want the line the process printed", out.Output)
	}
	if out.Next <= 0 || out.TotalBytes <= 0 {
		t.Errorf("offsets = %+v, want a non-zero next offset", out)
	}

	// Following from next is what the polling log view does; with nothing new it
	// must return an empty window and the same offset.
	var follow struct {
		Output string `json:"output"`
		Next   int64  `json:"next"`
	}
	h.getJSON(t, "/api/jobs/"+running.ID+"?from="+strconv.FormatInt(out.Next, 10), http.StatusOK, &follow)
	if follow.Output != "" {
		t.Errorf("follow-up read = %q, want nothing new", follow.Output)
	}
	if follow.Next != out.Next {
		t.Errorf("offset moved from %d to %d without new output", out.Next, follow.Next)
	}

	// An id this process never issued is a 404 with a message, not a 200 with an
	// empty panel.
	var missing map[string]any
	h.getJSON(t, "/api/jobs/job-000000", http.StatusNotFound, &missing)
	if missing["error"] == nil {
		t.Errorf("404 body = %+v, want an error message", missing)
	}

	// Stop it, and read the tail that comes back with the result.
	resp := h.postJSON(t, "/api/jobs/"+running.ID+"/stop", map[string]string{"signal": "term"})
	var stopped struct {
		Job     jobs.Job `json:"job"`
		Stopped bool     `json:"stopped"`
		Output  string   `json:"output"`
	}
	decodeOK(t, resp, http.StatusOK, &stopped)
	if !stopped.Stopped || stopped.Job.Status != jobs.StatusStopped {
		t.Fatalf("stop = %+v, want a stopped job", stopped)
	}
	if stopped.Job.ExitCode == nil {
		t.Error("a stopped job has no exit code")
	}

	// Stopping it again is a conflict: the panel must not be told it stopped
	// something that was already over.
	requireStatus(t, h.postJSON(t, "/api/jobs/"+running.ID+"/stop", nil), http.StatusConflict)

	// Forget drops the record and the log file.
	resp = h.deleteJSON(t, "/api/jobs/"+running.ID)
	var forgotten struct {
		Forgotten bool `json:"forgotten"`
	}
	decodeOK(t, resp, http.StatusOK, &forgotten)
	if !forgotten.Forgotten {
		t.Error("forget did not report success")
	}
	if _, err := os.Stat(running.LogPath); !os.IsNotExist(err) {
		t.Errorf("the log file survived forget (err = %v)", err)
	}
	h.getJSON(t, "/api/jobs/"+running.ID, http.StatusNotFound, nil)
}

// TestJobsAPI_ForgetRefusesARunningJob is the refusal that protects the
// operator: deleting the record of a live process would hide something nothing
// can stop afterwards.
func TestJobsAPI_ForgetRefusesARunningJob(t *testing.T) {
	h, mgr := newJobsHarness(t, true)
	h.login(t)

	job := startJobForTest(t, mgr, "sleep 30")
	requireStatus(t, h.deleteJSON(t, "/api/jobs/"+job.ID), http.StatusConflict)

	// A stop with no body at all is what the panel's button sends: it must mean
	// "term", not "bad request".
	resp := h.postJSON(t, "/api/jobs/"+job.ID+"/stop", nil)
	var stopped struct {
		Stopped bool `json:"stopped"`
	}
	decodeOK(t, resp, http.StatusOK, &stopped)
	if !stopped.Stopped {
		t.Errorf("a body-less stop did not stop the job: %+v", stopped)
	}

	var list jobsListResponse
	h.getJSON(t, "/api/jobs", http.StatusOK, &list)
	if list.Total != 1 {
		t.Errorf("the refused delete dropped the record: %+v", list)
	}
}

// TestJobsAPI_StopAcceptsAKillSignal checks the forced path, since a process that
// ignores SIGTERM is the reason it exists.
func TestJobsAPI_StopAcceptsAKillSignal(t *testing.T) {
	h, mgr := newJobsHarness(t, true)
	h.login(t)

	job := startJobForTest(t, mgr, "trap '' TERM; sleep 30")
	resp := h.postJSON(t, "/api/jobs/"+job.ID+"/stop", map[string]string{"signal": "kill"})
	var stopped struct {
		Job     jobs.Job `json:"job"`
		Stopped bool     `json:"stopped"`
	}
	decodeOK(t, resp, http.StatusOK, &stopped)
	if !stopped.Stopped || stopped.Job.Status != jobs.StatusStopped {
		t.Fatalf("stop = %+v, want a stopped job", stopped)
	}
}

// TestJobsAPI_DisabledExplainsItself covers a deployment with no supervisor: the
// console must be told why, rather than showing an error it cannot act on.
func TestJobsAPI_DisabledExplainsItself(t *testing.T) {
	h, _ := newJobsHarness(t, false)

	// The routes sit behind the session check like every other console route.
	requireStatus(t, h.get(t, "/api/jobs"), http.StatusUnauthorized)

	h.login(t)
	var list jobsListResponse
	h.getJSON(t, "/api/jobs", http.StatusOK, &list)
	if list.Enabled {
		t.Error("the console reports background jobs as enabled with no manager")
	}
	if len(list.Jobs) != 0 {
		t.Errorf("jobs = %+v, want none", list.Jobs)
	}
	if list.Message == "" {
		t.Error("the disabled response says nothing about why")
	}

	requireStatus(t, h.postJSON(t, "/api/jobs/job-000000/stop", nil), http.StatusConflict)
	requireStatus(t, h.deleteJSON(t, "/api/jobs/job-000000"), http.StatusConflict)
}

// TestJobsAPI_UnknownIdIs404 checks the mapping from the manager's sentinel
// errors onto statuses, so the panel can tell "that job is gone" from "the
// request was wrong" from "the server broke".
func TestJobsAPI_UnknownIdIs404(t *testing.T) {
	mgr, err := jobs.New(jobs.Options{Dir: t.TempDir(), Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	h, _ := newJobsHarnessWith(t, mgr)
	h.login(t)

	for _, path := range []string{"/api/jobs/job-000000"} {
		var body map[string]any
		h.getJSON(t, path, http.StatusNotFound, &body)
		if body["error"] == nil {
			t.Errorf("GET %s: body = %+v, want an error message", path, body)
		}
	}
	requireStatus(t, h.postJSON(t, "/api/jobs/job-000000/stop", nil), http.StatusNotFound)
	requireStatus(t, h.deleteJSON(t, "/api/jobs/job-000000"), http.StatusNotFound)
}
