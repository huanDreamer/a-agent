# Capability: artifacts

## Purpose

Give the resources the agent produces — a generated page, a report, a chart, an
image — a place to live on the server, a URL to open them, and a way to see what
each conversation produced.

## Scope

- `internal/artifact` — file layout, containment, types, atomic bounded write,
  the shared `Saver`, `URL()`.
- `internal/store/artifact.go` + migration v15 — the index.
- `internal/server/artifacts.go` — the routes, the headers, the per-turn publish.
- `internal/tool/artifact.go` + `internal/tool/builtin/artifact.go` — the
  `save_artifact` tool and the vocabulary it shares with its host.
- `cmd/huan-agent/artifacts.go` — the command surfaces.
- `web/` — the 产物 button and drawer, 产物中心.

## Requirements (MUST)

### Storage

1. **Separate root** — artifact bytes MUST live under a directory of the
   server's own (`tools.artifacts.root`, defaulting to `artifacts/` beside the
   database file) and MUST NOT be written into any workspace.
2. **Containment** — every read and every write MUST go through one path check
   that rejects NUL bytes, cleans the path first, refuses absolute paths and
   traversals (`..`), and re-checks after resolving symlinks on the longest
   existing prefix. A path that resolves outside the root MUST be refused, not
   clamped.
3. **Extension whitelist** — only `html htm md txt csv json xml png jpg jpeg gif
   webp pdf` MUST be accepted. An unknown extension MUST be refused at write
   time rather than stored.
4. **Type from the extension** — the served content type MUST be derived from
   the stored extension and MUST NOT come from anything a client sent. The write
   path and the serving route MUST answer from the same table.
5. **Bounded, atomic write** — content MUST be written through a temp file in the
   destination directory and renamed into place, and MUST be refused (not
   truncated) when it exceeds `tools.artifacts.max_bytes` (default 32 MiB).
6. **Path as name** — a stored file MUST be named
   `<session>/<YYYY-MM-DD>/<stem>.<ext>`, where the stem comes from the title,
   then from the suggested file name, and finally from a fixed generic word. The
   stem MUST keep non-ASCII letters, so a Chinese title produces a Chinese file
   name rather than an opaque token; it MUST NOT be transliterated into pinyin. A
   session id used as a path component MUST be validated as a single component
   (letters, digits, dash, underscore only).
7. **Date directory is the local day** — the `YYYY-MM-DD` component MUST be the
   day of the saving process's local clock, not UTC, so that the directory and the
   console's day grouping name the same day for the same file. An artifact saved
   before the local/UTC offset is reached MUST NOT be filed under tomorrow.
8. **Names do not overwrite** — two artifacts wanting the same name on one day
   MUST both be stored: the second MUST be suffixed `-2`, the third `-3`, and so
   on, with the suffix placed before the extension. A file already on disk MUST
   count as taken even when no row points at it.

### Index

9. **Own table** — artifacts MUST be recorded in a table of their own, not as
   more rows in `media_assets`.
10. **File is the truth, row is the index** — the database MUST record the
   relative `path` the serving route resolves; the store's contents MUST NOT be
   derived from the rows.
11. **Bytes first, row second** — a failed insert MUST remove the file it just
   wrote, so no listing entry ever points at a missing file.
12. **Row first on delete** — deletion MUST remove the row and then the file. A
    file that cannot be removed MUST leave the row deleted and MUST be reported
    in the log and in the response, not turned into a failed request.
13. **Newest first** — a listing MUST be ordered by `created_at` descending.

### Serving

14. **Login by default** — artifact files MUST require a valid console session
    unless the deployment explicitly opted into public URLs
    (`tools.artifacts.public_urls`), which MUST default to off and MUST log a
    warning at startup when combined with a non-loopback bind.
15. **Sandboxed documents** — a response typed `text/html`, `application/xhtml+xml`
    or `image/svg+xml` MUST carry `Content-Security-Policy: sandbox` **without**
    `allow-same-origin`, so the document runs in an opaque origin and cannot read
    the console's cookies, `localStorage` or API.
16. **No sniffing** — every artifact file response MUST carry
    `X-Content-Type-Options: nosniff`.
17. **Streamed** — a file MUST be served as a stream, not read into memory.
18. **Disabled means explained** — with the feature off, the artifact **JSON**
    endpoints (list, get, delete) MUST answer with a JSON body saying
    `enabled: false` and why, MUST NOT 404, and MUST NOT vary their wording
    between endpoints. The file route is the exception: a request for a file
    that cannot exist is a 404, and the console reads `enabled` from the listing
    rather than from a file it has not asked for.
19. **URL built in one place** — the URL a model is handed and the links the
    console renders MUST be built by the same function from the same prefix.

### Tool

20. **Nothing to configure at call time** — `save_artifact` MUST take no host
    argument; which session owns an artifact and where the console serves it
    MUST be published on the turn's context.
21. **Refusal in words** — a surface with no artifact store MUST answer the model
    with a message it can act on, rather than failing obscurely.
22. **Honest about no URL** — a surface with no HTTP server MUST report the path
    it wrote and say no URL is available, rather than quoting a path as a link.
23. **Kind validated, not stored blindly** — a kind outside
    `html / document / image / other` MUST be refused with the real options, and
    a recognised spelling (`doc`, `page`, `img`, `chart`, …) MUST be normalised.
24. **Registered serial** — the tool MUST NOT run concurrently with itself, so
    two saves in the same second with the same title cannot collide into one
    file with two rows.
25. **Available in a read-only workspace** — saving an artifact MUST NOT be
    withheld from a read-only workspace, because it writes to the deployment's
    own store and cannot choose a path.

### Console

26. **Per conversation** — the chat header MUST offer a 产物 control that shows
    the count for the current conversation and opens its artifacts, newest first.
27. **Across conversations** — 统计监控 MUST offer 产物中心 listing every
    session's artifacts, with the owning session on each row (falling back to the
    id when the title is unknown, never to a blank) and kind/session filters.
28. **Grouped by day, named by file** — 产物中心 MUST group its rows by the
    local calendar day of `created_at`, newest day first and the server's order
    kept within a day, and MUST show each artifact's stored file name. A row whose
    timestamp cannot be parsed MUST be shown in a trailing bucket rather than
    dropped. The day grouping MUST agree with the date in the artifact's own path.
29. **Rows act** — each row MUST offer 打开 (new tab), 下载 and 删除, and 删除
    MUST confirm first.
30. **Fresh after a save** — a `save_artifact` tool result in the stream MUST
    make the list reload, so an artifact saved mid-turn appears without a manual
    refresh.
31. **Disabled means hidden** — with the feature off the 产物 control MUST be
    hidden rather than showing an error about something the operator never turned
    on.
32. **Every template identifier is bound** — a component template MUST NOT
    reference a name its `<script setup>` does not bind. Such a reference
    compiles to `_ctx.<name>`, passes the build and the type check, and throws
    only when that branch renders; a whole panel can therefore render as a count
    with no rows. The repository MUST enforce this before the build, by compiling
    each SFC with the build's own binding metadata and rejecting every non-`$`
    `_ctx.<name>`.
33. **The shipped bundle is built explicitly** — `internal/server/webui/dist`
    MUST be produced by the client build (`npm run build`), because that is the
    directory `//go:embed` serves and the render probes write elsewhere. A green
    probe run MUST NOT be read as evidence that the served bundle contains the
    change.

## Out of scope

- Rendering artifact contents inside the console (they open as files).
- Versioning or rewriting a stored artifact — a stored file is immutable.
- Pruning the store directory; deleting is per-artifact and manual.
- Any automatic upload to an external service.
