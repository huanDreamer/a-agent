# Phase 25 — Artifacts (产物)

## Summary

Give the agent's *output* a place to live. A task that builds a dashboard, writes
a report or renders a chart ends with something that is neither code nor project
documentation, and until now it had nowhere to go: `write_file` could put it in
the workspace, but the workspace is the user's project — a generated page sitting
among its sources is clutter, it pollutes the user's `git status`, and it cannot
be opened in a browser.

This change stores those resources in a directory of the server's own, indexes
them, serves them over HTTP with a URL, exposes them per conversation through a
产物 button in the chat header, and lists every session's through 产物中心 in
统计监控.

## Why

The gap is visible in three ways:

- **A deliverable that cannot be delivered.** The model can already generate an
  HTML page. Without this, the only ways to hand it over are pasting its source
  into the reply or leaving it in the user's repo. Neither is "here is the page".
- **Attachments are the wrong tool.** `media_assets` holds what a *person*
  uploaded, its bytes live in the workspace, and the question asked of it is
  "show this to the model". An artifact is what the *agent* produced, it has to
  outlive the workspace it was made in, and the question asked of it is "give me
  a URL".
- **Nothing to look back at.** Even after workspaces are gone, an operator wants
  to answer "what did this thing actually produce, and when".

## What Changes

### ADDED

