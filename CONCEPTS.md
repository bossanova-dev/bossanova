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

Delivery is not queued, at either end. Terminal input sent while an Attach is not connected is discarded rather than held, and input for an attach id the daemon no longer knows is dropped too — so the browser can never assume a send landed. Terminal size is the deliberate exception, and the repair lives in the browser rather than in the connection: a new Attach starts at a default size, and the browser resends the latest size it remembers once the Attach is up. A one-shot action carrying text the user authored has no such repair, so it cannot be dispatched optimistically — it must be refused, visibly, while the Attach is not deliverable.

Attaching to a chat whose stored pane pointer has been cleared is a revival, not an error: a cleared pointer means the chat was torn down, not that it is dead, so the attach wakes the chat and the wake re-persists the pointer. Both the TUI and web terminal take that path, so "attach to a torn-down chat" means the same thing on either surface. A wake may succeed while still losing the prior conversation — a fresh-fallback resume — and that degradation is told to the user in the terminal's own output stream, since an empty pane with no explanation is indistinguishable from a bug.

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

### Blocked reason

The single human-readable field on a session that says why it is not progressing — the text an
operator reads first when a session stalls, and the only durable record of the cause. It has many
producers (draft-PR creation failures, agent auth failures, stalls) but no history: each producer
overwrites whatever the last one wrote.

That last property is a design constraint, not an implementation detail. A writer that replaces a
_specific_ reason with a _generic_ one destroys the diagnosis, so any automated actor that both
**selects** sessions on the blocked reason and **triggers** a code path that rewrites it must verify
the operation's precondition itself and skip when it does not hold — an attempt that is certain to
fail is not free, because failing is itself a write. Reason text is also structured enough to be
recognised by its producer (a draft-PR failure is identifiable as such), which is what lets a sweep
select on it — and what makes an overwrite keep matching the same selector.

That recognition is a **prefix** test, not a tag field, and that shapes how a producer may refine
its own reason. A narrower kind — a draft-PR failure that is a passing remote outage rather than a
real misconfiguration — nests inside the producer's recognisable opening rather than taking a
parallel one, so every selector written for the broad kind keeps matching the narrow one and the two
predicates form a strict hierarchy. A genuinely different state (a draft PR that is still being
created, say) does take its own form, because it must _stop_ matching. The discriminator also leads
the text rather than trailing it, so a surface that clips a reason to one line keeps the framing and
loses only the raw cause; surfaces are then free to render the same reason differently, which is a
deliberate divergence rather than a parity defect.

### Waiting reason

The human-readable text saying why a chat is parked on an armed external event — it names the
trigger being awaited and the pull request it is armed on. Distinct from a Blocked reason: a waiting
chat is healthy and progressing toward a known event, not stalled on a fault, so it is styled as
information rather than as a warning.

Unlike a Blocked reason, it has exactly one producer. The daemon derives the canonical wording and
every surface renders that text verbatim rather than re-spelling it; the surfaces are required to
read identically, so changing the wording changes all of them at once — here parity is the contract,
not the divergence. The reason carries no status label of its own, because every surface that shows
one already displays the waiting badge and its spinner alongside it, and a label in the text would
only repeat what is on screen while pushing the informative half of the line rightward. A reason that
is absent or missing any of its parts is rendered as nothing at all rather than as an empty row:
callers skip the line entirely, so the layout does not shift and no unselectable row is left for a
cursor to strand on.

## Chat panes and reaping

### Chat pane

The terminal pane that hosts one chat's interactive coding-agent process, on the machine running the
daemon. A chat has at most one pane, and the pane is the only place the agent's live output exists —
it is a runtime resource, not a record, so losing it loses nothing durable.

### Pane pointer

The chat's stored reference to its pane. Set means "this chat has a pane"; cleared means "this chat
was deliberately torn down". The pointer is the project's only durable signal of intent about a
pane's absence, which is why it is load-bearing well outside the code that writes it: a cleared
pointer marks a chat as parked and attaching to it revives the chat, whereas a chat that still
points at a pane which is no longer alive is read as the agent having exited, and finalizes its
session. Those two readings are opposite, so any operation that both destroys a pane and clears its
pointer must be ordered so that its reachable partial-failure state is the harmless one.

