# Concepts

Shared domain vocabulary for this project — entities, named processes, and status concepts with project-specific meaning. Seeded with core domain vocabulary, then accretes as ce-compound and ce-compound-refresh process learnings; direct edits are fine. Glossary only, not a spec or catch-all.

## Reverse streaming

### Reverse stream

A long-lived bidirectional connection a daemon opens _outbound_ to the orchestrator, inverting the usual client/server direction so the orchestrator can push commands to a daemon it cannot dial directly (the daemon may sit behind NAT). A daemon holds its reverse streams open for its whole lifetime, reconnecting with backoff whenever one drops.

### DaemonStream

The control-plane reverse stream: the daemon sends an initial state snapshot followed by session, chat, and status deltas, and receives orchestrator commands (stop, pause, resume, transfer, webhook dispatch) back on the same connection. One per daemon.

### TerminalStream

The web-terminal reverse stream that carries interactive PTY traffic for the browser terminal feature, kept separate from the DaemonStream so keystroke and output volume can never starve control-plane commands. It multiplexes many concurrent Attaches over one connection, keyed by attach id.

### Attach

A single live binding between one browser terminal and one daemon-side PTY, carried over the TerminalStream.

An Attach owns its PTY: tearing one down must close the PTY _before_ the stream's context is cancelled, or the PTY leaks on the daemon. Attaches do not survive a stream reconnect — the browser must re-attach to recover its terminal.

## Repo automation flags

Per-repo booleans on the `Repo` model that gate what the daemon does
automatically for that repo's sessions. All are consumed in
`bossd/internal/session/dispatcher.go` (event-driven) except Dependabot
auto-merge, which runs in the task orchestrator's poll loop.

### Mark ready for review when checks pass (`CanAutoMerge`)

Despite the legacy field name, this does **not** merge. When a draft PR's checks
pass, it promotes the PR from draft to ready-for-review (`MarkReadyForReview`)
and advances the session state. Regular PRs are never auto-merged: a merge
happens only via the manual `MergeSession` RPC, or for Dependabot PRs. The UI
label was corrected to match; the proto/DB field stays `can_auto_merge`.

### Dependabot auto-merge (`CanAutoMergeDependabot`)

The only flag that triggers a real `MergePR`. The task orchestrator polls repos
with this enabled and auto-merges eligible Dependabot PRs using the repo's merge
strategy. Independent of the dispatcher's session flow.

### Automatic repair (`CanAutoRepair`)

A single per-repo toggle (default on) gating the repair plugin
(`bossd-plugin-repair`), which repairs a PR's failing CI checks, merge
conflicts, and review feedback as one flow. It triggers on the PR's display
status (FAILING / CONFLICT / REJECTED) via the host's `NotifyStatusChange`, and
honors `CanAutoRepair` in the plugin's `lookupSession` (the flag is denormalized
onto the session the plugin reads). An older in-process `FixLoop` in the
dispatcher, with three separate per-behavior toggles, was removed: it was wired
with a nil handler and never ran, so those toggles
(`CanAutoAddressReviews` / `CanAutoResolveConflicts` / `CanAutoFixChecks`) gated
dead code and were collapsed into `CanAutoRepair`.

## Authentication state

### AuthState

The shared signal telling every reverse stream whether the daemon's credential is currently usable ("auth OK") or has been revoked and awaits re-login ("needs login"). A single AuthState instance is shared by all of a daemon's streams so that one logout pauses them together rather than each holding its own drifting view.

AuthState is edge-triggered in both directions: a logout cancels any in-flight stream immediately rather than at the next reconnect, and a later login wakes the paused streams. The two transitions are distinct observable signals — seeing "needs login" is what lets a clean logout be told apart from a stream failure, so an intentional pause is not logged or backed off as if it were an error.

## Chat / session status

### usage-limited

A chat whose coding agent (Claude or Codex) has hit its subscription usage cap and cannot make progress until the cap resets. It is detected read-only — from the CLI's limit banner scraped off the interactive tmux pane (`CHAT_STATUS_LIMITED`, BOS-166) or the exit-log tail of a headless run (BOS-164) — never inferred from ordinary output. When the banner carries a parseable reset time it is threaded through as an absolute instant and rendered as a WARN badge: the chat-level badge shows a bare `limited`, while the session-level badge composes the reset time into `usage-limited (resets ~HH:MM)` (or just `usage-limited` when the reset time is unknown). Session detail additionally names which provider/agent is limited; account-level attribution ("which of my accounts") is out of scope until accounts exist (Epic 3).

`usage-limited` is a display/attention state, not a lifecycle state: it rides the existing `display_label`/`display_intent` transport (no new proto field) and is **orthogonal to the PR-lifecycle session state machine**. In the session badge it outranks `working`/`idle` but loses to `question`. Each limit transition emits exactly one session event (`limit-entered` on enter, `limit-recovered` on leave) from the tracker's change-detection hook — never spamming on repeated polls. It is the read-only detection foundation that Epic 4's automatic account rotation reacts to.

