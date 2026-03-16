## Handoff: Flight Leg 8c — CLI Auth + Terraform

**Date:** 2026-03-16 28:00
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-823: Implement boss login/logout with Auth0 PKCE flow and OS keychain storage
- bossanova-w7d: Create Terraform modules (fly, auth0, cloudflare, github) with staging/prod envs
- bossanova-x66: Add OIDC client tests (token refresh, keychain mock, login flow)

### Files Changed

- `services/boss/internal/auth/oidc.go:1-279` — Full PKCE authorization code flow: local callback server on random port, browser launch, code exchange, token refresh. Uses S256 code challenge, CSRF state parameter.
- `services/boss/internal/auth/tokenstore.go:1-89` — OS keychain persistence via 99designs/keyring (macOS Keychain, Linux secret-service, Windows credential manager). TokenStore interface for mock injection.
- `services/boss/internal/auth/manager.go:1-80` — Token lifecycle: load from keychain, auto-refresh expired tokens, login/logout, status reporting with email from ID token.
- `services/boss/internal/auth/claims.go:1-37` — ID token JWT payload extraction (sub, email, name) without validation (already validated by issuer during exchange).
- `services/boss/internal/auth/auth_test.go:1-340` — 15 tests: mock keychain store, token validity, manager lifecycle, PKCE helpers, refresh flow with mock server, end-to-end login with mock Auth0 + RSA JWT signing.
- `services/boss/cmd/auth.go:1-117` — Cobra commands: `boss login` (PKCE flow + keychain save), `boss logout` (keychain delete), `boss auth-status` (email, expiry, remaining). Config via BOSS_OIDC_ISSUER, BOSS_OIDC_CLIENT_ID, BOSS_OIDC_AUDIENCE env vars.
- `services/boss/cmd/main.go:31-41` — Added login, logout, auth-status to root command.
- `services/boss/go.mod` — Added 99designs/keyring, golang-jwt/jwt/v5.
- `infra/modules/fly/main.tf:1-59` — Fly.io app, machine, persistent volume, TLS/HTTP service.
- `infra/modules/fly/variables.tf:1-48` — App name, org, region, image, cpus, memory, volume size, OIDC config.
- `infra/modules/auth0/main.tf:1-103` — API resource server (RS256, 24h tokens), native app (PKCE, rotating refresh tokens), database connection, optional Google/GitHub social logins.
- `infra/modules/auth0/variables.tf:1-71` — API audience, client name, callback URLs, social login toggles and credentials.
- `infra/modules/cloudflare/main.tf:1-26` — R2 bucket for Litestream SQLite backups, DNS CNAME record to Fly.io.
- `infra/modules/github/main.tf:1-21` — Webhook secret generation (random 32-byte), GitHub App installation management.
- `infra/environments/staging/main.tf:1-48` — Staging: lower resources (1 CPU, 256MB, 1GB), api.staging subdomain.
- `infra/environments/production/main.tf:1-50` — Production: higher resources (2 CPU, 512MB, 2GB), api subdomain.

### Learnings & Notes

- **99designs/keyring requires CGO on macOS**: The go-keychain binding for macOS Security framework needs CGO. This is fine for the CLI binary (built on target platforms) but differs from the "pure Go, no CGO" goal of other modules. Only affects boss CLI, not bossd/bosso.
- **PKCE flow pattern**: Local callback server on `127.0.0.1:0` (random port) avoids port conflicts. The redirect_uri with the actual port is passed to the authorize request. Auth0's "native" app type supports `http://127.0.0.1/callback` as an allowed callback wildcard.
- **openBrowserFn variable pattern**: The `openBrowser` function is a package-level `var` to allow test injection. Tests replace it with an HTTP client that follows the authorize→callback redirect, simulating the browser flow entirely in-process.
- **Token refresh keeps old refresh token**: If Auth0 doesn't issue a new refresh token in the response, we preserve the existing one. Important for rotating refresh token setups where the server may not always reissue.
- **Terraform module isolation**: Each module declares its own required_providers block. Environments compose modules and wire outputs→inputs (e.g., auth0.issuer → fly.oidc_issuer).

### Issues Encountered

- **Stray `cmd` binary at project root**: Still present from earlier sessions. Not related to this leg.
- **clang deployment version warnings**: macOS SDK version mismatch warnings during CGO compilation. Cosmetic only, builds succeed.

### Current Status

- Build: PASSED — all 4 modules
- Vet: PASSED
- Format: PASSED
- Tests: PASSED — boss: 15 tests, bossd: 66 tests, bosso: 20 tests, bossalib: machine tests (119 total)

### Next Steps (Flight Leg 9: Multi-Daemon + Remote CLI + Streaming)

Per the plan (Leg 9):

- Daemon → orchestrator upstream connection (reconnect with backoff, heartbeat 30s)
- Remote CLI: `boss --remote` or auto-detect, same BossClient interface
- Stream relay: orchestrator proxies AttachSession stream (daemon → CLI)
- Session transfer: commit+push on A, registry update, fetch+worktree on B

Note: No bd tasks exist yet for Leg 9 of the Go rewrite. The next session should run `/pre-flight-checks` or `/file-a-flight-plan` to create them.

### Resume Command

To continue this work:

1. Review planning doc: `docs/plans/2026-03-16-1700-bossanova-go-rewrite.md` (Leg 9 section)
2. Create tasks for Leg 9 via `/pre-flight-checks`
3. Key files for context: `services/boss/internal/auth/oidc.go`, `services/boss/internal/auth/manager.go`, `services/boss/cmd/auth.go`, `services/bosso/internal/auth/middleware.go`
