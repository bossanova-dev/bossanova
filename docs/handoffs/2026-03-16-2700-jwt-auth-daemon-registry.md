## Handoff: Flight Leg 8b — JWT Auth + Daemon Registry

**Date:** 2026-03-16 27:00
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-nlz: Implement JWT validation middleware for ConnectRPC (connectrpc/authn-go)
- bossanova-e4t: Implement daemon registry (RegisterDaemon, Heartbeat, ListDaemons RPCs)
- bossanova-gi3: Implement orchestrator entry point (ConnectRPC HTTPS server, DB, migrations, graceful shutdown)
- bossanova-vfr: Add auth + registry tests (JWT validation, daemon heartbeat timeout, registration)

### Files Changed

- `services/bosso/internal/auth/auth.go:1-51` — Info type (UserID/DaemonID), InfoFromContext via authn.GetInfo, IsUser/IsDaemon helpers.
- `services/bosso/internal/auth/jwt.go:1-165` — OIDC JWT validator with JWKS caching (1hr TTL), RSA key parsing, issuer/audience/expiry validation. Uses golang-jwt/jwt/v5.
- `services/bosso/internal/auth/middleware.go:1-65` — connectrpc.com/authn middleware: tries OIDC JWT first (looks up user by sub), falls back to session token (looks up daemon by GetByToken). Returns \*Info via authn mechanism.
- `services/bosso/internal/server/server.go:1-164` — OrchestratorServiceHandler implementing RegisterDaemon (user auth, generates 32-byte hex token, stores daemon, audit log), Heartbeat (daemon auth, daemon_id match check, updates heartbeat/sessions/online), ListDaemons (both auth types, lists by user_id). Proto conversion helper daemonToProto.
- `services/bosso/internal/server/server_test.go:1-339` — 10 integration tests with mock JWKS server + real RSA JWTs: RegisterDaemon (success, no auth, invalid token, audit log), Heartbeat (success, mismatched id), ListDaemons (user auth, daemon auth, no auth), expired JWT.
- `services/bosso/cmd/main.go:1-130` — Full orchestrator entry point: zerolog console writer, env config (BOSSO_ADDR, BOSSO_OIDC_ISSUER, BOSSO_OIDC_AUDIENCE), SQLite open + goose migrations, store init, JWT validator + authn middleware wrapping mux, ConnectRPC handler, graceful shutdown on SIGINT/SIGTERM.
- `services/bosso/go.mod:1-30` — Added connectrpc.com/authn, connectrpc.com/connect, golang-jwt/jwt/v5, zerolog, goose/v3.

### Learnings & Notes

- **connectrpc.com/authn middleware**: Works at the HTTP level (before Connect unmarshal), returns `any` which is stored via `authn.SetInfo`. Retrieved downstream via `authn.GetInfo(ctx)` and type-asserted to `*auth.Info`. Cleaner than custom context keys.
- **Dual auth scheme**: The middleware tries OIDC JWT validation first; if that fails (not a valid JWT), falls back to session token DB lookup. This means a single Bearer token header supports both user JWTs and daemon session tokens transparently.
- **Session token generation**: 32-byte crypto/rand hex (64 chars). Not a JWT — just an opaque token stored in the daemons table. Simpler than issuing a second JWT for daemon auth.
- **Heartbeat daemon_id check**: The middleware attaches the daemon's identity from the token lookup. The Heartbeat handler then verifies the request's daemon_id matches the authenticated daemon — prevents one daemon from heartbeating as another.
- **Test pattern**: Integration tests spin up a full httptest.Server with mock JWKS, real RSA key signing, authn middleware, and ConnectRPC handler. Each test gets a fresh in-memory SQLite DB. Very clean pattern for end-to-end verification.

### Issues Encountered

- **Stray `cmd` binary**: Still present at project root (noted in Leg 8a). Not related to this leg.
- **go.sum churn**: Each `go get` in the bosso module updates go.sum. Not an issue, just noise in diffs.

### Current Status

- Build: PASSED — all 4 modules
- Vet: PASSED
- Format: PASSED
- Tests: PASSED — bosso: 20 tests (10 DB + 10 server), bossd: 66 tests, bossalib: machine tests

### Next Steps (Flight Leg 8c: CLI Auth + Terraform)

- bossanova-823: Implement boss login/logout with Auth0 PKCE flow and OS keychain storage
- bossanova-w7d: Create Terraform modules (fly, auth0, cloudflare, github) with staging/prod envs
- bossanova-x66: Add OIDC client tests (token refresh, keychain mock, login flow)
- bossanova-896: [HANDOFF] Run /handoff-task skill and STOP

### Resume Command

To continue this work:

1. Review planning doc: `docs/plans/2026-03-16-1700-bossanova-go-rewrite.md` (Leg 8 section)
2. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` — should show bossanova-823
3. Key files for context: `services/bosso/internal/auth/middleware.go`, `services/bosso/internal/server/server.go`, `services/bosso/cmd/main.go`
