package store

import (
	"context"
	"fmt"
)

// migration represents a single ordered schema migration.
type migration struct {
	version int
	name    string
	up      string
}

// migrations is the ordered list applied at Open() time. Append new entries;
// never mutate or remove an existing one.
var migrations = []migration{
	{
		version: 1,
		name:    "create_usage_logs",
		up: `CREATE TABLE IF NOT EXISTS usage_logs (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id       TEXT    NOT NULL,
			provider         TEXT    NOT NULL,
			model            TEXT    NOT NULL,
			prompt_tokens    INTEGER NOT NULL,
			completion_tokens INTEGER NOT NULL,
			total_tokens     INTEGER NOT NULL,
			duration_ms      INTEGER NOT NULL,
			created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_usage_session  ON usage_logs(session_id);
		CREATE INDEX IF NOT EXISTS idx_usage_provider ON usage_logs(provider);
		CREATE INDEX IF NOT EXISTS idx_usage_created  ON usage_logs(created_at);`,
	},
	{
		version: 2,
		name:    "create_tool_invocations",
		up: `CREATE TABLE IF NOT EXISTS tool_invocations (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id  TEXT    NOT NULL,
			tool_name   TEXT    NOT NULL,
			arguments   TEXT    NOT NULL,
			result      TEXT    NOT NULL DEFAULT '',
			err         TEXT    NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_inv_session ON tool_invocations(session_id);
		CREATE INDEX IF NOT EXISTS idx_inv_tool    ON tool_invocations(tool_name);
		CREATE INDEX IF NOT EXISTS idx_inv_created ON tool_invocations(created_at);`,
	},
	{
		version: 3,
		name:    "add_user_attribution",
		// NOTE: ALTER TABLE ... ADD COLUMN is not idempotent in SQLite, but
		// Migrate guards on schema_migrations and applies each version at most
		// once, inside a single transaction (SQLite DDL is transactional, so a
		// failure rolls the whole migration back). Pre-existing rows get the
		// empty-string default, i.e. "unattributed".
		up: `ALTER TABLE usage_logs ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
		ALTER TABLE tool_invocations ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
		CREATE INDEX IF NOT EXISTS idx_usage_user ON usage_logs(user_id);
		CREATE INDEX IF NOT EXISTS idx_inv_user   ON tool_invocations(user_id);`,
	},
	{
		version: 4,
		name:    "create_chat_sessions",
		up: `CREATE TABLE IF NOT EXISTS chat_sessions (
			id          TEXT PRIMARY KEY,
			title       TEXT NOT NULL DEFAULT '',
			user_id     TEXT NOT NULL DEFAULT '',
			provider    TEXT NOT NULL DEFAULT '',
			model       TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_chat_sessions_updated ON chat_sessions(updated_at);
		CREATE INDEX IF NOT EXISTS idx_chat_sessions_user    ON chat_sessions(user_id);

		CREATE TABLE IF NOT EXISTS chat_messages (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id  TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
			role        TEXT NOT NULL,
			content     TEXT NOT NULL DEFAULT '',
			reasoning   TEXT NOT NULL DEFAULT '',
			tool_calls  TEXT NOT NULL DEFAULT '',
			tool_call_id TEXT NOT NULL DEFAULT '',
			tool_name   TEXT NOT NULL DEFAULT '',
			usage_json  TEXT NOT NULL DEFAULT '',
			error       TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id, id);`,
	},
	{
		version: 5,
		name:    "create_llm_catalog",
		up: `CREATE TABLE IF NOT EXISTS llm_providers (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL DEFAULT '',
			base_url     TEXT NOT NULL DEFAULT '',
			api_key      TEXT NOT NULL DEFAULT '',
			api_key_env  TEXT NOT NULL DEFAULT '',
			kind         TEXT NOT NULL DEFAULT 'openai',
			source       TEXT NOT NULL DEFAULT 'user',
			enabled      INTEGER NOT NULL DEFAULT 1,
			last_error   TEXT NOT NULL DEFAULT '',
			created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS llm_models (
			provider_id  TEXT NOT NULL REFERENCES llm_providers(id) ON DELETE CASCADE,
			model_id     TEXT NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			capabilities TEXT NOT NULL DEFAULT '',
			enabled      INTEGER NOT NULL DEFAULT 1,
			source       TEXT NOT NULL DEFAULT 'fetched',
			fetched_at   TIMESTAMP,
			PRIMARY KEY (provider_id, model_id)
		);
		CREATE INDEX IF NOT EXISTS idx_llm_models_provider ON llm_models(provider_id);

		CREATE TABLE IF NOT EXISTS llm_bindings (
			capability   TEXT PRIMARY KEY,
			provider_id  TEXT NOT NULL DEFAULT '',
			model_id     TEXT NOT NULL DEFAULT '',
			updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS media_assets (
			id           TEXT PRIMARY KEY,
			session_id   TEXT NOT NULL DEFAULT '',
			kind         TEXT NOT NULL DEFAULT '',
			path         TEXT NOT NULL DEFAULT '',
			mime         TEXT NOT NULL DEFAULT '',
			bytes        INTEGER NOT NULL DEFAULT 0,
			sha256       TEXT NOT NULL DEFAULT '',
			created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_media_session ON media_assets(session_id);`,
	},
	{
		version: 6,
		name:    "add_message_attachments",
		// Separate from the migration that created chat_messages rather than
		// appended to it: a database that already applied that version would
		// never run the added statement.
		//
		// Attachments are stored as a JSON array of asset ids on the message, so
		// a reloaded conversation still shows the image that was sent with it —
		// the file itself lives in media_assets.
		up: `ALTER TABLE chat_messages ADD COLUMN attachments TEXT NOT NULL DEFAULT '';`,
	},
	{
		version: 7,
		name:    "create_traces",
		// The trace store is what makes 链路追踪 work without a Langfuse server.
		//
		//   - `traces` is one agent turn: its name, who ran it, which session it
		//     belongs to, its input/output, and the window it ran in. `ended_at`
		//     is NULL while the turn is still going, which is what lets a reader
		//     tell "in progress" from "finished instantly" — a distinction the
		//     waterfall needs and a zero duration cannot express.
		//   - `observations` is one node of that turn: a GENERATION (a model call)
		//     or a SPAN (a tool call). It nests through `parent_id`, and its own
		//     start/end are the only inputs the waterfall geometry uses.
		//   - `chat_messages.trace_id` links an assistant answer to the trace that
		//     produced it, so a conversation can offer a way into its own trace.
		//     Rows written before this migration default to '' = "no trace
		//     recorded", which the UI renders as "no link" rather than a broken
		//     one.
		//
		// `observations.trace_id` cascades, so pruning or clearing a trace cannot
		// leave orphaned nodes behind. Timestamps are always supplied by the
		// caller in UTC rather than defaulted here, so every row in these tables
		// carries the same format and `ORDER BY started_at` sorts correctly.
		//
		// ALTER TABLE ... ADD COLUMN is not idempotent in SQLite, but Migrate
		// guards on schema_migrations and applies each version at most once
		// (see migration 3).
		up: `CREATE TABLE IF NOT EXISTS traces (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			user_id    TEXT NOT NULL DEFAULT '',
			input      TEXT NOT NULL DEFAULT '',
			output     TEXT NOT NULL DEFAULT '',
			started_at TIMESTAMP NOT NULL,
			ended_at   TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_traces_started ON traces(started_at);
		CREATE INDEX IF NOT EXISTS idx_traces_session ON traces(session_id);
		CREATE INDEX IF NOT EXISTS idx_traces_user    ON traces(user_id);

		CREATE TABLE IF NOT EXISTS observations (
			id                TEXT PRIMARY KEY,
			trace_id          TEXT NOT NULL REFERENCES traces(id) ON DELETE CASCADE,
			parent_id         TEXT NOT NULL DEFAULT '',
			type              TEXT NOT NULL DEFAULT '',
			name              TEXT NOT NULL DEFAULT '',
			model             TEXT NOT NULL DEFAULT '',
			step              INTEGER NOT NULL DEFAULT 0,
			input             TEXT NOT NULL DEFAULT '',
			output            TEXT NOT NULL DEFAULT '',
			level             TEXT NOT NULL DEFAULT '',
			status_message    TEXT NOT NULL DEFAULT '',
			prompt_tokens     INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens      INTEGER NOT NULL DEFAULT 0,
			started_at        TIMESTAMP NOT NULL,
			ended_at          TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_obs_trace   ON observations(trace_id, started_at);
		CREATE INDEX IF NOT EXISTS idx_obs_started ON observations(started_at);

		ALTER TABLE chat_messages ADD COLUMN trace_id TEXT NOT NULL DEFAULT '';`,
	},
	{
		version: 8,
		name:    "create_mcp_servers",
		// MCP servers were config-file only until the console grew an MCP tab:
		// the agent read `mcp.servers` at process start, so adding one meant
		// editing YAML and restarting, and the web console could neither show
		// what was configured nor test whether it worked.
		//
		//   - `transport` is which client to build: stdio spawns `command` with
		//     `args` (the original and still the common case), sse and http dial
		//     `url`. The two HTTP transports answer different protocols (legacy
		//     SSE vs streamable HTTP), so they are separate values rather than a
		//     single "remote".
		//   - `args`, `env` and `headers` are JSON arrays of strings. A TEXT
		//     column rather than child tables: they are short, only ever read
		//     and written whole, and a join would buy nothing.
		//   - `source` mirrors llm_providers: a `config` row is re-synced from
		//     the config file at every start, so the UI shows it as read-only
		//     instead of offering an edit the next restart would undo.
		//   - `last_error` is written by the runtime manager on a failed connect,
		//     so a broken server is visible without re-testing it.
		//
		// Secret-looking env values are stored as given: unlike an LLM API key
		// they are an arbitrary KEY=value list the operator wrote, and the
		// connection needs them verbatim.
		up: `CREATE TABLE IF NOT EXISTS mcp_servers (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL DEFAULT '',
			transport  TEXT NOT NULL DEFAULT 'stdio',
			command    TEXT NOT NULL DEFAULT '',
			args       TEXT NOT NULL DEFAULT '',
			env        TEXT NOT NULL DEFAULT '',
			url        TEXT NOT NULL DEFAULT '',
			headers    TEXT NOT NULL DEFAULT '',
			source     TEXT NOT NULL DEFAULT 'user',
			enabled    INTEGER NOT NULL DEFAULT 1,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
	},
	{
		version: 9,
		name:    "create_workspaces",
		// Named workspaces, and which one each scope is in.
		//
		// Until this migration the agent had exactly one root, taken from
		// `tools.workspace`: switching project meant editing the config and
		// restarting. These two tables make the set of roots, and the selection,
		// runtime state.
		//
		//   - `workspaces.root` is the absolute directory the sandbox confines a
		//     turn to. It is stored rather than derived from the name so a row
		//     stays meaningful if `tools.workspaces_dir` is later changed; the
		//     name is the identity, the root is where it points today.
		//   - `read_only` / `enable_bash` are per workspace on purpose: "look at
		//     this repo" and "build me a prototype" want different permissions
		//     and should not share one global switch.
		//   - The built-in "default" workspace is NOT a row: it is synthesised
		//     from `tools.workspace`, so an existing deployment keeps pointing at
		//     its configured root without anything having to be migrated, and it
		//     cannot be deleted out from under the operator.
		//   - `workspace_bindings` keys a choice by scope (`web:<session>` /
		//     `feishu:<open_id>`), never globally: two conversations may work in
		//     two projects at the same time and must not fight over one pointer.
		//     A missing row means "the default workspace", so no scope needs a
		//     row to work.
		//
		// The two ALTERs put the workspace on the records that would otherwise
		// be ambiguous after a switch:
		//
		//   - `tool_invocations.workspace` is what lets the audit answer "which
		//     project did this edit touch" without inferring it from the
		//     arguments (which are workspace-relative and identical across
		//     workspaces).
		//   - `media_assets.workspace` is a correctness requirement, not
		//     metadata: `path` is workspace-relative, so an attachment must be
		//     read back through the workspace it was stored in — otherwise the
		//     same relative path in a newly selected workspace resolves to a
		//     different file, or to none.
		up: `CREATE TABLE IF NOT EXISTS workspaces (
			name        TEXT PRIMARY KEY,
			root        TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			read_only   INTEGER NOT NULL DEFAULT 0,
			enable_bash INTEGER NOT NULL DEFAULT 1,
			created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS workspace_bindings (
			scope      TEXT PRIMARY KEY,
			workspace  TEXT NOT NULL,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_ws_bindings_workspace ON workspace_bindings(workspace);
		-- Names are resolved case-insensitively (a person typing "Blog" at the
		-- console means the workspace called "blog"), so uniqueness has to be
		-- case-insensitive too — otherwise two rows would both answer to one
		-- name and which one wins would depend on the query.
		CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_lower_name ON workspaces(lower(name));

		ALTER TABLE tool_invocations ADD COLUMN workspace TEXT NOT NULL DEFAULT '';
		ALTER TABLE media_assets     ADD COLUMN workspace TEXT NOT NULL DEFAULT '';`,
	},
	{
		version: 10,
		name:    "workspaces_become_directories",
		// A workspace stopped being "a directory this process creates under a base
		// dir, with a policy" and became "a directory the operator picked", with no
		// policy at all: read_only / enable_bash / limits are process-wide again
		// (`tools.*`), because a workspace answers *where* the agent works and not
		// *what it may do*.
		//
		// So the three policy columns go, and `name` is demoted from "the directory
		// name" to a label the sidebar shows and the operator can rename. `root` is
		// untouched, which is what makes this safe for a database that already holds
		// rows: the directories those rows name keep pointing exactly where they did.
		//
		// The unique index on lower(name) stays: a name is how a scope refers to a
		// workspace, and two rows answering to one name would make which-one-wins
		// depend on the query.
		//
		// DROP COLUMN needs SQLite 3.35+ (modernc.org/sqlite is far past that), and
		// refuses a column that is indexed or part of a constraint — none of these
		// three is.
		up: `ALTER TABLE workspaces DROP COLUMN description;
		ALTER TABLE workspaces DROP COLUMN read_only;
		ALTER TABLE workspaces DROP COLUMN enable_bash;`,
	},
	{
		version: 11,
		name:    "chat_message_stop_reason",
		// A turn can end because a budget ran out rather than because the model
		// answered, and that distinction used to exist only inside the answer
		// text. Persisting it lets the console still mark the turn after a
		// reload, and lets a reader tell "the answer is complete" from "the
		// answer is what fit in the budget".
		//
		// Empty is the ordinary case, and the value every existing row gets: the
		// model answered on its own.
		up: `ALTER TABLE chat_messages ADD COLUMN stop_reason TEXT NOT NULL DEFAULT '';`,
	},
	{
		version: 12,
		name:    "create_app_settings",
		// A small key/value table for the settings the console may change while
		// the process runs. It is deliberately generic: the first tenant is the
		// per-turn budget (设置 → 对话预算), and a second one should not need
		// another table and another migration.
		//
		// Only an explicit override is stored. An absent key means "use the value
		// from config.yaml", which is what makes 恢复默认 a DELETE instead of a
		// copy of the startup default — and what lets an edit of config.yaml stay
		// visible to a console that never overrode that key.
		up: `CREATE TABLE IF NOT EXISTS app_settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
	},
	{
		version: 13,
		name:    "chat_message_steps",
		// The turn broken down by iteration: each step's reasoning, what it said,
		// and the tool calls it asked for. A turn used to be stored as one blob of
		// reasoning plus a flat array of tool calls, which is exactly the pairing a
		// reader needs and exactly what that shape loses — nothing in it says which
		// thought asked for which call.
		//
		// reasoning and tool_calls are still written: the audit log, the session
		// statistics and older clients read them. This column is what the console
		// renders, and an empty value means "no step information" (a row written
		// before this migration), which the console falls back from.
		up: `ALTER TABLE chat_messages ADD COLUMN steps TEXT NOT NULL DEFAULT '';`,
	},
	{
		version: 14,
		name:    "create_chat_plans",
		// The task plan a conversation's model maintains through the plan_* tools,
		// and the thing the console renders above the composer.
		//
		// It is one row per conversation holding the whole plan as JSON, rather
		// than a table of tasks, because a plan is read and written as a unit: the
		// console renders all of it, the model rewrites all of it, and nothing ever
		// queries one task. A task table would add joins and a second ordering
		// column to a document that has neither.
		//
		// It is the one piece of turn state that outlives its turn on purpose:
		// "what have I already done" is exactly what a turn that died cannot
		// answer, and what the next one needs in order not to start over.
		up: `CREATE TABLE IF NOT EXISTS chat_plans (
			session_id TEXT PRIMARY KEY REFERENCES chat_sessions(id) ON DELETE CASCADE,
			goal       TEXT NOT NULL DEFAULT '',
			tasks      TEXT NOT NULL DEFAULT '[]',
			revision   INTEGER NOT NULL DEFAULT 0,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
	},
	{
		version: 15,
		name:    "create_artifacts",
		// 产物：agent 在干活过程中产出的、不是代码也不是项目文档的东西 —— 一个 HTML
		// 页面、一份报告、一张图。
		//
		// It is a table of its own rather than more rows in media_assets because the
		// direction is reversed. A media asset is something a *person* uploaded, its
		// bytes live in the workspace, and the question asked of it is "show this to
		// the model". An artifact is something the *agent* produced, its bytes live in
		// the artifact store on the server, and the question asked of it is "give me
		// a URL". Folding them together would mean every media query carrying a
		// filter for which kind of thing it wanted.
		//
		// path is the artifact's location under the store's root and is also its
		// identity: the serving route resolves exactly this string, so a corrupted
		// row cannot become a read outside the root (see internal/artifact).
		up: `CREATE TABLE IF NOT EXISTS artifacts (
			id         TEXT PRIMARY KEY,
			session_id TEXT NOT NULL DEFAULT '',
			title      TEXT NOT NULL DEFAULT '',
			kind       TEXT NOT NULL DEFAULT '',
			path       TEXT NOT NULL,
			mime       TEXT NOT NULL DEFAULT '',
			bytes      INTEGER NOT NULL DEFAULT 0,
			source     TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_artifacts_session ON artifacts(session_id);
		CREATE INDEX IF NOT EXISTS idx_artifacts_created ON artifacts(created_at);`,
	},

	{
		version: 16,
		name:    "model_context_window",
		// 模型的上下文窗口，记在目录里而不是每次现算。
		//
		// Two sources fill it, in this order of authority: the provider's own
		// /models response when it carries one (context_length and friends — an
		// exact number, free), and a direct question to the model when it does
		// not ("你的上下文窗口是多少 token"). A provider that reports nothing is
		// the norm outside the big gateways: deepseek, for one, answers /models
		// with ids only.
		//
		// It lives here rather than being recomputed per turn because the
		// question is expensive and the answer does not change: an agent turn
		// needs the number in microseconds, and asking a model what it is costs
		// a round trip.
		//
		// context_window_source records which of them answered, so the console
		// can say where a number came from — a value the model guessed and one
		// its provider published deserve different amounts of trust.
		up: `ALTER TABLE llm_models ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE llm_models ADD COLUMN context_window_source TEXT NOT NULL DEFAULT '';
		ALTER TABLE llm_models ADD COLUMN context_window_checked_at TIMESTAMP;`,
	},
}

// Migrate applies any pending migrations idempotently.
func (s *sqliteStore) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`); err != nil {
		return fmtErr("create migrations table", err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmtErr("read migrations", err)
	}
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return fmtErr("scan migration", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmtErr("iterate migrations", err)
	}
	_ = rows.Close()

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmtErr("begin tx", err)
		}
		if _, err := tx.ExecContext(ctx, m.up); err != nil {
			_ = tx.Rollback()
			return fmtErr("apply migration %d (%s)", err, m.version, m.name)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.version, m.name); err != nil {
			_ = tx.Rollback()
			return fmtErr("record migration %d", err, m.version)
		}
		if err := tx.Commit(); err != nil {
			return fmtErr("commit migration %d", err, m.version)
		}
	}
	return nil
}

func fmtErr(msg string, err error, args ...any) error {
	format := msg
	if len(args) > 0 {
		format = fmt.Sprintf(msg, args...)
	}
	return fmt.Errorf("%s: %w", format, err)
}
