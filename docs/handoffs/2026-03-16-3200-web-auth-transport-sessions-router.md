## Handoff: Flight Leg 11b — Auth0 + ConnectRPC Transport + Session List + Router

**Date:** 2026-03-16
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md
**bd Issues Completed:** bossanova-1qd, bossanova-750, bossanova-jbl, bossanova-csw

### Tasks Completed

- bossanova-1qd: Set up Auth0 provider with PKCE flow in App.tsx
- bossanova-750: Create ConnectRPC transport with Auth0 JWT interceptor
- bossanova-jbl: Build session list page with polling (ProxyListSessions)
- bossanova-csw: Add React Router layout with nav (sessions, daemons)

### Files Changed

- `services/web/src/App.tsx` — Wrapped in Auth0Provider + ApiProvider + BrowserRouter, routes for Sessions (index) and Daemons
- `services/web/src/api.ts` — NEW: createApi() factory with createConnectTransport + auth interceptor attaching Bearer JWT
- `services/web/src/ApiContext.tsx` — NEW: React context providing authenticated ConnectRPC client via useApi() hook
- `services/web/src/Layout.tsx` — NEW: Layout with NavLink nav (Sessions, Daemons), Auth0 login/logout, Outlet
- `services/web/src/pages/Sessions.tsx` — NEW: Session list table calling ProxyListSessions with 5s polling, shows title/branch/state/PR
- `services/web/src/pages/Daemons.tsx` — NEW: Placeholder page (implemented in leg 11c)
- `services/web/.env.example` — NEW: Documents VITE_AUTH0_DOMAIN, VITE_AUTH0_CLIENT_ID, VITE_AUTH0_AUDIENCE, VITE_API_BASE_URL

### Learnings & Notes

- Connect-ES v2 uses `createClient(ServiceDesc, transport)` — no separate `createPromiseClient` needed
- Auth0 `getAccessTokenSilently()` is stable across renders (from useAuth0), safe to pass as closure to createApi
- React Router v7 uses `react-router` package (not `react-router-dom`), `<Route element={<Layout />}>` with `<Outlet />` for nested layouts
- `ApiProvider` wraps inside `Auth0Provider` but outside `BrowserRouter` — auth is available before routing
- Vite env vars must be prefixed with `VITE_` to be exposed to client code
- tsconfig.app.json already has `"types": ["vite/client"]` which provides `import.meta.env` types — no extra .d.ts needed
- `verbatimModuleSyntax: true` doesn't conflict with Auth0/ConnectRPC imports since they're value imports

### Issues Encountered

- None — implementation was straightforward following established patterns

### Next Steps (Flight Leg 11c)

- bossanova-fms: Build session detail page with server-streaming (ProxyAttachSession)
- bossanova-tum: Build daemon list page (ListDaemons)
- bossanova-4b99: Add session action buttons (stop, pause, resume, transfer)
- bossanova-4fw5: Add CF Pages deployment config (wrangler.toml + build settings)
- bossanova-378d: [HANDOFF]

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review files: `services/web/src/App.tsx`, `services/web/src/api.ts`, `services/web/src/ApiContext.tsx`, `services/web/src/pages/Sessions.tsx`