### Reaping

The daemon's periodic sweep that kills panes it judges no longer worth keeping. It acts on two
independent reasons and never confuses them: an **orphan reap** kills a pane that no live row claims
at all, and an **idle reap** kills the pane of a chat that is genuinely still in use but has been
reported idle, with no visible output, for longer than a configured window.

The two reasons differ in reversibility, and everything else follows from that. An orphan reap
destroys the only trace of whatever was running, so it is opt-in and conservative. An idle reap is
recoverable — the chat row survives, the chat stays listed, and attaching wakes it — so it ships on
by default. Both require a candidate to be seen eligible on more than one consecutive sweep before
acting, and each reason carries its own confirmation marker so a sighting for one reason can never
confirm the other.

Because reaping is the daemon's only destructive sweep, its rule on evidence is inverted from the
rest of the system: it must **under**-reap when unsure. A pane whose identity is ambiguous keeps, a
chat with no current telemetry keeps (absence of evidence is unknown, never idle), and a chat that
could not be resumed with its history intact keeps regardless of age — reaping that one would
destroy context rather than reclaim memory. The idle clock is in-memory, so a daemon restart resets
it; on a host restarted more often than the window, idle reaping simply never fires.

## Scheduled sessions (cron)

### Cron gate

The optional command a scheduled job runs immediately before it would fire, to decide whether there
is actually work to do. Exit zero means fire; anything else blocks the fire, creates no session, and
still advances the job's next run time. The gate is a decision procedure, not a health check — it is
the job's own answer to "is there anything to do right now".

The contract is deliberately **fail-closed**: when the gate's condition cannot be established, the
fire is blocked rather than attempted, because an unverifiable condition should skip the run rather
than spend agent tokens on a state nobody confirmed. Failing closed is therefore correct by design,
and is separate from — never a substitute for — recording _why_ the fire was blocked. Gate authors
signal "no work" with a plain non-zero exit; the shell's own "could not run what you asked" codes are
reserved, and a gate that borrows one is reported as broken rather than as a skip.

### Gate outcome

The recorded verdict of a cron gate run, and the durable half of the fail-closed contract. It
separates two states that both block the fire but carry opposite operator instructions: the gate
**ran and said no**, which is a healthy skip, and the gate **could not be evaluated at all** — never
launched, not executable, timed out, killed by a signal, or never configured — which means the gate
condition is unknown and nothing is being checked.

Only the second is a failure. It derives a failed, red cron row so a broken gate is escalatable,
while a healthy skip stays a warning-styled "waiting" row. The distinction is load-bearing precisely
because it is invisible when collapsed: a broken gate that records the healthy verdict renders as
positive evidence that the system is working, so no alert, dashboard, or human scan of cron history
can catch it. Compare **Capability preflight**, which carries the same
distinguish-your-causes principle scoped to an agent runtime's readiness rather than to a scheduled
job's recorded history.

A gate outcome is served to API clients, not merely logged, so moving a case from one verdict to the
other changes an existing client's observable answer even when no schema field changes — a
behavioural API change that must be versioned and down-converted for older clients.

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

## Tracker seam

### Tracker adapter

The object that adapts one issue tracker to the tracker-agnostic interface the portable planning and
build skills program against, so a skill can select, read, and update tickets without naming a
particular tracker. A repo supplies one adapter per tracker it uses; the interface is deliberately
dependency-free, so another repo can vendor it and write an adapter for its own tracker without
pulling in the reference implementation.

An adapter is checked for conformance over its **whole declared surface at once**, without reference
to which members any caller actually invokes — so anything the contract marks required is a
precondition for the adapter being admitted at all. That check is a declaration gate proven by each
adapter's own test suite, not a runtime call on the path that resolves an adapter, so a
non-conforming adapter fails its author's tests rather than a live session.
The contract separates what an adapter MUST declare from what it MAY: an omitted optional member
conforms, while a declared one is validated exactly as strictly as a required one. Only the meaning
of absence differs between the two tiers. A member earns required status only when a skill's control
flow cannot proceed without it; one that every caller already treats as best-effort, or that no
caller invokes, belongs in the optional tier — requiring it instead rejects otherwise-usable
adapters over something nothing depends on at call time.

### Tracker capability