- `internal/artifact/` — the artifact store: the file layout
  (`<root>/<session>/<timestamp>-<slug>.<ext>`), the path containment rules
  (shared with the workspace sandbox's reasoning), the extension whitelist, the
  atomic bounded write, and `URL()` which builds every link.
- `internal/artifact/saver.go` — `Saver`, the one implementation of
  `tool.ArtifactSaver`: bytes through the store, row through the database, URL
  handed back.
- Migration v15 — the `artifacts` table (own table, not more `media_assets`
  rows) with indexes on `session_id` and `created_at`.
- `internal/store/artifact.go` — `Artifact`, the four kinds, `CreateArtifact` /
  `GetArtifact` / `ListArtifacts` / `DeleteArtifact`.
- `internal/server/artifacts.go` — the four routes, the sandbox CSP, the
  optional public-URL mode, and `withTurnArtifacts`, which publishes the saver
  for one turn.
- `internal/tool/artifact.go` — the `ArtifactSaver` interface, `ArtifactInput` /
  `ArtifactResult`, and the context key. The vocabulary lives next to the Tool
  interface because two packages must agree on it and neither owns the other.
- `internal/tool/builtin/artifact.go` — the `save_artifact` tool.
- `cmd/huan-agent/artifacts.go` — the command surfaces (REPL, one-shot run,
  Feishu) build one store for the process and publish a saver per turn.
- Console: a 产物 button in the conversation header with a count and a drawer;
  产物中心 under 统计监控, listing every session's with kind and session filters.
- Config: `tools.artifacts.{enable,root,public_urls,max_bytes}`.

### CHANGED

- `internal/server/turns.go` publishes the saver on the turn context.
- `cmd/huan-agent/{chat,run,admin}.go` wire the store and register the tool.

## Design decisions

- **A separate table, not more rows in `media_assets`.** The direction of the
  relationship is reversed (person→model vs agent→URL) and the bytes live in
  different places. Folding them together would mean every media query carrying
  a filter for which kind of thing it wanted.
- **The files on disk are the truth; the row is an index.** `Path` is both the
  address and the identity — the serving route resolves exactly that string
  under the configured root and nowhere else, so a corrupted row cannot become a
  read outside the root. `ListArtifacts` answers from the table, and a file whose
  row is gone is merely not listed.
- **Bytes first, row second, and the failure handling follows from the order.**
  A file with no row is bytes nothing lists (harmless, visible to an operator
  with `ls`); a row with no file is a listing entry that 404s. So a failed insert
  removes the file it just wrote. Deletion runs the other way for the same
  reason: row first, because a row is not reversible and a file that refuses to
  go is reported rather than turning into a failed request the client would retry
  against a row that is already gone.
- **The model chooses a name; it never chooses a path.** `Resolve` takes a
  relative path, rejects NUL, cleans first, refuses absolute paths and
  traversals, then re-checks after resolving symlinks on the longest existing
  prefix (so a path whose final component does not exist yet can still be
  validated). The store is on the server's own disk, so a path that escapes it
  is a read of the server's filesystem, not of a product.
- **An extension whitelist, refused at write time.** `html htm md txt csv json xml
  png jpg jpeg gif webp pdf`. Anything else would be served as an opaque
  download, which is not what "save this so it can be opened" means.
- **A content type is derived from the extension and never from what a client
  said.** `MIMEByExt` is exported so the write path and the serving route answer
  the question the same way — what a stored file *is* is decided once.
- **HTML/SVG is served in an opaque origin.** `Content-Security-Policy: sandbox`
  without `allow-same-origin`, plus `nosniff`. Without it a page the model wrote
  would run as the logged-in console: read its cookie, its `localStorage`, call
  its API as the admin who opened it. The difference between "the agent can show
  me a page" and "the agent can hand itself my session". Scripts, forms and
  downloads stay allowed, which is what keeps a generated dashboard useful.
- **File URLs require a session by default** (`public_urls: false`). The bytes
  are authored by the model and steered by whatever text reached it — including
  text it read out of the workspace — so an unauthenticated artifact URL is
  stored XSS against whoever opens it. Turning it on is a deliberate operator
  choice, and a non-loopback bind logs a warning at startup.
- **Zhu (session) is part of the path prefix.** `<session>/<file>` makes "this
  conversation's artifacts" a directory operation rather than a query, and keeps
  two sessions from colliding on a name. A session here is an opaque owner token
  — a chat session id, or a one-shot run's — not a foreign key.
- **A title with no ASCII is named by a random token, not pinyin.** `slugify`
  returns `""` for a Chinese title on purpose; transliterating would be a
  dependency and a guess, and a short opaque name is honest about being one. The
  artifact is still addressable and still carries its title for display.
- **Atomic, bounded writes.** Temp file in the destination directory (so the
  rename stays on one filesystem) + `rename`; one byte past the cap is read so
  "exactly at the limit" and "over it" are distinguishable and the message can
  say which.
- **`save_artifact` is registered `Serial`.** Two artifacts saved in the same
  second with the same title produce the same file name; the second would
  overwrite the first and leave two rows pointing at one file.
- **The tool is `CapRead`, so it works in a read-only workspace.** Saving an
  artifact writes to the deployment's own store, cannot even choose a path, and
  cannot touch a file the user cares about — which is exactly the situation a
  long analysis produces a report in.
- **No saver configured means a message, not a crash.** The tool is built once at
  startup with no arguments (where an artifact goes is a property of the *turn*),
  and answers in words the model can act on when no store is published.
- **No URL on a surface that serves nothing.** The CLI and one-shot run store
  bytes and report the path; quoting a path as if it were a link would be a lie.
  The console still lists those artifacts, because the store is shared.
- **Newest first, deliberately unlike `ListMediaAssets`.** An attachment list is
  the files a message refers to and reads in the order sent; this is a gallery,
  and the thing just produced is what the reader came for.
- **One endpoint for both list views.** `?session=` narrows it, omitting it asks
  for everything: they are one question — "which artifacts" — asked with a
  narrower answer in one case.

## Impact

- **Schema**: migration v15 is additive (`CREATE TABLE IF NOT EXISTS` + two
  indexes). Existing rows and deployments are untouched.
- **Defaults**: the feature is **on** by default, because the store is one
  directory beside the database and a deployment that cannot store an artifact
  cannot show the user the page it just wrote. `public_urls` is off.
- **Security**: file URLs require a login unless opted out; served HTML/SVG is
  CSP-sandboxed into an opaque origin and `nosniff`ed; every read and write goes
  through one containment check.
- **Disk**: a new `artifacts/` directory beside the database file (or
  `tools.artifacts.root`), capped per file at 32 MiB. Deleting an artifact
  removes row and file; nothing prunes the directory on its own.
- **Compatibility**: no existing command, config key or route changes meaning.
