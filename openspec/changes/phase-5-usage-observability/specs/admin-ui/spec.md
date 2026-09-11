# Capability: admin-ui

## Purpose

Give an operator a single authenticated surface to inspect usage, the audit log
and skill state, and to run the service's admin API, without a separate frontend
deployment.

## Scope

- `internal/server` — the HTTP service, authentication, and the embedded UI.
- `web/` — the Vue 3 single-page application.
- `huan-agent admin serve` / `admin set-password`.

## Requirements (MUST)

1. **Opt-in** — the admin server MUST be off unless explicitly enabled
   (`server.enable`), because it exposes usage data.
2. **Authentication** — every data endpoint MUST require a valid session;
   unauthenticated requests MUST receive HTTP 401 with a JSON error body.
   Liveness/health MAY be unauthenticated.
3. **Single-user credentials** — the admin account MUST be a single username with
   a bcrypt password hash from config. A missing or malformed hash MUST fail at
   startup, not at first login.
4. **Session cookie** — the session cookie MUST be HttpOnly and `SameSite=Lax`,
   MUST be `Secure` only when the request arrived over TLS (so localhost over
   plain HTTP still works), and MUST be revoked on logout.
5. **Login throttling** — repeated failed logins MUST be throttled, and the
   throttle MUST also refuse the correct password while it is active.
6. **Password hashing** — a CLI MUST exist to produce the bcrypt hash, and it
   MUST prefer a no-echo terminal prompt over a command-line flag (a flag value
   is visible in the process list).
7. **Embedded UI** — the built UI MUST be embedded in the binary; the server MUST
   serve the shell, hashed assets with a long-lived immutable cache, and MUST
   fall back to the shell for unknown non-API GET paths so client-side routes
   survive a reload.
8. **API 404s stay JSON** — an unknown `/api/*` path MUST return a JSON 404 and
   MUST NOT be answered with the HTML shell.
9. **Screens** — the UI MUST provide a login view and, for an authenticated
   operator, usage summary and trend, by-model and by-user breakdowns, recent
   calls, the audit log with a filter, and skill enable/disable.
10. **Honest rendering** — the UI MUST distinguish "unknown cost" (no price
    entry matched) from a real zero, MUST show an explicit empty state rather
    than a blank panel, and MUST surface request failures with a retry action.
11. **Charts without dependencies** — charts MUST be rendered without pulling in
    a charting library, keeping the dependency surface minimal for a
    security-sensitive admin tool.
12. **Build/test** — `go build ./...`, `go vet ./...` and `go test ./...` MUST
    pass without a browser or network access.

## Non-goals

- Multi-user accounts, roles or OAuth (MVP is a single admin password).
- Server-side rendered pages.
- Editing configuration or LLM credentials through the UI.

## Key interfaces

- `POST /api/login`, `POST /api/logout`, `GET /api/me`, `GET /api/health`,
  `GET /api/meta`
- `GET /api/audit`, `GET /api/skills`, `POST /api/skills/{name}`
- `server.New`, `server.Config`, `server.HashPassword`, `SessionCookieName`
- `huan-agent admin serve`, `huan-agent admin set-password`