A unit of adapter behavior the skill invokes as ordinary code, where the adapter itself computes the
answer. Contrast a Tracker operation: a capability is satisfied by supplying an implementation.

### Tracker operation

A unit of adapter behavior the skill performs by having the **agent** call one of the tracker's MCP
tools. The adapter supplies the tool's name and a one-line summary of its use rather than executing
anything itself, which is what distinguishes an operation from a Tracker capability — code for a
capability, a tool name for an operation.

### Plan contract

The versioned agreement fixing which top-level sections a ticket's plan description carries and how
each one is classified — always emitted, emitted only in a named circumstance, or merely recognised
and never required. The planning skill is its sole producer; the build and sweep skills are its
consumers.

The contract version is stamped inside the plan description itself rather than tracked out of band,
so a consumer reading a plan it did not write can tell which version it is holding, and a plan
written before stamping existed is read as the first version. Adding or renaming a section changes
the contract; registering a section as recognised-but-never-required does not newly require it of
plans already stamped against an earlier version.

## Agent runtime gating

### Agent runner

A plugin that owns one coding-agent CLI's subprocess lifecycle on behalf of the daemon — starting
runs, building interactive commands, and reporting the agent's capabilities. Sessions cannot start
unless at least one is loaded; the daemon stays healthy without one and fails the session request
instead.

### Capability preflight

The check an agent runner performs, before a gated run starts, that the agent runtime can actually
do the work the run requires — and which fails the run closed when it cannot, rather than letting it
start and discover the gap mid-flight. Its defining obligation is that it must profile the **same**
runtime the gated run will get: same account home, same model, same environment, and the same
working directory, since agents discover per-repo configuration relative to where they run. A
preflight that profiles anything else reports on a runtime nobody will use.

A preflight verdict is only as trustworthy as its ability to tell apart causes with opposite fixes.
"The repo declared nothing" and "the credential never reached the session" both surface as an empty
capability inventory, so a preflight must classify the empty case explicitly instead of deriving a
list of unmet requirements from it — otherwise every credential failure instructs the operator to
repair a declaration that is already correct.

### Headless capability profile

A named, versioned contract naming the set of runtime operations an unattended run needs before it
is allowed to start — the thing a capability preflight checks against. Requesting no profile means
no gating. Each profile is an exact allowlist of real operations rather than a pattern or a set of
plausible synonyms, because a name no runtime can satisfy buys no tolerance and only risks admitting
a runtime the caller cannot actually drive.

## Account rotation

### Account

A registered provider credential (Claude or Codex) that Bossanova can run sessions under: a label plus metadata (status, priority, cooldown) in the daemon store, with the secret itself in the OS keyring. The user's pre-existing `~/.claude`/`~/.codex` login is the implicit system-default account 0 — never imported, never injected.

### Rotation

The daemon's automatic response to a usage-capped account: put the limited account on cooldown, select the next eligible account for the provider (active, not cooling, lowest priority first), and respawn/resume the interrupted session under it — posting an in-chat notice, and never auto-resending the interrupted prompt. On by default once extra accounts are registered, with per-repo opt-outs; `managed_accounts.enabled=false` (set via `boss settings --no-managed-accounts`; `--no-rotation` is a deprecated alias) is the global kill-switch that halts all automatic rotation, re-read live per decision, while manual `boss account switch` keeps working. Every decision is audit-logged as a rotation event (labels only, never credentials).

### Cooldown

The per-account "do not select until T" window applied when an account hits its usage cap, where T is the reset time parsed from the provider's limit message (or a conservative default when unparseable). Persisted on the account, so restarts don't forget it.

### Limited

The status of an account (or a whole session, when every account for its provider is cooling) that has hit a usage cap. An all-accounts-limited session parks with an "all accounts limited until ~T" badge — T being the earliest cooldown expiry — and resumes automatically at that reset, emitting a single notification per episode.

### Usage probe

The daemon's periodic read-only check of how much of an account's provider quota remains, reported as one Utilization window per quota period, which keeps rotation's eligibility decisions current. Where the provider exposes a dedicated usage endpoint the probe is free; where it does not, or where the credential lacks the scope to read it, the fallback path issues a real metered call — so a probe that silently takes the fallback is an ongoing cost, not a cosmetic defect.

