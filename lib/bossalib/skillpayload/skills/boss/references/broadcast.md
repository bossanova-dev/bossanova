<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Broadcasts

### `boss broadcast`

Send messages to the chats a selector resolves to

### `boss broadcast list [flags]`

Alias: `boss broadcast ls`

List recent broadcasts

**Flags:**

- `--chat` — Filter to broadcasts addressed to this target chat id
- `--json` — Emit a stable JSON schema instead of a table
- `--limit` — Cap the number of broadcasts returned (0 = unlimited) (default: 0)
- `--origin` — Filter by originating chat id
- `--state` — Filter by broadcast state

### `boss broadcast remove <broadcast-id>`

Alias: `boss broadcast rm`

Remove a broadcast and its deliveries by id

### `boss broadcast send [flags]`

Send a broadcast to the audience a selector resolves to

**Flags:**

- `--cross-daemon` — Ask bosso to route this broadcast to other daemons too, not just this daemon's own chats; bosso fans it out to the tenant's other live daemons and each re-resolves the selector against its own chats. Best-effort: a daemon offline at fan-out time never receives it, and past 32 other daemons the fan-out is refused rather than truncated. Pair it with a repo:/agent:/chat: selector — a daemon:<id> term matches no chats on any daemon, because chat rows carry an empty daemon id
- `--expires-in` — How long to keep retrying, as a duration (e.g. 30m, 24h, 7d); default 24h, max 30d
- `--from` — Originating chat id (default: $BOSS_AGENT_SESSION_ID; empty is allowed)
- `--include-origin` — Deliver to the origin chat too instead of excluding it from its own audience
- `--json` — Emit the sent broadcast as a stable JSON schema
- `--message` — Prompt delivered to each target chat; - reads it from stdin (required)
- `--to` — Selector resolving the audience, e.g. repo:<id>,agent:claude (required)

### `boss broadcast subscribe [flags]`

Register a standing rule that broadcasts when a session settles

**Flags:**

- `--expires-in` — How long the rule stands, as a duration (e.g. 30m, 24h, 7d); default 24h, max 30d
- `--from` — Registering chat id (default: $BOSS_AGENT_SESSION_ID; provenance only)
- `--json` — Emit the created subscription as a stable JSON schema
- `--message` — Prompt broadcast when it fires; - reads it from stdin (required)
- `--on` — Outcome to wait for: completed, errored, settled (required)
- `--session` — Session whose outcome fires it (default: $BOSS_SESSION_ID)
- `--to` — Selector resolving the audience at fire time (required)

### `boss broadcast subscriptions [flags]`

List standing broadcast subscriptions

**Flags:**

- `--json` — Emit a stable JSON schema instead of a table
- `--limit` — Cap the number of subscriptions returned (0 = unlimited) (default: 0)
- `--on` — Filter by trigger (completed, errored, settled)
- `--session` — Filter by owning session id
- `--state` — Filter by state (active, fired, canceled, expired)

### `boss broadcast unsubscribe <subscription-id>`

Retire a standing broadcast subscription by id
