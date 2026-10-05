<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## GitHub Callbacks

### `boss callback`

Manage GitHub PR callbacks (durable one-shot event notifications)

A GitHub callback is a durable, one-shot notification: it fires a prompt into a chat once a pull request reaches a chosen state, then retires. Use it to answer natural-language asks like "tell me when PR #123 is merged", "ping this chat when PR #123 goes green", "let me know if PR #123's checks fail", "notify me when PR #123 is closed", "tell me when PR #123 comes out of draft", or "ping me when PR #123 is green and ready to merge". Triggers map to those phrasings: `merged`, `checks_passed` (green), `checks_failed` (red), `closed`, `ready_for_review` (the draft→ready flip), and `checks_passed_ready` (green and not a draft — the merge-eligibility moment). Triggers are evaluated on PR state, not on transitions: a callback armed on a PR that ALREADY satisfies its trigger fires on the next evaluation rather than waiting for a fresh event unless `--on-transition` is set. Delivery only signals that the event fired — always verify the PR's actual state before acting on it. Callbacks expire after 24h by default and may not outlive 30 days.

### `boss callback add <pr> <trigger> [flags]`

Register a callback for a pull request event

Register a one-shot callback. `<pr>` is a bare PR number (resolved against the current repository) or a full `https://github.com/owner/repo/pull/N` URL. `<trigger>` is one of `merged`, `closed`, `checks_passed`, `checks_failed`, `ready_for_review` (the draft→ready flip), or `checks_passed_ready` (green and not a draft — merge-eligible). Triggers match on PR state, not on transitions, so arming one against a PR that already satisfies it fires on the next evaluation unless `--on-transition` is set. The `--message` prompt is delivered verbatim to the target chat when the callback fires and is treated as a secret — it is never echoed back on any surface. Expiry defaults to 24h and may not exceed 30 days. Arming a trigger that cannot be satisfied at the same time as a callback already armed for the same chat and PR under a DIFFERENT `--group` prints a warning on stderr: sibling cancellation is group-scoped, so two groups of one never cancel each other and the losing leg stays armed until it expires. Put both triggers in one `--group` to fix that, or pass `--independent-watch` when the watch is genuinely meant to outlive its sibling. The create always succeeds either way. `--json` writes the documented schema to stdout only; advisories (that split-pair warning, the skill-refresh hold line, a stale-binary note) go to stderr, so parse stdout alone and never merge the streams with `2>&1`. The group key in that schema is `group_id`, not `group`.

**Flags:**

- `--chat` — Target agent-session (chat) id to notify (default: $BOSS_AGENT_SESSION_ID)
- `--expires-in` — Expiry as a duration (e.g. 24h, 7d, 2w); default 24h, max 30d. A watch must outlast the wait it backs
- `--group` — Optional group id; siblings in a group cancel each other on first fire
- `--independent-watch` — This watch is meant to outlive any sibling, so do not warn that a mutually exclusive callback is armed under another group. Records intent; it changes nothing about when the callback fires
- `--json` — Emit the created callback as a stable JSON schema on stdout only
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
# a pass/fail fork done right: ONE group, so whichever fires cancels the other
boss callback add 123 checks_passed --group pr123-settle --message "PR #123 is green" && boss callback add 123 checks_failed --group pr123-settle --message "PR #123 is red"
# a standing red alarm meant to outlive any sibling — no split-pair warning
boss callback add 123 checks_failed --independent-watch --message "PR #123 went red"
```

### `boss callback list [flags]`

Alias: `boss callback ls`

List registered GitHub callbacks

List registered callbacks, optionally filtered by chat, repository, trigger, state, id, or `--pr <n>` (matched on `pr_number`, so no `jq` filter is needed to scope a listing to one pull request). `--json` writes an array of the same schema as `boss callback add --json` to stdout only; advisories go to stderr, so never merge the streams with `2>&1` before parsing. The group key is `group_id`.

**Flags:**

- `--chat` — Filter by target agent-session (chat) id
- `--id` — Filter by callback id
- `--json` — Emit a stable JSON schema instead of a table, on stdout only
- `--pr` — Filter by pull request number (matched on pr_number) (default: 0)
- `--repo` — Filter by repository as owner/repo
- `--state` — Filter by state (active, leased, triggered, delivered, canceled, expired)
- `--trigger` — Filter by trigger (merged, closed, checks_passed, checks_failed, ready_for_review, checks_passed_ready)

```bash
boss callback list
boss callback list --id cb_abc123
boss callback list --repo acme/widget --trigger merged
boss callback list --chat "$BOSS_AGENT_SESSION_ID" --pr 123 --json
boss callback list --json
```

### `boss callback remove (<callback-id> | --all) [flags]`

Alias: `boss callback rm`

Remove a GitHub callback by id, or every active callback for the chat

Remove one callback by id, or pass `--all` to remove every live (`active`, `leased` or `triggered`) callback owned by the resolved chat (`--chat`, else `$BOSS_AGENT_SESSION_ID`), optionally narrowed by `--pr` and `--repo`. `--all` never touches another chat's callbacks. After removing, it re-lists the same scope and exits non-zero naming every id still active, so a cleanup that left a watch armed is a failure rather than silence. Success always prints the count, including `Removed 0 callback(s) for chat <id>; 0 active remain.` when nothing matched. A callback that fired or expired between the list and the delete counts as gone. With `--json` it writes `{"removed":[…],"remaining_active":[…],"chat":"…","pr_number":n|null}` to stdout only (a non-empty `remaining_active` comes with a non-zero exit); advisories go to stderr.

**Flags:**

- `--all` — Remove every active callback owned by the chat (instead of one <callback-id>), then verify none remain
- `--chat` — Owning chat id (default: $BOSS_AGENT_SESSION_ID). Honoured locally as well as remotely: it is the ownership guard, so removing a callback owned by another chat is refused, and it is the routing key for a remote daemon
- `--json` — With --all: emit {removed, remaining_active, chat, pr_number} as JSON on stdout only
- `--pr` — With --all: only callbacks for this pull request number (default: 0)
- `--repo` — With --all: only callbacks for this repository (owner/repo)

```bash
boss callback remove cb_abc123
# tear down every watch this chat armed on PR #123, and prove none remain
boss callback remove --all --pr 123 --json
```
