<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## GitHub Callbacks

### `boss callback`

Manage GitHub PR callbacks (durable one-shot event notifications)

A GitHub callback is a durable, one-shot notification: it fires a prompt into a chat once a pull request reaches a chosen state, then retires. Use it to answer natural-language asks like "tell me when PR #123 is merged", "ping this chat when PR #123 goes green", "let me know if PR #123's checks fail", "notify me when PR #123 is closed", "tell me when PR #123 comes out of draft", or "ping me when PR #123 is green and ready to merge". Triggers map to those phrasings: `merged`, `checks_passed` (green), `checks_failed` (red), `closed`, `ready_for_review` (the draft→ready flip), and `checks_passed_ready` (green and not a draft — the merge-eligibility moment). Triggers are evaluated on PR state, not on transitions: a callback armed on a PR that ALREADY satisfies its trigger fires on the next evaluation rather than waiting for a fresh event unless `--on-transition` is set. Delivery only signals that the event fired — always verify the PR's actual state before acting on it. Callbacks expire after 24h by default and may not outlive 30 days.

### `boss callback add <pr> <trigger> [flags]`

Register a callback for a pull request event

Register a one-shot callback. `<pr>` is a bare PR number (resolved against the current repository) or a full `https://github.com/owner/repo/pull/N` URL. `<trigger>` is one of `merged`, `closed`, `checks_passed`, `checks_failed`, `ready_for_review` (the draft→ready flip), or `checks_passed_ready` (green and not a draft — merge-eligible). Triggers match on PR state, not on transitions, so arming one against a PR that already satisfies it fires on the next evaluation unless `--on-transition` is set. The `--message` prompt is delivered verbatim to the target chat when the callback fires and is treated as a secret — it is never echoed back on any surface. Expiry defaults to 24h and may not exceed 30 days.

**Flags:**

- `--chat` — Target agent-session (chat) id to notify (default: $BOSS_AGENT_SESSION_ID)
- `--expires-in` — Expiry as a duration (e.g. 30m, 24h, 7d, 2w); default 24h, max 30d
- `--group` — Optional group id; siblings in a group cancel each other on first fire
- `--json` — Emit the created callback as a stable JSON schema
- `--message` — Prompt delivered to the chat when the callback fires (required)
- `--on-transition` — Fire only after the trigger transitions from unsatisfied to satisfied
- `--repo` — Repository as owner/repo (default: the current repository's origin)

```bash
# "tell me when PR #123 is merged"
boss callback add 123 merged --message "PR #123 merged — pull main and redeploy"
# "ping this chat when PR #123 goes green"
boss callback add 123 checks_passed --message "PR #123 is green — start the release"
# "let me know if PR #123's checks fail"
boss callback add 123 checks_failed --message "PR #123 is red — investigate the failing checks"
# "notify me when PR #123 is closed" (full URL, longer expiry)
boss callback add https://github.com/acme/widget/pull/123 closed --message "PR #123 was closed" --expires-in 7d
# "tell me when PR #123 comes out of draft"
boss callback add 123 ready_for_review --message "PR #123 left draft — review it"
# "ping me when PR #123 is green and ready to merge"
boss callback add 123 checks_passed_ready --message "PR #123 is green and ready to merge"
# "tell me if this PR becomes red later, but do not fire for its current red state"
boss callback add 123 checks_failed --on-transition --message "PR #123 became red"
```

### `boss callback list [flags]`

Alias: `boss callback ls`

List registered GitHub callbacks

**Flags:**

- `--chat` — Filter by target agent-session (chat) id
- `--id` — Filter by callback id
- `--json` — Emit a stable JSON schema instead of a table
- `--repo` — Filter by repository as owner/repo
- `--state` — Filter by state (active, leased, triggered, delivered, canceled, expired)
- `--trigger` — Filter by trigger (merged, closed, checks_passed, checks_failed, ready_for_review, checks_passed_ready)

```bash
boss callback list
boss callback list --id cb_abc123
boss callback list --repo acme/widget --trigger merged
boss callback list --json
```

### `boss callback remove <callback-id> [flags]`

Alias: `boss callback rm`

Remove a GitHub callback by id

**Flags:**

- `--chat` — Owning chat id for remote routing (default: $BOSS_AGENT_SESSION_ID; ignored locally)

```bash
boss callback remove cb_abc123
```
