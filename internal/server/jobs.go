package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"syscall"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/workspaces"
)

// Background-process endpoints.
//
// The console can see and stop what the agent started, which is the other half of
// making resident processes a feature rather than a leak: a dev server the model
// started three messages ago is otherwise findable only with ps, and stoppable
// only by guessing at a pid.
//
// The routes are registered even when no manager is wired: a UI that asks should
// be told "background jobs are off" rather than get a 404, which is
// indistinguishable from a broken deployment. (The trace endpoints make the same
// choice, for the same reason.)
func (s *Server) registerJobRoutes(authed *route.RouterGroup) {
	authed.GET("/jobs", s.handleListJobs)
	authed.GET("/jobs/:id", s.handleJobOutput)
	authed.POST("/jobs/:id/stop", s.handleStopJob)
	authed.DELETE("/jobs/:id", s.handleForgetJob)
}

// jobOutputDefaultBytes is how much of a job's log one console read returns when
// it does not ask for a size: enough for a screenful of a server's output without
// pulling a whole log into a browser.
const jobOutputDefaultBytes = 64 << 10

// handleListJobs lists every job this process supervises.
//
// `?session=<id>` additionally answers with the scope key that session's jobs are
// recorded under. The conversation is what a job belongs to — the header of a
// session shows its own processes — and the translation from a session id to that
// key lives here, so the browser never has to know how a scope is spelled (or
// that one exists at all). Every job is still listed either way: the console
// shows a session its own jobs first and the rest of the workspace's below them,
// which is one request rather than two.
func (s *Server) handleListJobs(_ context.Context, c *app.RequestContext) {
	if s.jobs == nil {
		c.JSON(http.StatusOK, map[string]any{
			"enabled": false,
			"jobs":    []any{},
			"message": "后台进程未启用：本进程没有作业管理器（tools.enable_background 关闭或日志目录不可用）",
		})
		return
	}
	list := s.jobs.List()
	opts := s.jobs.Options()
	body := map[string]any{
		"enabled":  true,
		"dir":      opts.Dir,
		"max_jobs": opts.MaxJobs,
		"running":  s.jobs.Running(),
		"total":    len(list),
		"jobs":     list,
	}
	if id := strings.TrimSpace(c.Query("session")); id != "" {
		body["scope"] = workspaces.WebScope(id)
	}
	c.JSON(http.StatusOK, body)
}

// handleJobOutput returns one window of a job's output.
//
// Reading by offset is what lets the log view follow a running process without
// re-fetching everything it has ever printed; the response carries next for the
// caller to continue from, and dropped_bytes so a gap is reported rather than
// silently shown as a shorter stream.
func (s *Server) handleJobOutput(ctx context.Context, c *app.RequestContext) {
	if s.jobs == nil {
		c.JSON(http.StatusOK, map[string]any{"enabled": false, "output": ""})
		return
	}
	id := c.Param("id")
	job, err := s.jobs.Get(id)
	if err != nil {
		writeJobError(c, err)
		return
	}
	from, _ := strconv.ParseInt(c.Query("from"), 10, 64)
	maxBytes := 0
	if v := c.Query("max_bytes"); v != "" {
		maxBytes, _ = strconv.Atoi(v)
	}
	if maxBytes <= 0 {
		maxBytes = jobOutputDefaultBytes
	}
	read, err := s.jobs.Output(ctx, id, jobs.ReadOptions{From: from, MaxBytes: maxBytes})
	if err != nil {
		writeJobError(c, err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"job":           job,
		"output":        read.Data,
		"from":          read.From,
		"next":          read.Next,
		"total_bytes":   read.TotalBytes,
		"dropped_bytes": read.DiscardedBytes,
		"skipped_bytes": read.SkippedBytes,
		"output_open":   read.OutputOpen,
		"log_capped":    job.LogCapped,
	})
}

// handleStopJob stops a running job: SIGTERM to its process group, then SIGKILL
// after the configured grace.
func (s *Server) handleStopJob(ctx context.Context, c *app.RequestContext) {
	if s.jobs == nil {
		c.JSON(http.StatusConflict, map[string]string{"error": "后台进程未启用：本进程没有作业管理器"})
		return
	}
	id := c.Param("id")

	// The body is optional: a stop with no body means "term", which is what the
	// panel's button sends.
	var body struct {
		Signal string `json:"signal"`
	}
	if raw := c.Request.Body(); len(raw) > 0 {
		_ = c.BindJSON(&body)
	}
	signal := syscall.SIGTERM
	if strings.EqualFold(strings.TrimSpace(body.Signal), "kill") {
		signal = syscall.SIGKILL
	}

	job, err := s.jobs.Stop(ctx, id, signal, 0)
	if err != nil {
		writeJobError(c, err)
		return
	}
	out := map[string]any{"job": job, "stopped": true}
	// The tail of what it printed before it stopped is where a crash on shutdown
	// explains itself, and it costs one read of an already-buffered window.
	if read, rerr := s.jobs.Output(ctx, id, jobs.ReadOptions{MaxBytes: 8 << 10}); rerr == nil {
		out["output"] = read.Data
	}
	c.JSON(http.StatusOK, out)
}

// handleForgetJob drops a finished job's record and deletes its log file.
//
// A running job is refused with 409 rather than stopped: the record and the log
// are how a running process can be found and stopped, so deleting them while it
// lives would hide a process the operator cannot then reach.
func (s *Server) handleForgetJob(_ context.Context, c *app.RequestContext) {
	if s.jobs == nil {
		c.JSON(http.StatusConflict, map[string]string{"error": "后台进程未启用：本进程没有作业管理器"})
		return
	}
	job, err := s.jobs.Forget(c.Param("id"))
	if err != nil {
		writeJobError(c, err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"job": job, "forgotten": true})
}

// writeJobError maps the manager's sentinel errors onto HTTP statuses.
func writeJobError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, jobs.ErrUnknownJob):
		c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, jobs.ErrStillRunning):
		c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, jobs.ErrNotRunning):
		c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, jobs.ErrClosed):
		c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}