### Utilization window

A rolling quota period an account is measured against, together with the fraction of it consumed — one short (intra-day) window and one weekly window, both produced by the Usage probe and thresholded by Rotation: at or above full the account counts as Limited, and a lower band triggers a proactive look for somewhere better to put the session. Each window also carries the instant it resets, which is what times a Cooldown.

By contract each window is a single fraction, but a provider does not always report it that way: the weekly quota may arrive as an account-wide reading, as separate per-model readings, or — on plans that split the week by model — only per model. The account-wide reading is the account's verdict even when a per-model reading is higher, because one model's spent week does not bench the account for traffic to other models; the per-model readings decide only when no account-wide reading was reported. Reducing several readings to the one fraction Rotation consumes is a decision, not an aggregation, and an absent reading is not a reading of zero.

### Probe throttle

The provider's usage endpoint refusing the daemon's own polling rate. It is evidence about our request volume, not about the account's quota, so it is deliberately not a Cooldown and does not make an account Limited: nothing is written to the account's stored state and its real capacity is untouched. The only correct reaction is caller-side backoff before the next poll, applied in memory by the refresh loop and forgotten on restart. A retry horizon stated by the provider is treated as an unvalidated hint and bounded at both ends before use, since neither an absent value nor an implausibly long one may be honoured literally.

## Multi-instance owner routing

### Daemon token authority

The Redis-backed fleet-wide mapping from a hashed daemon session token to its daemon and user identity. Any bosso pod can authenticate a daemon request; raw tokens are never Redis keys or persisted values.

### Owner claim

A short-lived Redis claim mapping a daemon, session, or chat to the bosso instance holding its live `DaemonStream`. Claims are routing metadata, refreshed by the owning registry, and rechecked at the target before pod-local state access.

### Owner dispatch

Finite daemon commands resolve a global owner and use one dispatcher: local owners enter the local stream registry; remote owners use the authenticated internal command relay. Fleet operations enumerate all ready owner claims and dispatch with bounded concurrency. Commands are never retried after ambiguous delivery.

### Raw stream routing

Long-lived transports (`DaemonStream`, `TerminalStream`, attach/create/chat streams, attach-token issuance, and WebSocket attach) are proxied directly to the current owner. Load-balancer affinity is never a correctness mechanism; an exhaustive RPC catalog and source-level boundary test enforce the distinction between raw streams and distributed finite commands.

## Chat coordination

### Broadcast

One message delivered to every agent chat a selector resolves to, durably and with retries, by _waking_ each target chat and handing it the message as a prompt. The sibling primitive to a GitHub PR callback: a callback is a one-shot "notify this chat when that PR does X", a broadcast is "tell this whole audience X now". Two things are easy to get wrong: the audience is resolved **once**, at send time, so chats created afterwards never receive it; and the message body is a **secret** that is delivered verbatim but never echoed back on any list or inspect surface.

### Selector

The textual grammar naming a broadcast's audience, over six dimensions — `chat`, `session`, `repo`, `agent`, `account`, `daemon`. A selector is a disjunction of clauses: `,` joins terms inside one clause (different dimensions AND, repeated values of one dimension OR) and `+` joins clauses (OR). An **empty selector is a hard error, never "match everything"** — that rule is the only thing between a typo and a daemon-wide message storm. Selectors carry ids, not credentials, so unlike a broadcast body they are safe to log.

### Broadcast subscription

A standing rule that sends a broadcast when a session settles: it names a trigger (`completed`, `errored`, or `settled`), the session whose outcome fires it, and the selector to resolve **at fire time**. It is how a coordinator learns a child session finished without polling its transcript. One-shot like a callback — once fired (or canceled or expired) it no longer stands.

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

## Skill distribution

### Published skill core

A skill whose payload Bossanova installs into the user's **global** agent skill directories, where it
appears in every repository on the machine rather than only in this one. That reach is what makes a
core's project-agnostic phrasing a hard requirement: repository-specific behavior reaches a core only
through repo-local configuration and repo-local extensions, never by naming this project inside the
core.

### Toolbox

The set of shared helper modules a skill carries alongside its instructions. A toolbox is distributed
by **copy**, not by reference: each skill's payload holds its own copy of every helper it uses, and
the install writes another copy into the global skill directory.

