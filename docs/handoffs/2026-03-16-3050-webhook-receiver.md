## Handoff: Flight Leg 10 — Webhook Receiver (VCS-agnostic)

**Date:** 2026-03-16
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md
**bd Issues Completed:** bossanova-783, bossanova-n83, bossanova-fcd, bossanova-7vn, bossanova-tkc, bossanova-btj, bossanova-5n2

### Tasks Completed

- bossanova-783: Add webhook proto definitions (DeliverVCSEvent RPC on daemon, webhook config CRUD RPCs on orchestrator)
- bossanova-n83: webhook_configs migration + WebhookConfigStore (SQLite-backed CRUD with GetByRepo lookup)
- bossanova-fcd: Webhook parser interface + GitHub parser (HMAC-SHA256, check_suite/check_run/pull_request/pull_request_review → VCS events)
- bossanova-7vn: Webhook HTTP handler with event routing to daemons via relay pool + ListByRepoID on DaemonStore
- bossanova-tkc: Wire webhook handler into orchestrator main + mux, webhook config server RPCs
- bossanova-btj: 17 tests (HMAC verification, event parsing, handler HTTP behavior)
- bossanova-5n2: [HANDOFF]

### Files Changed

- `proto/bossanova/v1/daemon.proto` — Added DeliverVCSEvent RPC + request/response messages
- `proto/bossanova/v1/orchestrator.proto` — Added CreateWebhookConfig/ListWebhookConfigs/DeleteWebhookConfig RPCs + WebhookConfig message
- `services/bosso/migrations/20260316300000_add_webhook_configs.sql` — NEW: webhook_configs table with unique (repo_origin_url, provider) constraint
- `services/bosso/internal/db/store.go` — Added WebhookConfig type, CreateWebhookConfigParams, WebhookConfigStore interface, ListByRepoID to DaemonStore
- `services/bosso/internal/db/webhook_config_store.go` — NEW: SQLite-backed WebhookConfigStore implementation
- `services/bosso/internal/db/daemon_store.go` — Added ListByRepoID with JOIN on daemon_repos
- `services/bosso/internal/webhook/parser.go` — NEW: Parser interface, ParsedEvent type, Registry for multi-provider support
- `services/bosso/internal/webhook/github.go` — NEW: GitHubParser with HMAC-SHA256 verification and event mapping
- `services/bosso/internal/webhook/handler.go` — NEW: HTTP handler (POST /webhooks/{provider}) with signature verification and daemon routing
- `services/bosso/internal/webhook/webhook_test.go` — NEW: 17 tests
- `services/bosso/internal/server/webhook.go` — NEW: CreateWebhookConfig/ListWebhookConfigs/DeleteWebhookConfig server handlers
- `services/bosso/internal/server/server.go` — Added webhooks field to Server, updated constructor
- `services/bosso/internal/server/server_test.go` — Updated testEnv with webhook store
- `services/bosso/cmd/main.go` — Wired webhook config store, parser registry, HTTP handler

### Learnings & Notes

- GitHub webhooks are raw HTTP POSTs, not ConnectRPC — registered as `POST /webhooks/{provider}` on the mux alongside ConnectRPC routes
- Parse first, verify signature second — need the repo URL from the payload to look up the HMAC secret
- `r.PathValue("provider")` works in Go 1.22+ with `http.ServeMux` pattern matching
- check_suite → aggregate pass/fail signal; check_run → individual check detail for failures
- pull_request "synchronize" action can carry mergeable=false for conflict detection
- DaemonPool interface in webhook package avoids circular dependency with relay package
- gofmt alignment in struct fields with mixed tag lengths needs to be consistent

### Issues Encountered

- gofmt formatting issue with struct field alignment in GitHub payload types — fixed with `gofmt -w`
- Pre-existing lint issues in boss and bossd modules (errcheck, staticcheck) are unrelated to this leg

### Next Steps (Flight Leg 11)

Per the plan, Leg 11 covers Web SPA (CF Pages):

- React SPA: Vite + React + @connectrpc/connect-web + @auth0/auth0-react
- Auth0 PKCE flow in browser → JWT → ConnectRPC interceptor
- Session list page (polling), session detail (server-streaming Claude output)
- Daemon list page, session actions (stop, pause, resume, transfer)
- CORS middleware on orchestrator (`rs/cors`)
- CF Pages deployment via wrangler or git integration

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review files: `services/bosso/internal/webhook/handler.go`, `services/bosso/internal/webhook/github.go`, `services/bosso/internal/server/webhook.go`