## Epic runs (boss-epic)

### Epic run

One unattended execution of the `boss-epic` skill over an epic's Linear
sub-issues (or an explicit ticket list): build the dependency graph from
blocker relations, implement up to N tickets in parallel in separate bossd
sessions, merge serially in dependency order, and report progress on the
parent issue. All scheduling decisions come from the pure library
`scripts/bs-epic-lib.mjs`; the skill prose is I/O glue only.

### Driver

The long-running chat that runs the boss-epic scheduling loop. It holds no
authoritative state: on (re)start it reconstructs everything from Linear
ticket states plus the daemon's session list, so a killed driver can be
re-launched with the identical command and resume.

### Child session

A bossd session the driver creates for one epic ticket (prompt
`/boss-build BOS-NN`, tracker fields set, `claude` agent by default —
codex-exec sessions have no chat row for mid-run delivery).

### Isolate

The permanent-failure disposition for a child: leave the session and its
work open for a human, mark the ticket failed inside the run, and skip its
transitive dependents. Isolation never stops or deletes the session —
evidence is preserved.

### Serialized merge

The epic-run rule that at most one merge is in flight at any time, in the
order computed by `nextToMerge` (dependency-clean greens first, then
priority, then age), even while implementation runs in parallel.

## Account rotation

### Account

A registered provider credential (Claude or Codex) that Bossanova can run sessions under: a label plus metadata (status, priority, cooldown) in the daemon store, with the secret itself in the OS keyring. The user's pre-existing `~/.claude`/`~/.codex` login is the implicit system-default account 0 — never imported, never injected.

### Rotation

The daemon's automatic response to a usage-capped account: put the limited account on cooldown, select the next eligible account for the provider (active, not cooling, lowest priority first), and respawn/resume the interrupted session under it — posting an in-chat notice, and never auto-resending the interrupted prompt. On by default once extra accounts are registered, with per-repo opt-outs; `managed_accounts.enabled=false` (set via `boss settings --no-managed-accounts`; `--no-rotation` is a deprecated alias) is the global kill-switch that halts all automatic rotation, re-read live per decision, while manual `boss account switch` keeps working. Every decision is audit-logged as a rotation event (labels only, never credentials).

### Cooldown

The per-account "do not select until T" window applied when an account hits its usage cap, where T is the reset time parsed from the provider's limit message (or a conservative default when unparseable). Persisted on the account, so restarts don't forget it.

### Limited

The status of an account (or a whole session, when every account for its provider is cooling) that has hit a usage cap. An all-accounts-limited session parks with an "all accounts limited until ~T" badge — T being the earliest cooldown expiry — and resumes automatically at that reset, emitting a single notification per episode.

## Build caching

### Facade

The `make` → `bazel` delegation (BOS-339): `make test` and its smoke/per-module variants run `bazel test //...` under the hood rather than a raw `go test` loop, presenting a stable `make` interface over the Bazel build graph. Falls back to the native per-module `go test` loop when `bazel` is absent or `BOSS_NO_BAZEL=1` (keeps the bufless public mirror green).

### Ledger

The reconciliation between what `bazel test //...` runs and what `make` must still cover natively. A small set of tests are tagged out of the sandbox (`manual`/`local`); the ledger (`scripts/bazel/ledger.json`, mirrored in the sandbox-patterns doc) records each exclusion and its native fallback so `make test` runs them as an extra `go test` pass and no coverage is lost.

### Stamp

A content-hash gate over a step's inputs: the step is skipped when the hash is unchanged. Used for cached lint/gen layers (e.g. `GEN_STAMP`, the cached-lint stamp) so unchanged inputs don't re-trigger expensive work.

### Disk cache

Bazel's machine-wide, content-addressed action store (`~/.cache/bazel-bossanova-disk`), shared across every worktree of the repo on one machine. Because keys are content hashes, cross-worktree reuse is safe by construction: a fresh worktree of the same commit serves most of its actions from this cache instead of rebuilding.

### Remote cache

The secret-gated BuildBuddy action cache (`grpcs://remote.buildbuddy.io`) that extends caching **across machines** (dev + CI). It activates only when a gitignored, per-worktree `.bazelrc.user` provides `build --config=remote` plus the `BUILDBUDDY_API_KEY`; absent that file the build is disk-cache-only and never errors. `make setup-worktree` propagates the file to new worktrees; the key is never committed. The same file also enables the **Build Event Stream** (`--bes_backend` + `--bes_results_url`, `fully_async`): BES is what uploads each invocation to the BuildBuddy "Builds" dashboard — `--remote_cache` on its own only feeds cache metrics and leaves the dashboard reading "No builds found". BES is a **local-dev** affordance only; CI uses the remote cache without BES, because a BES upload error is fatal (`exit 38`) and CI must not hard-depend on the free-tier BES endpoint (a cache miss, by contrast, is non-fatal).