Because the copies are independent, they age independently. A helper that reads repository-owned
configuration is therefore always potentially older than the configuration it is reading, and any
vocabulary the configuration gains reaches some copies long before others.

### Toolbox drift

The condition where an installed copy of a toolbox helper is older than the source that produced it.
Drift is reported as a warning at skill preflight rather than enforced as a gate, on the reasoning
that a stale helper degrades rather than fails — which holds only for helpers that do not hard-reject
what they do not recognise.

## Local attach paste path

### Paste claim

The decision, made as a bracketed paste arrives at a local terminal attach, whether boss consumes
the pasted body and substitutes something of its own or re-emits it to the agent verbatim. A paste
is claimed or passed through; there is no partial handling, and an unclaimed body must reach the
agent byte-for-byte with its original framing intact.

Claiming is installed only where boss is positioned to act on the content, so an attach with nothing
to do never inspects a paste at all. "Not claimed" is therefore the answer to two different
questions — the content was not claimable, or no claimer was ever installed — and separating them is
the first diagnostic step for any paste that reaches the agent unchanged.

### Status-line overlay

A transient one-row message painted onto the last row of the outer terminal while a full-screen
agent owns the screen: the cursor is saved, the row erased and written, and the cursor restored. It
is a flash rather than a widget — the agent redraws over it on its next frame — and it is the only
feedback channel available once the terminal has been handed to the agent.

An overlay must fit one row measured in display cells, because a write that reaches the right edge
of the last row wraps, scrolls the agent's whole frame up a line, and leaves the cursor restore
addressing different content — a notice that moves the pane it exists to explain. Where an overlay
mixes fixed guidance with a variable fragment, the fragment is the half that gives way: the fragment
is bounded in every unit that any cap on the render path measures, so no blunt truncation can reach
the guidance.

## Review and verification

### Vacuous gate

A check that passes on the very values it exists to reject, so its verdict carries no information
while still reading as assurance. It is worse than an absent gate rather than equivalent to one: an
absent gate leaves a risk everyone downstream still treats as unchecked, while a vacuous one converts
that risk into a false assurance every consumer spends.

A gate can become vacuous at any of three layers, and each has been observed independently: its
parser, when it splits on a delimiter the operand may legally contain; its classifier, when the
written form of a marker is not the form the test recognises, so the item silently leaves the
contract with no detector anywhere; and its own regression test, when that test borrows the
definition it was supposed to check independently — normalising the very bytes it exists to pin, or
measuring a bound in the same unit the guarded code counts in, so it can only ever confirm that the
code agrees with itself. The failure is silent by construction, because a gate's job is to be quiet
when nothing is wrong.

### Falsification

A test written to prove that a specific guard is load-bearing: it constructs the exact value the
guard exists to reject, asserts the guard refuses that value for the stated reason rather than merely
failing, and is confirmed by mutating the guarded branch away and watching the test die.
Falsifications are named and numbered alongside the guard they defend, and stand as the durable
evidence that a gate is not vacuous.

Asserting only that a good input still passes is not a falsification — the refusing direction is the
whole record. A falsification that would stay green with its guard deleted is itself vacuous, one
level up, which is why the mutation step is part of the definition rather than an optional check.

## Flagged ambiguities

- "Capability" carries two unrelated senses. A **Tracker capability** is an adapter behavior a skill
  calls as code; the sense in **Capability preflight** and **Headless capability profile** is an
  operation a coding-agent runtime must support before a gated run may start. Neither gates the
  other, and a conformant tracker adapter implies nothing about an agent runtime's profile.
- "Gate" carries several unrelated senses and should never appear unqualified. A **Cron gate** is a
  command a scheduled job runs to decide whether it has work; a **Repo automation flag** gates what
  the daemon may do to a repo; a declaration gate (see **Tracker adapter**) admits an adapter; a
  **Stamp** gates a build step on an input hash. Only the cron sense has a recorded **Gate outcome**.
- "Rate limited" had been used for both a **Limited** account, which has exhausted its own usage
  cap, and a **Probe throttle**, which is the usage endpoint refusing our polling rate — these are
  distinct, and only the former justifies a **Cooldown**.
