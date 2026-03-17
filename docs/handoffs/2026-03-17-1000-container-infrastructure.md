## Handoff: Container Infrastructure (Flight Leg 1)

**Date:** 2026-03-17
**Branch:** main
**Flight ID:** fp-2026-03-17-production-readiness-gaps
**Planning Doc:** docs/plans/2026-03-17-production-readiness-gaps.md

### Tasks Completed This Flight Leg

- bossanova-ficc: Fix DefaultDBPath() to honor BOSSO_DB_PATH env var
- bossanova-u21y: Create Litestream config for R2 replication
- bossanova-73bx: Create multi-stage Dockerfile for bosso
- bossanova-1xxk: Create Fly.io config for bosso

### Files Changed

- `services/bosso/internal/db/db.go:15-25` — Added BOSSO_DB_PATH env var check before macOS fallback
- `services/bosso/litestream.yml` — Created; R2 replication config with env-var-templated credentials, 10s sync interval
- `services/bosso/Dockerfile` — Created; multi-stage build (golang:1.25-bookworm → litestream:0.3 → debian:bookworm-slim), CGO_ENABLED=0, stub go.mod for boss/bossd
- `services/bosso/fly.toml` — Created; Fly v2 format, port 8080, /data volume mount, auto_stop=false for single-instance SQLite

### Implementation Notes

- Dockerfile builds from repo root context (`docker build -f services/bosso/Dockerfile .`)
- Stub go.mod files created in builder stage for boss/bossd since go.work references all modules
- Litestream uses `force-path-style: true` for R2 compatibility
- fly.toml uses `min_machines_running = 1` and `auto_stop_machines = false` since SQLite requires single-instance

### Current Status

- Tests: pass (all bosso tests green)
- Lint: pass (go vet clean)
- Build: not tested (Docker build requires go 1.25 image)

### Next Flight Leg

- bossanova-el11: Create web SPA CI workflow (test-web.yml)
- bossanova-q1hm: Create tag-triggered deploy workflow (deploy.yml)
- bossanova-l12x: Delete release.yml (merged into deploy.yml)
- bossanova-99xi: [HANDOFF] Review Flight Leg 2
