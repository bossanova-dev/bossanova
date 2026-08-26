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

### Draft PR placeholder commit

An empty scaffolding commit whose only job is to make a draft pull request possible before a branch has reviewable work. It is not evidence of user work, and commit-message policy must treat it differently from non-empty work commits.

The placeholder may carry stale PR-number tags from earlier tooling, but those tags do not change its identity. Guards that ask whether a branch has real work must classify the placeholder by its semantic role rather than by a single literal subject shape.

### PR tag

A pull-request number marker embedded in a commit subject so repository automation and humans can associate branch commits with their review surface. It belongs on non-empty work commits; scaffolding commits that carry no work are exempt even when the branch as a whole needs tagged commits.

## Authentication state

### AuthState

The shared signal telling every reverse stream whether the daemon's credential is currently usable ("auth OK") or has been revoked and awaits re-login ("needs login"). A single AuthState instance is shared by all of a daemon's streams so that one logout pauses them together rather than each holding its own drifting view.

AuthState is edge-triggered in both directions: a logout cancels any in-flight stream immediately rather than at the next reconnect, and a later login wakes the paused streams. The two transitions are distinct observable signals — seeing "needs login" is what lets a clean logout be told apart from a stream failure, so an intentional pause is not logged or backed off as if it were an error.

### LoginVerification

The three-outcome verdict `boss login`'s save path returns after reading the credential record back inside the same lock it just wrote under, so a keychain that reports a successful write can no longer be trusted at face value. Outcomes are `LoginVerified` (the read-back matches the login just performed), `LoginVerifyRecordNotUpdated` (the read-back proves the record does not reflect it — absent, still flagged for re-login, missing a token, or holding a different access token), and `LoginVerifyInconclusive` (the read-back itself could not complete — a keychain that hung past the shared lock-acquisition timeout, an undecryptable record, or a cancelled context). The outcome enum's zero value is a deliberately invalid "verification never ran" sentinel, so a caller that forgets to set it cannot be mistaken for a verified login. Both the CLI and the TUI gate their "Logged in" success path on this one verdict rather than each deriving its own from the save call's error alone.

### Login verdict

The daemon's own answer to "did that login actually leave me able to talk upstream?", produced after it reloads credentials in response to a login notification and returned to the CLI so a login can no longer report an unqualified success while the daemon stays parked. Distinct from **LoginVerification**, which is the CLI-side read-back proving the record was written: the login verdict is about whether the credentials the daemon subsequently loaded are usable by the daemon.

The verdict is an enumerated non-secret outcome plus, only when the record was flagged, the enumerated re-login reason — never token material. Its outcomes separate causes that have opposite remedies: usable credentials with the auth gate cleared; a record still carrying a persisted re-login marker; a record with neither a token nor a marker, meaning the login's credentials never reached this process; and clean credentials whose gate was cleared but whose proactive re-register failed, which is reported rather than fatal because the reactive path remains the backstop. The outcome's zero value means "nothing was evaluated" — what a logout, an absent notifier, or a daemon too old to have a verdict reports — and it must render as silence rather than as a false success, so a newer CLI against an older daemon degrades to saying nothing instead of claiming an OK the daemon never gave.

## Chat / session status

### Effective runtime

The model and reasoning-effort values Boss resolved for a session at launch time, after applying request fields, per-agent plugin settings, and agent-owned defaults. It is historical session metadata: later plugin-default changes do not rewrite what an existing session says it ran with.

### Requested runtime

The model or reasoning-effort preference a caller supplies when creating or launching a session. An omitted requested value is an instruction to use the relevant agent's default, not proof that the effective runtime is unknown.

### Duplicate target

The work identity a live session reserves so another session cannot start the same work at the same time. A target can be a tracker issue, a pull request, or a branch; dead or terminal sessions release their targets, while active sessions keep them reserved.

The target kind matters. A session can reserve one kind without reserving another, so carve-outs must be made at the target-key level rather than by treating the whole session as either blocking or non-blocking.

### Planning session

A session whose plan is to draft or refresh a tracker issue rather than implement it. It may carry the tracker context that the eventual build session will use, but that context is a handoff record, not proof that implementation work already owns the issue.

A planning session should not reserve its tracker issue as a **Duplicate target**. If it has a concrete pull request or branch target, those targets still reserve normally because they represent specific work surfaces rather than the planning handoff itself.

### Liveness signal

An affirmative marker scraped from a **Chat pane**'s current screen that says the agent is
doing work right now — as distinct from inferring activity from output changing over time.

A liveness signal is only valid for the lifetime of the turn that painted it. Most such
markers are self-evicting: the agent redraws them away the moment the work they announce
finishes, which is what lets a bare match count as proof. That property belongs to the
renderer, not to the bytes — a turn that dies mid-work stops repainting and freezes its last
frame, so the marker persists indefinitely and goes on asserting work that has ended. A marker
whose disappearance depends on a live writer therefore needs an explicit freshness bound:
positive evidence, in the same frame and below the marker's last occurrence, that the turn
producing it has finished. The bound reads the last occurrence because a turn started after an
earlier one ended is live again, and the earlier turn's terminators sit above its marker.

The bound is deliberately asymmetric: evidence that proves nothing either way leaves the
signal live, because a chat wrongly read as idle can be interrupted mid-work by an automated
lane, while one wrongly read as working only waits. The freshness rule is also owned by a
single predicate shared by every reader of a marker — liveness is consumed on more than one
path, so a rule fixed on one path leaves the others asserting the old answer. For the same
reason a terminator grammar is written for the lifecycle question specifically, never borrowed
from a classifier built to answer a different one about the same lines.

### Activity floor

A status timestamp that records when a chat's captured surface last changed _at all_, rather than
when the agent last did work. It is a lower bound and nothing more: a spinner repainting its elapsed
counter advances it indefinitely, so it cannot separate a working chat from a stalled one, and a chat
first observed during a poll tick is stamped with that tick's single instant, so a whole session's
chats can carry an identical floor with no observation behind any of them.

A floor answers "nothing has happened since" and never "something is happening now". A rail needing
the latter reads a **Liveness signal**, together with the separate markers a poller keeps for
substantive output and for a still-unobserved seed. Because the collision case makes a staleness
comparison unsatisfiable rather than merely wrong, a rail built on a floor fails as a silent stall,
which is indistinguishable from ordinary waiting.

### Delivery state

The machine-readable outcome of attempting to submit a message into an agent chat. It distinguishes a confirmed submit, a confirmed non-submit, an unconfirmed submit, and a queued message from paths where no submit verifier ran, so callers do not have to infer retry policy from human notice text.

A queued delivery is successful but deferred behind the agent's current turn, so callers must not resend it. An unconfirmed delivery is explicitly unknown: the pane could not be verified, so the user should inspect before retrying. Older proxy paths can omit the state, in which case callers fall back to notice guidance rather than treating the missing state as a clean result.

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

### Chat title

The human-readable name of a chat. It has **several writers** — a user rename, a caller-supplied
title at creation, and more than one best-effort backfill that derives a name from the chat's own
transcript — and, like a Blocked reason, it keeps no history: each write replaces the last.

A **placeholder title** is one no human chose: the empty string, or the literal `New chat` a
freshly-created chat starts life with. Everything else is a real name. That distinction is the whole
precedence rule for **title writes**: an explicit rename always wins, and an auto-derived title may
only ever fill in a placeholder — never overwrite a real name. A derived title is not evidence of
intent (it is read back out of the transcript, typically the first user message), so a writer that
clobbered a real name with one would be destroying the only durable record of what the user called
the chat, in exchange for a name it can always re-derive later.

Because the derivations are best-effort and repeat, **skipping a title write is cheap and taking one
is not**: another backfill will fill a placeholder in on the next pass, so a writer that cannot
establish what the stored title currently is must skip rather than guess. That also makes freshness
part of the rule — a chat can be renamed while it is open, so a writer checks the stored title
immediately before writing rather than trusting one it read earlier.

The rule is scoped to title writes. It says nothing about deletion: a chat can still be reaped whole
on other evidence, and a real name does not by itself protect the row.

### Archive

The terminal state of a session whose work has landed: its isolated workspace is released and the
row is stamped as closed, so the session stops being offered as something to resume. Archiving is
normally automatic — the daemon archives a session once its change is merged, without anyone asking
— which means it is triggered by an external event rather than by a user action, and can begin at
any moment, including while the daemon is shutting down.

Archiving is destructive before it is durable: the workspace is released first and the closed stamp
written last, so an archive cut off midway leaves a row that still reads as active pointing at a
workspace that is gone. That is the same partial-failure ordering hazard the **Pane pointer** states,
with the opposite resolution available — the steps cannot be reordered, so the archive is instead
joined to shutdown and waited out as a **Drain**, on the far side of the wait that proves the things
which start archives have stopped.

An archive is also started by a periodic repair pass, for sessions whose change merged while the
automatic trigger was unreachable. That pass selects on a _combination_ of fields rather than an
explicit marker — no closed stamp, and a merged state — so the combination is a shape no other writer
may be caught wearing, and one that is only safe to act on if the row is re-read immediately before
the archive is dispatched rather than trusted from the list the pass began with.

### Resurrect

The reverse of an **Archive**: an archived session is brought back — its released workspace is
recreated from the branch it left behind, the closed stamp is cleared, and a fresh agent process is
started against it. Only an archived session can be resurrected; the operation refuses a row that is
already live.

Resurrecting records no durable intent. Clearing the closed stamp is all it leaves behind, so nothing
downstream can tell a session someone deliberately resurrected from one the daemon has merely not
archived yet. That is why a resurrect must also leave the terminal state in the _same_ write: a row
that is un-archived while still wearing its terminal state is at once the shape the merged-but-
unarchived repair pass exists to heal, and a row wedged on its own terms, since a terminal state
permits no lifecycle event and so nothing else would ever move it. Written as two steps, the row is
briefly readable in exactly that shape — the same partial-failure ordering hazard the **Pane pointer**
states, on a database row rather than a runtime handle, but with the stronger resolution available:
instead of ordering the steps so the reachable intermediate state is the harmless one, a single
conditional write makes the intermediate state unobservable.

A resurrect is not finished when that write commits, either. Recreating the workspace and starting
the agent can still fail afterwards, and a failure that leaves the row un-archived strands the
session: live, agent-less, refused on retry by the archived-only guard, and immovable by any
lifecycle event. So a failed start compensates, with one atomic write that returns the row to the
closed stamp and state it originally wore — not a fresh stamp, which would restate a long-archived
session as newly archived and move it in the trash order.

Recreating the workspace is idempotent, which is what keeps a second attempt from wedging on the
debris of the first: a workspace already present and checked out on the branch the session left
behind is adopted as it stands rather than rebuilt, and a registration left over from a workspace
that was released out of band is cleared before a new one is created. Neither signal decides that
alone — a released workspace stays registered until the registration is cleared, so registration
proves only that the workspace once existed, and presence has to be read from the directory itself.

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

Reaping is not the daemon's only destructive sweep — the merged-but-unarchived repair pass described
under **Archive** dispatches archives, which release workspaces — but it is the one that destroys the
only trace of live work, so its rule on evidence is inverted from the rest of the system: it must
**under**-reap when unsure. A pane whose identity is ambiguous keeps, a
chat with no current telemetry keeps (absence of evidence is unknown, never idle), and a chat that
could not be resumed with its history intact keeps regardless of age — reaping that one would
destroy context rather than reclaim memory. The idle clock is in-memory, so a daemon restart resets
it; on a host restarted more often than the window, idle reaping simply never fires.

## Input delivery

### Composer

The agent's text-entry row in its pane — the one place a keystroke becomes part of a message rather
than a selection. It is a mode, not a widget: the same rows show a composer one moment and a
selection UI the next, and nothing outside the pane is told which. Everything in this cluster exists
because that question can only be answered by looking at what the pane currently renders.

### Ready marker

The per-agent glyph that marks a live composer row. Each agent declares its own, and the delivery
gate resolves a composer by finding the bottom-most row whose own leading glyph is that agent's
marker.

A marker match is evidence, not proof. The agent picks its marker for looks rather than for
uniqueness, and commonly reuses the same glyph as a selection cursor, so any state that draws the
marker at the head of a row is indistinguishable from a composer to a rule that only asks whether a
row starts with it. A match therefore establishes that a row _could_ be a composer, and must be
corroborated by something else before anything is typed into it.

### Boot interstitial

A screen an agent draws at startup that takes the keyboard before any conversation exists — an
update prompt, an onboarding picker. It is the hardest case for the **Delivery gate** for two
reasons: it appears only on the start path, which is the path with no conversation to reason about,
and it usually renders as a numbered menu led by the agent's own **Ready marker**, so the naive
composer test matches it.

An interstitial blocks input while asking nothing anyone needs to be told about, which is the
counterexample that keeps the gate's two predicates independent. Recognising one is per-agent work —
the grammar belongs to the agent's own plugin — so an agent with no clause for its boot screen is
unprotected however complete the surrounding plumbing looks.

### Delivery gate

The check performed before typing into a pane: wait for a composer to appear, then confirm that what
appeared is a composer rather than a menu. The two halves are separate and neither is redundant —
the first answers "is anything ready", the second answers "is the ready-looking thing safe to type
into".

Two predicates about a pane are routinely confused, and neither may be inferred from the other:
whether the pane **blocks input**, meaning a keystroke is consumed as a choice, and whether the agent
is **asking** something a human should be notified about. A conversational question posed with a live
composer is the second without the first; a **Boot interstitial** is the first without the second.
Gating delivery on the asking predicate refuses to answer the very question that was asked; gating
notification on the blocking one stays silent about real questions.

The gate fails **open**: a check that is unavailable or errors delivers rather than refusing, because
the check crosses a process boundary and that population of failures is dominated by causes carrying
no evidence about the pane, while failing closed would turn any hiccup into a session that cannot
start. Fail-open is defensible only while it is visible — a degraded gate must announce its
degradation once per occurrence, since silent fail-open is a gate deleted without anyone deciding to
delete it.

Its two error directions are not comparable, and grammars are sized on that asymmetry rather than on
symmetry. A wrong refusal is loud, names itself, and is retried; a wrong delivery presses a key in a
menu whose selected row may be irreversible. Where the two conflict, over-refuse — and pin the
accepted wrong refusals with tests that say so, so that a later reader who finds a way to narrow the
grammar tightens it deliberately instead of reverting it as a bug.

## Session environment

### Inherited path

An absolute filesystem path that one process resolves once and exports into a session's environment,
where every later command in that session reads it as the location of a tool. It is the counterpart
of a bare name looked up on a search path: it names an exact file instead of asking the system to
find one, so it is unambiguous, it outranks any search, and nothing revalidates it.

An inherited path is a hint, never a guarantee, because its correctness is bounded by the lifetime of
whatever holds the file it names — and that container can be reclaimed independently of both the
process that exported the path and the session still reading it. The value does not change when the
file disappears; an environment has no invalidation. So an inherited path is validated at the moment
of use rather than at the moment of export, and it is consulted as the first candidate of a chain
whose later candidates must still be reachable when it fails. A chain that accepts its first
candidate for being _set_ rather than for being _usable_ has no fallback at all, because the arm most
likely to be stale is the one it consults first. When no candidate resolves, the failure names each
one tried and why it lost: a tool that cannot be located is otherwise indistinguishable from a
capability the environment does not have, and that confusion turns a missing file into a silent,
permanent degrade (see **Callback**).

## Boss-build runs

### NO_CHANGE

A `boss-build` terminal state meaning the run intentionally produced no implementation change because
there was no eligible work it could safely own, such as no candidate, a lost claim, or foreign work.

`NO_CHANGE` is evidence-bearing: when a run has touched tracker state, it must preserve a short
diagnostic breadcrumb and restore only state that the run itself produced, so a later run can tell a
healthy yield from an untouched ticket without clobbering a third-party owner.

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
behavioural API change that must be versioned and served through a **Down-convert transform** for
older clients.

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

### Push oracle

The rule that whether a child's work has reached the remote is answered by the remote itself —
counting the commits a pushed branch carries above its base — and never by a session's lifecycle or
check state, which move when the daemon re-polls checks that already exist while the branch is
unchanged. Reading such a transition as a push puts the **Driver** on merge rails against a branch
still holding only its bootstrap commit.

The reading is sound only after remote-tracking refs are refreshed, since those are local copies that
go stale and will report no commits for work that was in fact pushed. It must also treat a missing
remote branch as zero rather than as an error: absence is precisely the "nothing pushed" case the
oracle exists to detect. Collapsing every unreadable case onto zero is safe here only because each
of them must block a merge; a caller wanting to _report_ why nothing was pushed needs the failure
itself, not the count.

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

### Implementation plan

The durable plan artifact attached to a tracker issue by the planning skill, containing the
implementation-ready guidance a later build session consumes.

An Implementation plan is a prerequisite for planning completion, not completion by itself: the
ticket is planned only once the artifact and the accompanying tracker metadata have both committed.
A ticket that still carries the planning queue signal remains retryable even if an Implementation
plan artifact already exists, because the artifact may have been written before the metadata commit
point.

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

Compare **Startup preflight**, which asks a similar question for the whole machine rather than for
one gated run.

### Startup preflight

The boss CLI's own check, run before it will draw a session interface at all, that the external
software and shell environment a session depends on are present and usable — replacing the interface
with a single blocking explanation when one is not, rather than letting the gap surface later as a
crash inside a feature that needed it. It answers for the machine the user is sitting at; a
**Capability preflight** answers for the runtime one gated run will get.

Some of its checks are bounded probes rather than lookups, because the only faithful way to ask
whether a launched agent will resolve is to ask the user's interactive login shell, which exposes the
check to however long that shell takes to start. A bounded check therefore has two outcomes that must
be kept apart — what it observed, and whether it finished observing at all — and a check that ran out
of time has to be reported as such rather than as a negative finding, because the two have unrelated
remedies and the negative finding is the more confident-sounding of the pair. Since the check blocks
the first frame, the wait a user actually experiences is the whole preflight's rather than any one
probe's, so independent probes are run concurrently rather than in sequence.

### Headless capability profile

A named, versioned contract naming the set of runtime operations an unattended run needs before it
is allowed to start — the thing a capability preflight checks against. Requesting no profile means
no gating. Each profile is an exact allowlist of real operations rather than a pattern or a set of
plausible synonyms, because a name no runtime can satisfy buys no tolerance and only risks admitting
a runtime the caller cannot actually drive.

## Account rotation

### Account

A registered provider credential (Claude or Codex) that Bossanova can run sessions under: a label plus metadata (status, health, priority, cooldown) in the daemon store, with the secret itself in the OS keyring. The user's pre-existing `~/.claude`/`~/.codex` login is the implicit system-default account 0 — never imported, never injected.

### Account home

The per-account agent home a session's agent is pointed at instead of the machine's own, so the credential it discovers there is the bound Account's rather than the ambient login's. It is assembled from the machine's shared agent home rather than copied wholesale: the credential file is account-local, while the remaining top-level entries are projected in as links back to the shared home so shared configuration stays shared and keeps tracking its source.

That projection is a reconciliation repeated before every spawn, and it does not own the directory it reconciles — the agent runs _with_ the account home as its own home and rewrites configuration and regenerates state inside it on every run. Entries the projection did not create are therefore its normal steady state rather than a corruption signal: such an entry is never deleted or replaced, and meeting one is a reason to leave that entry alone, not to abandon the rest of the projection.

### Credential injection

Realising a bound Account's credential into the environment an agent process is actually started with — assembling the Account home and handing the process the environment pointing at it. Injection is best-effort by policy: a failure does not block the spawn, it degrades it to the implicit system-default account, so the session runs normally to completion under a credential nobody selected and the bound account records no usage at all.

That outcome is indistinguishable from success at the moment it happens, and stays indistinguishable for as long as the ambient login happens to hold the same credential that was intended — so a degrade that is announced only in a log is, in practice, not announced. A failed injection is therefore recorded on the Account itself, where every surface that already lists accounts renders it.

### Account health

Whether an Account is currently usable, as distinct from whether an operator has enabled it: Rotation selects only an account that is both enabled and healthy. Several unrelated events write it — a credential the provider rejected, a confirmed suspension, a Credential injection that could not be completed — and each records a reason alongside it saying which, because the remedies differ and the health value alone does not separate them.

The writers differ in what they prove, so they differ in how their records may be withdrawn. A failure that indicts the credential itself must stand until something re-establishes the credential, and no later success on an unrelated path may clear it. A Credential injection failure is a local condition that says nothing about the credential, so it withdraws itself on the next injection that succeeds — but only its own record: a reason another writer left is preserved, and each writer's automatic clear is scoped to the reasons attributable to it, so no writer can launder another's diagnosis into a self-clearing one. A record that erased state it could not restore would not be withdrawable at all, which is why a self-clearing writer must leave everything outside its own reason untouched. Withdrawal also depends on a later attempt actually being made, and an unhealthy account is one Rotation will not normally select — so the automatic clear is a convenience, not a guarantee.

### Rotation

The daemon's automatic response to a usage-capped account: put the limited account on cooldown, select the next eligible account for the provider (active, healthy, not cooling, lowest priority first), and respawn/resume the interrupted session under it — posting an in-chat notice, and never auto-resending the interrupted prompt. On by default once extra accounts are registered, with per-repo opt-outs; `managed_accounts.enabled=false` (set via `boss settings --no-managed-accounts`; `--no-rotation` is a deprecated alias) is the global kill-switch that halts all automatic rotation, re-read live per decision, while manual `boss account switch` keeps working. Every decision is audit-logged as a rotation event (labels only, never credentials).

### Switch

The daemon operation that moves a chat from its currently bound Account to a different target: validate the target Account, stop the chat's pane, rebind it, and resume or start fresh under the new Account. A manual account change, a Rotation, and a Respawn-in-place all ultimately issue a Switch.

Validation happens before anything about the chat is touched, so a Switch can be refused — the target Account is disabled, failed, or cooling, or the chat is mid-turn — without disturbing the chat at all. That refused-before-touched distinction matters to anything that charges a budget per Switch attempt: a refusal that touched nothing must not spend the same budget as an attempt that reached the chat and then failed.

### Respawn in place

The healer that restarts a wedged agent pane under the Account it is already bound to, instead of rotating it to a different one. It exists for the case where the pane cannot authenticate but the bound Account probes healthy — a local wiring fault rather than a credential fault — so switching accounts would fix nothing and would cost a working binding. Because it is a disruptive action taken on a heuristic, it fires only after repeated healthy confirmations spaced minutes apart, and is capped per chat per window.

A respawn reuses the pane rather than clearing it, so the banner that triggered the heal is still on screen afterwards and the detector keeps reporting failure for a while. The heal therefore destroys its own accumulated confirmations on success: a still-visible banner must not be able to resurrect them, and only a fresh failure edge may open a new attempt.

Respawn in place charges its cap before issuing the Switch, so an attempt that reaches the chat and then fails still counts. When the Switch is instead refused before touching the chat, the charge is refunded — nothing was attempted. A refusal caused specifically by the bound Account being ineligible triggers a real Rotation to an eligible Account instead of a retry, since retrying the same Account can only be refused again; any other before-touch refusal is retried against its own separate, smaller budget so it cannot outlast the respawn cap's own limit.

### Auth-failed episode

One continuous stretch of a pane being unable to authenticate, as Respawn in place accounts for it — deliberately a different boundary from the momentary auth-failed marker the status surface renders. That marker is level-triggered and self-healing: it clears on the first reading that does not see a login banner, so the user-facing overlay disappears the moment a pane logs back in. But a pane stuck in an agent's own auth-retry countdown redraws itself, scrolling the banner out of the detector's view for a reading at a time, so the marker drops and re-establishes repeatedly inside a single wedge.

The healer therefore keeps its own episode, latched: a clean reading is held rather than believed, and the episode ends only once the pane has stayed clear for a grace window derived from the poll cadence and the retry cadence together. The latch is private to the healer, leaving the shared marker's instant clear untouched, and its identity is the healer's own record of accumulated confirmations — never a timestamp read back from the marker, which re-pins on exactly the flap the latch exists to absorb.

### Failover proxy

A loopback HTTP proxy the daemon runs and points every agent's provider traffic at, so model requests pass through Bossanova on their way out. It is what lets a Rotation happen without restarting the agent: the proxy inspects a response's opening frames before committing to them, so a usage-cap refusal that arrives before any content can be retried under a different Account and the replacement response returned in the failure's place — the swap is invisible to the agent process.

Because the agent talks to nothing else, the proxy's lifetime _is_ the agent's connection lifetime, which makes it the last thing a shutdown may tear down rather than the first: cutting it mid-response severs the agent's stream, and the agent reports a lost connection rather than a restart. It is bound whenever the daemon runs; turning managed accounts off withholds its address from new sessions, it does not stop the proxy existing.

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

## Daemon binary lifecycle

### Staged daemon binary

The copy of the daemon executable that the OS service manager actually launches, held at a stable per-user location rather than at the package manager's versioned install path. It exists because OS privacy grants follow an executable's _resolved real path_, so launching the installed binary directly — or a symlink to it — forfeits those grants on every package upgrade, while copying the same signed executable to one unchanging path preserves both the grant and the signing identity. Only platforms with that path-keyed grant behaviour need it; elsewhere the staged path is defined to be the installed path itself and nothing is copied.

Staging creates an indirection, and the indirection is the whole hazard: the service definition names the staged copy, never the freshly installed build, so a package upgrade leaves the staged copy behind until something re-stages it. The obligation therefore belongs to the artifact rather than to any one command — every path that hands the service definition to the loader must refresh the staged copy first, or it starts the previous build and reports success. A sibling command that re-stages unconditionally does not cover the ones that do not; it only hides them, by making the stale start look like a workflow quirk that a restart fixes. Deciding whether a re-stage is needed is a content comparison rather than a timestamp check, and it is not free: answering "already current" requires digesting the source and the staged copy in full.

### Daemon staleness

Two independent questions about a daemon that must never collapse into one verdict: whether the _staged copy_ is behind the installed build, and whether the _running process_ is behind the staged copy. A re-stage not followed by a successful restart makes the first false and the second true — the file on disk is current while the live process still executes the old bytes — so a check answering only one of them can report a fully healthy daemon that is running different code than the file it names. Each answer carries its own "known" flag, so an input that cannot be determined reports as unknown rather than as healthy.

Staleness is silent by construction: the service manager keeps something running, the socket comes up, and commands exit successfully, so the condition is indistinguishable from a healthy daemon at the call site and can persist for days. Only a change of process identity proves a restart actually replaced the running image, and a correct staleness signal written somewhere nobody reads is, in practice, no signal at all.

## Daemon shutdown

### Drain

The shutdown phase in which a component stops taking new work but keeps serving what is already in flight, until that work finishes or a budget expires. Two properties separate a real drain from a decorative one: it is ordered so that the set it waits on is both complete and closed, and it holds a budget of its own rather than sharing a deadline sized for short requests.

The ordering turns on which sense of _producer_ is in play, and the two senses point opposite ways. Where a producer's own in-flight work **is** the set — a relay waiting out streams its upstream is still writing — the drain must run _before_ that producer is torn down, or it reports an empty set it emptied itself. Where a producer instead **launches** work into the set — a poller or dispatcher spawning detached workers that outlive it — the drain must run _after_ it has stopped, or the set is still growing and the wait has no reason to terminate. One rule covers both: never drain a set that is still being added to, and never tear down what the set is made of before draining it.

A drain reports which of its two endings occurred, finished or expired, because only that distinction tells an operator whether the budget needs changing. The report has to be earned rather than assumed: a graceful-shutdown primitive that abandons its wait on expiry typically leaves the in-flight work still running, so a drain that reports work as cut must perform the cut itself, and one that reports a clean finish must not be reading an unrelated teardown error as failure.

### Shutdown ceiling

A fixed wait, imposed by a layer above the daemon, after which that layer stops being patient — the CLI polling for the socket to disappear, and the OS service manager counting down to an uncatchable kill. Exceeding a ceiling never buys a longer drain; it buys a hard kill that skips the daemon's remaining cleanup, and on the restart path can leave the machine with no daemon running at all.

Because the shutdown's legs run in sequence, it is their _sum_ that must stay under the lowest ceiling, and that makes a lengthening setting a bounded one: what an operator may configure is capped at what the ceilings can service, not at what the setting's author intended. The corollary for tests is that an invariant proved against a leg's default value proves nothing about a configured one — the sum must be checked against the largest value each leg can be made to produce.

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

### Callback

A standing, one-shot request to be told when a pull request reaches a named state — its checks
resolving, the PR merging or closing, or the PR becoming ready for review or ready to merge — so a
run waiting on that event is woken the moment it happens instead of blocking on a poll loop. Registering one is an alternative to waiting, never a
substitute for deciding: the woken run still reconciles the real state authoritatively before acting.

Callbacks are usable only where two independent things hold: the run is daemon-managed, so something
is behind the callback interface to answer, **and** the CLI that issues the registration actually
resolves here as a file this process can execute. An environment can supply the first without the
second — a scheduled one may export the managed-session marker while leaving the CLI off its search
path — so a gate keyed on the marker alone reports callbacks usable, arms a registration that cannot
run, and burns the attempt. Where callbacks are unavailable the waiting run degrades to bounded
polling, and because that fallback is legitimate behavior rather than an error, the unavailability
must carry a stated reason: nothing else in the system will ever raise it, so an unexplained degrade
is indistinguishable from normal operation and can persist indefinitely while runs keep succeeding.

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

### Review ledger

The per-run `boss-review` dispatch ledger, stored under `.git/boss-review-ledgers/` by default. It is
unrelated to the Bazel reconciliation ledger above: this one records reviewer coverage, not test-runner
coverage. Each row is a dispatch record for one discovered reviewer (`lens:<id>`,
`round:<extension>`, or `default:<capability>`) with its phase, tier, mode (`dispatched`, `inlined`,
or `unknown`), outcome (`completed`, `skipped`, `timed-out`, or `not-reached`), cause, and timing.

The important value is the seeded `not-reached` row. `boss-review` writes one before dispatching so a
killed run, skipped tier, or missing reviewer remains visible later instead of collapsing into "no
row, therefore no problem."

### Test command manifest

A generated inventory of the repository's test commands, module targets, and command-selection
guidance, used by agents and reviewers to choose appropriately scoped verification and to notice when
a branch changes the declared command surface. It is evidence about the intended verification
surface, not the runner itself: a stale manifest can mislead review even when the underlying build
target is correct, and a correct manifest cannot make a missing build-target entry execute.

### Budget regime

The exact measurement harness a pinned performance budget's numbers were taken under — which runner
executes the work, under which build flags, at what parallelism and sharding, and whether cached
results were permitted — recorded alongside the budget itself.

A budget number without its regime is not comparable to anything: the same target measured natively
and through the build facade, or at a different shard count, yields figures that differ by multiples,
so a budget re-derived from a figure taken under a different harness ratchets in the wrong direction
without anyone seeing it move. The regime is therefore part of the pinned artifact rather than
commentary on it, and a budget document that omits it is rejected rather than assumed. Changing the
regime invalidates every number under it — the budgets are re-measured, not rescaled. Where a regime
forbids cache-served results, a served result is a measurement failure rather than a fast pass,
because it reports a duration for work this run did not do.

### Stamp

A content-hash gate over a step's inputs: the step is skipped when the hash is unchanged. Used for cached lint/gen layers (e.g. `GEN_STAMP`, the cached-lint stamp) so unchanged inputs don't re-trigger expensive work.

A stamped step is a caching decision, not an enforcement point. A correctness check mounted on one inherits the skip, and it inherits it precisely for changes outside the hashed input set — so a check that reads a tree the key does not cover goes silently non-gating on exactly the change class it exists to police. The same reasoning governs a CI path filter and a Make prerequisite: whatever triggers a check must be keyed on everything the check reads, not on where it lives.

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

### Byte ratchet

A pinned bound on a published skill body's size, asserted in measured units — bytes, or lines where
line count is the property that matters — so that prose added to a core has to be argued for against
everything already there.

A ratchet may be written **one-sided**, as a ceiling, or **two-sided**, as an exact pin, and the two
are not equivalent. A ceiling catches growth and says nothing about shrinkage, so reclaimed budget is
never banked and the prose beside the number drifts out of agreement with it unobserved. An exact pin
fails in both directions: growth and shrinkage are each an event, each repinned to the measured value
in the same commit as the edit that justified it. Two-sided is the house form; a one-sided ceiling is
the exception that has to say why.

An exact pin's number is _measured_, never derived — not from a plan document, a ticket, or a previous
commit message, and never as the measurement plus a margin, which ships slack rather than recording a
fact. Where a passing pin and the prose around it disagree, the pin is the truth and the prose is what
drifted. Because a red pin does not say which of its two halves moved, its failure message is part of
the mechanism rather than decoration: it states what the number covers and what it does not, names a
remedy the artifact can actually perform, and says how to check the pin's own provenance before
assuming the artifact changed. Where that remedy refuses a class of growth as **policy** rather than
as arithmetic, it owes its own scope as well — which growth it refuses and which it would accept —
because the reader who trips a blanket refusal is usually the one case it was never aimed at, and
because a per-pin refusal is concatenated with the generic re-pin instruction rather than replacing
it, so the two contradict each other in a single message unless the narrower one says which case is
which.

Size is not a **sync** check. Where a mirror is generated from a source, generation may add bytes
unconditionally, so "larger than its source" is not evidence of drift; exact regeneration equality is.

Two kinds of pin sit alongside a ratchet and are not interchangeable. A **rolling** bound tracks the
ratchet at a documented margin, and moving it with the ratchet is its stated procedure. A **fixed
historical measurement** records a size the body actually had at a past moment, and exists to make a
later reduction provable — moving it destroys the evidence it was created to hold. The two are
indistinguishable in a diff and distinguishable only from the comment above them, so that comment is
read before either is touched. A ratchet with little headroom also shapes what may be written under
it, which is why terse spellings in a skill body are often deliberate and say so.

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

## Chat uploads

### Chat upload

A file boss materialises on disk on the user's behalf so that an agent can read it, delivered into
the conversation as a _path_ rather than as content. The agent never receives the bytes through the
chat; it opens the file itself, which is what lets an upload carry material too large or too binary
to paste.

An upload's directory is proved to be a real directory this daemon owns before anything is written
into it — not merely found at the expected name — because the enclosing location is shared with
other users on the host, and the same proof is owed by any code that later deletes inside it.
Uploads are reclaimed by a janitor on a staleness clock rather than at the end of the conversation
that named one, so a file deliberately outlives its message; the daemon's own sweep is the mechanism
and the host's temp reaper is only a backstop, which is why an upload location must be one the host
would eventually clean on its own if the daemon stopped sweeping. Deletion is conditional on
delivery being _known_ to have failed: a delivery whose outcome could not be confirmed retains the
file, because the path may already be in front of the agent.

## API compatibility

### Dated API version

A date-stamped label naming one fixed set of observable answers the orchestrator API serves, so a
client can pin the behaviour it was built against and keep receiving it after the server's behaviour
moves on. Versions are ordered by their date, with a baseline standing for "older than every recorded
change" and a current version standing for the server's present behaviour; a request that names no
version resolves to the baseline.

A new version is owed whenever an existing procedure's _observable_ answer changes — a different
status value, a different error code, a different default, a narrowed validation — even when no
schema field is added, removed, or retyped. Adding a new field or a new enum member is not on its own
a versioned change; changing what an existing caller already reads is.

### Down-convert transform

The rewrite that restores an older **Dated API version**'s answer, applied on the way out to a client
pinned below the version that changed the behaviour. Transforms are registered against the version
that introduced the change and fan out newest-to-oldest, so a client several versions back is walked
through every intervening rewrite in order.

A transform must be discriminated as narrowly as the behaviour it compensates for. Matching on the
procedure alone also rewrites answers that procedure was already giving for other reasons, which
turns the compatibility layer into a source of the very regression it exists to prevent — and that
regression is invisible, because the pre-existing answer is the one no test was watching. On the
error path the discriminator is an in-process marker attached where the new behaviour is produced,
and it does not survive serialization; an assertion that a transform fired is therefore a **Vacuous
gate** unless it is made on the producing side rather than over an error a client reconstructed from
the wire.

## Review and verification

### Dispatch record

A single row in a durable review ledger describing the fate of one planned reviewer invocation. It is
not a finding and it is not the human decisions ledger: it answers whether the reviewer was reached,
how it was reached, and whether it produced a readable envelope. Report confidence and diagnostics
consume dispatch records so a skipped or timed-out reviewer lowers confidence even when the remaining
reviewers found no code issue.

### Vacuous gate

A check that passes on the very values it exists to reject, so its verdict carries no information
while still reading as assurance. It is worse than an absent gate rather than equivalent to one: an
absent gate leaves a risk everyone downstream still treats as unchecked, while a vacuous one converts
that risk into a false assurance every consumer spends.

A gate can become vacuous at any of six layers, and each has been observed independently: its
**scan surface**, when the region it reads does not coincide with the corpus its criterion
quantifies over — narrower, so a defect that is really there is never seen, or wider than the
artifact it certifies, so a subject that artifact never carried is counted as shown;
its parser, when it splits on a delimiter the operand may legally contain; its classifier, when the
written form of a marker is not the form the test recognises, so the item silently leaves the
contract with no detector anywhere; its **operand**, when the value it compares merely constrains
the fact rather than being it — a declared version range, a configured default, a documented intent
— so two subjects satisfy the comparison identically and still resolve to different behaviour; its own regression test, when that test borrows the definition
it was supposed to check independently — normalising the very bytes it exists to pin, or measuring a
bound in the same unit the guarded code counts in, so it can only ever confirm that the code agrees
with itself; and its input discovery, when the set of subjects it scans comes back empty and that
emptiness is reported as a pass rather than as an inability to evaluate. The failure is silent by
construction, because a gate's job is to be quiet when nothing is wrong.

Discovery can come back empty without the discovery expression being wrong. Where a check reads its
subjects off disk rather than carrying them, the corpus is assembled by whichever runner executes
the check, and runners differ both in what they place within reach and in where they start looking.
One unchanged check is then sound under the runner an author invokes by hand and empty under the
runner that gates the merge — so which runner produced a green is part of the verdict, and a green
from the convenient one is not evidence about the deciding one.

The first and last layers are the two a **Falsification** usually cannot reach, and so the ones that
most need checking by hand: the other, mis-answering layers each mis-answer a value that arrives at
the check, while a reach gap means no value ever arrives, and no adversarial input you can construct will demonstrate
it. The widened surface is the exception that fixes the rule's shape — there the offending value
does arrive and the check wrongly accepts it, so one constructed input settles it. The operand layer
is settled the same way, by a pair of subjects that agree on the declaration and disagree on the
resolution. Direction decides
the discipline: a narrowed surface is established by measurement, a widened one by construction. A
gate therefore owes a floor on how many subjects it found, asserted inside the artifact that
actually blocks a merge rather than only in its tests, and measured under the runner that does the
blocking. Where that runner assembles the corpus, satisfying the floor is a two-artifact obligation:
the assertion inside the check, and the staging declaration that puts the subjects within its reach
— and the second half lives in a build file no one reading the check will open. Its cheapest tell
is a single check that bails loudly on one missing input while returning an empty success on another: two opposite policies
for one class of missing input means the quiet branch was never considered.

A further way is not a layer inside the check at all: the check is correct, non-vacuous by every test
it has, and simply is not **bound** on the path where the defect occurs. It belongs with the reach
gaps rather than the mis-answers — no value arrives, so no adversarial input demonstrates it — but it
hides one step further out, because the gate's own tests are green and its call sites are not where
anyone looking at a gate looks. It is established by census rather than by construction: enumerate
the entry points into the guarded operation and count how many build the check, rather than
confirming the check is built somewhere. Its cheapest tell is a rationale written at an unbound site
arguing that the check would refuse nothing there — a reasoned-through asymmetry is stronger evidence
of a hole than an unexplained one, because someone saw its shape and talked themselves past it.

Where the set of subjects is kept by hand — a list the check ranges over, maintained in step with the
declarations it mirrors — that list is itself a coverage claim, and it decays. A subject whose author
forgot the second edit is indistinguishable from one deliberately excluded, and it stays invisible
because the subject's own assertions still run and still pass; only the check over the subject is
missing. The structural remedy is to make enrolment a side effect of declaring the subject, so an
unenrolled subject cannot be written, and to add a gate that reds on a declaration which bypassed the
registration path. A floor on the count found is the weaker cousin: it catches a set that came back
empty, not a non-empty set that is the wrong set.

All six are properties of the check. The mirror case is a property of the **defect**: a check that is
sound at every one of these layers, answering correctly the question it was built to answer, over a
corpus that does contain the damaged artifact — and still green, because the defect does not violate
the property that check tests. That is a **Laundered defect**, not a vacuous gate, and the two want
opposite responses: one is repaired, the other needs a new detector.

### Laundered defect

A defect whose two independent detectors are destroyed by one mechanism: it leaves the artifact
syntactically valid, so the automated check guarding that file set answers its own question correctly
and stays green, and a formatter then normalises the wreckage into a shape that reads as an ordinary
reformat, so the human reading the diff sees tidy formatting rather than damage. Both detectors
report health, both report it truthfully, and their agreement carries no information about the
property that was actually violated. The name is used here in the _evidence-destroying_ sense — the
trace of the defect is washed out downstream — and not in the sense, used elsewhere in
`docs/solutions/`, of a broken artifact being passed through a gate that should have refused it.

It is not a **Vacuous gate**: the incumbent check is hollow at no layer, and no adversarial input
would expose it, because the property the defect violates is not the property that check tests. The
distinction decides what to build. A vacuous gate is repaired; a laundered defect needs a _new_
detector reasoning about the violated property directly, and the argument for building one cannot be
made from the incumbent's green runs. It is made by contrast: reintroduce the defect and show the
incumbent passing it while the candidate refuses it — a **Falsification** run against two detectors
at once, where the gap between them is the finding rather than either verdict alone. That contrast is
also the only thing that distinguishes a needed gate from a redundant one, which no amount of testing
the new gate in isolation can settle.

The formatter is the half that makes review unavailable, so the tell is a defect class whose damaged
form is _stable under formatting_: wherever a normaliser sits between the author and the reviewer,
review has stopped being a backstop for that class whether or not anyone has noticed. The corollary
runs the other way too — a formatter that rewrites the damaged shape can equally rewrite it past the
new detector, so a lexical gate is only as good as its position in the format-then-lint ordering, and
whether that ordering is enforced by a gate or merely by convention is worth establishing explicitly.

### Unsatisfiable criterion

An acceptance criterion no artifact can ever evidence, so ticking it reports something other than
what was verified. Two causes produce it, and both are properties of the criterion rather than gaps
in the work: the signal it measures does not exist yet, because the distinction it asks to split on
is precisely the distinction the change introduces and no historical data was ever told about it;
or the change itself closes the only window in which the measurement could be taken, so merging is
what makes the criterion permanently unmeetable.

It is not a **Vacuous gate**. A vacuous gate answers a satisfiable criterion wrongly — the criterion
is true on the day it is ticked and the check simply does not establish it. An unsatisfiable
criterion could never be true at all, so no repair to any gate reaches it; the defect is upstream, in
the prose, and the repair is to rewrite the criterion. Both end in a tick that carries no
information, which is why they are easy to confuse and why the distinction decides what to fix.

The tell is a criterion whose satisfaction depends on ordering, on an operator action, or on a
wall-clock window — none of which a diff can express. The remedy is to split it: what the branch owes
becomes an artifact a reviewer can open, and the remainder moves into a runbook addressed to the
party who can actually discharge it, with the criterion stating outright that merging does not close
it. A measurement obligation honestly relocated is more enforceable than a merge gate that was never
meetable, and the sentence about merging not discharging it is the clause that stops the obligation
from being silently reclassified as done. The same three questions — does the property exist, does
anything destroy it, what artifact enforces it — apply unchanged to any prose asserting a guarantee;
an unenforced rule labelled as discipline is durable, while a structural claim the code does not
provide is worse than silence.

### Falsification

A test written to prove that a specific guard is load-bearing: it constructs the exact value the
guard exists to reject, asserts the guard refuses that value for the stated reason rather than merely
failing, and is confirmed by mutating the guarded branch away and watching the test die.
Falsifications are named and numbered alongside the guard they defend, and stand as the durable
evidence that a gate is not vacuous.

Asserting only that a good input still passes is not a falsification — the refusing direction is the
whole record. A falsification that would stay green with its guard deleted is itself vacuous, one
level up, which is why the mutation step is part of the definition rather than an optional check.

A mutation that matched nothing is not a falsification either, and is the harder case to notice. A
substitution that alters no bytes still exits successfully and leaves the suite green, and that
green is byte-identical to the guard being vacuous — the very reading the probe was run to
produce. So a falsification owes a changed-target check: evidence that the mutation actually
altered the artifact, established before its exit code is read at all. The hazard is sharpest
where the mutation is matched against text, since a pattern can fail to match for reasons that
have nothing to do with the guard — see **Prose pin**.

A guard that discovers its own corpus needs its matching logic extracted as a pure predicate,
separate from the walk, or it offers no surface a falsification can reach: with the logic inlined,
the only assertion available is that today's corpus is clean, and a clean corpus is indistinguishable
from a predicate that has stopped matching anything. The extracted predicate is then pinned by a
table of both directions — values it must flag and values it must clear — because a one-off probe
against a mutated build shows the guard discriminated once, while the table is what makes it keep
discriminating. Where any narrowing step is being pinned — a carve-out, a section scope, a filter, an
exclusion — the row must be admissible to the un-narrowed matcher and must use a value the rest of
the predicate would not have excused anyway, or it passes for the wrong reason and the narrowing
goes unpinned. A fixture the matcher would have rejected with or without the narrowing tests
nothing, and the only way to tell the two apart is to mutate the narrowing away and watch the row
red. Where the live corpus holds no real counterexample — the narrowing being prospective — the
fixture must synthesise an admissible one rather than treat the absence as proof the narrowing is
unnecessary.

A falsification proves the guard's **rule**, and a **Scan surface** only where that surface is too
wide: it hands the value straight to the check, so it can show a check accepting what it should
refuse, but never that the walker feeding the check fails to reach where the value is written. A
guard can hold every falsification and still enforce nothing over most of its corpus.

### Prose pin

A gate assertion whose subject is a specific sentence or phrase of human-readable text in a tracked
file — that a document still says a particular thing — as opposed to an assertion over a value, a
structure, or the shape of a command. Its defining hazard is that the text it quotes has unstable
line breaks: the wording is the contract and the wrapping is not, yet a pattern written with literal
spaces binds both, and the agent that moves a line is usually a later author or agent rewrapping a
neighbouring paragraph rather than any formatter.

Its rule is owed to two consumers that fail in opposite directions, and documenting only one of them
leaves the worse half uncovered. The pin itself fails **loud and false**: a rewrap that changes no
word reds an assertion naming no defect. The **Falsification** aimed at the same phrase fails
**silent and inverted**: it matches nothing, changes nothing, exits successfully, and its green is
indistinguishable from the finding it exists to produce — that the pin is vacuous. Only the loud half
is mechanically enforceable, because a pin is a committed literal some gate can read while a
falsification pattern is typically a one-off command no gate ever sees; where that asymmetry holds it
belongs in the written rule, or a reader infers coverage that does not exist.

The remedy is a whitespace-tolerant gap, applied by subject rather than uniformly. A tolerant gap
also matches a line break, so spelling every gap tolerantly is a **widening** rather than a
normalisation: where the subject is prose an author may rewrap, absorbing the reflow is the whole
point, but where it is a command, a fenced block, or a claim that two words share a line, the exact
gap is the contract and tolerance quietly accepts text that could never occur or that the assertion
existed to forbid. A negative assertion inverts the trade, since there the wider form forbids a
superset and is the stronger one. A gate enforcing the rule therefore needs a marked, greppable
exception for the deliberate exact gap, and the audit that reads those markers must match them the
same way the gate does, or it reports a complete census that is not one.

### Self-inflicted finding

A review finding whose subject is text an earlier fix round of the same review wrote — the defect was
absent from the work under review and was introduced by the act of remediating it. It is distinct
from an ordinary regression because nothing it breaks is executable: its surface is almost always a
claim no gate reads, so the only detector is another reader, and the round that produced it has
already been recorded as closing a finding.

Its rate is what makes it a planning concern rather than a curiosity. Wherever the deliverable is the
accuracy of claims, a fix round is itself an authoring act and reintroduces the class at roughly the
rate the original authoring did, so remediation converges as a series rather than terminating in one
pass. Any stopping rule keyed on the named finding being gone, or on a fixed round count, therefore
halts somewhere inside that series with residue still in the tree while every gate reports green; the
terminating condition is a round over the current text that yields nothing new. Two structural
consequences follow. The confirming pass must re-read the replacement text rather than the absence of
the original. And it must run in a context that did not write that text, because the reasoning which
produced a claim is still resident in the context that produced it and will assent to it — a second
persona inside one context is not a second reader. The cost of re-verification is set by what a claim
is bound to: an identifier or a quoted literal can be re-derived by search, and a claim some gate
reads is at least a **Prose pin** that fails loudly, whereas a line number or a pronoun decays with
no signal at all.

### Relational guard

A test whose subject is a required relationship between two constants rather than the value of either
one — that one budget stays under another, that a ceiling exceeds the clamp it exempts from — written
because the relationship spans a boundary the type system cannot express. The usual cause is a
one-way module dependency: the two values sit on opposite sides of an import edge that exists in only
one direction, so neither can be derived from the other and the duplication is forced rather than
careless.

It differs from a value test in what it reads and in what it says. It reads both operands from the
production sources that define them — through the real builder or accessor, not a literal restated in
the test — so that either side moving is what changes the outcome; a relational guard that hard-codes
the numbers it compares is a **Vacuous gate** one level up, able only to confirm that the test agrees
with itself. It must also read them at the layer that determines behaviour rather than one that
merely constrains it: where a declaration and a resolution both exist — a declared version range and
the version a lockfile pins, a configured default and the value a run resolves — only the resolution
is the fact, and two sides declaring the same constraint can still resolve differently while the
guard stays green. Where the fact lives in a file that names the same operand for other reasons, the
parse is scoped to the block that is authoritative for the question. It also owes a failure message naming both constants and the file each lives in, because
whoever trips it usually knows one of the two and has no reason to suspect the other exists, and owes
the remediation in both directions since either constant may be the one that should move.

Two further obligations keep it honest. It must sanity-check its operands before comparing them, or a
builder that quietly stops returning the value it once returned turns the comparison into a trivially
satisfied assertion that passes forever. And it must separate the invariant it enforces from any
policy margin it also encodes, saying which is which, so that a drift meaning "these two need
re-deciding together" is not read as a correctness break — and, for the same reason, record the cases
it cannot see at all, chiefly operator-configurable values that replace a compiled default at runtime,
where someone about to trust it will read them.

Recording that blind spot makes it findable; it is not coverage. Where the operand is a setting an
operator may raise without a ceiling, the relation can hold at the default and invert past a
threshold named in no comment, no test and no setting reference, so a guard reading the compiled
default is green across the whole broken range by construction. Closing it has two halves and
neither works alone: the constant on the other side stops being a compiled number and becomes a
function of the setting, because a fixed value cannot stay in relation to one that moves; and the
guard ranges over configured values chosen to straddle the threshold rather than over the default.
Two cautions attach to the second half. The quantity compared must be what the guarded step actually
receives rather than the whole budget it is carved from, or the guard stays green while that step is
already being shortened. And rows over configured values may be algebraically identical against a
correct derivation, which makes them a regression tripwire rather than independent checks — their
value is established only by reverting the derivation and confirming that the rows past the
threshold, and only those, go red.

### Scan surface

The region of an artifact a gate actually reads, as distinct from the corpus its acceptance criterion
quantifies over. A gate that walks fenced code blocks has fenced blocks as its scan surface even when
the criterion it discharges says "anywhere under this root"; the difference between those two is
enforcement the project believes it has and does not.

A surface can miss its corpus in either direction, and the two fail differently. Narrower is the
common case: the gate exits clean over a region that excludes where the defect is written, and the
loss is a defect never seen. Wider is the case where the gate reads beyond the artifact it
certifies — where the thing being gated is one artifact and the thing actually read is a larger
capture taken alongside it — and the loss is the opposite: a pass asserting that an artifact showed
a subject it never carried. Whenever a gate reads a companion artifact rather than the one it
certifies, the two are collected by different code and their scopes drift silently; the scope of
what is read must be derived from the scope of what is certified, not from whatever handle was
convenient to grab.

A scan surface is inherited, not chosen: a new rule added to an existing scanner gets the surface
that was picked for the original rule, which was picked for a different defect. A **narrowed** gap
of that kind is invisible from both ends — the gate exits clean, and the criterion is ticked — so it
is established by measurement rather than inspection: run the gate over a corpus known to contain
the defect and compare its finding count against a count obtained some other way. A widened one is
not established that way, because it hides in a pass rather than in a silence; construct the subject
the artifact does not carry and watch the gate accept it. Where a reach gap is real and a
second parser is not worth building, the standard cover is a whole-file assertion that imports the
rule's own pattern rather than restating it, and that pins how many files it read before asserting it
found nothing.

Width is not the only way a surface can miss: it can also be too **coarse**, reading at a larger unit
than the one its invariant is stated over. A guard whose invariant is per-fixture but whose surface is
per-file passes as soon as one satisfying token appears anywhere in the file, so a genuinely defective
fixture is immunised by an unrelated correct one hundreds of lines away — the surface is neither
narrow nor wide, and every falsification still passes, because a value handed straight to the check is
already at the right granularity. Coarseness is sometimes the right trade: refining the unit means
guessing at the distance between a construct and the code that protects it, and where the prevailing
idiom protects many constructs from one shared helper, a finer window rejects correct code. What the
trade is not is invisible. A guard that reads at the wrong granularity records that limit in the
artifact itself, in the terms a green run may be read in — every _file_ contains a disable, never
every fixture — because the reader who over-trusts it is the one who added the next fixture.

### Silently-empty stub

A member of a test double that answers with a zero-valued success no fixture stands behind, handing
its caller a plausible "nothing here" — an empty list, a clean report — so the test that drove it
reads as covered while exercising nothing. It is the test-double form of a **Vacuous gate**, and
worse than an absent member for the same reason: an unimplemented member leaves a gap everyone
downstream still treats as untested, while a silently-empty one converts that gap into coverage the
team believes it has.

Every member of a test double therefore belongs to one of exactly two categories, and a third is
forbidden: **fixture-backed** — state a test can both stage through a seeder and read back, where a
call log is the fixture whenever the response itself is empty by design — or **unimplemented**,
refusing loudly. Which one a member gets is decided by whether a real consumer exists today, never by
uniformity across the surface: a fixture nothing reads is itself a silent gap, drifting out of step
with the real service while still looking like coverage. A member is promoted from unimplemented to
fixture-backed when a consumer appears, never softened back the other way, and the invariant is held
by a test that asserts the refusal — stated in a comment alone it is documentation, and documentation
does not fail. Being a real process rather than a mock is no defence: a double that genuinely runs and
genuinely fails is still silently empty when it never emits the field the code path under test reads.

### Diagnostic conflation

A single rendered value standing for two opposite causes — an observation that was taken and came
back empty, and an observation that was never successfully taken at all — so the diagnostic carrying
it distinguishes neither, while still reading as a report of what was seen. It is the reporting-side
member of the family that includes a **Vacuous gate** and a **Silently-empty stub**: a signal that
looks like information and carries none, and costs more than saying nothing because the reader spends
it.

The conflation is created by an accumulator whose zero value is also a legitimate observed value, and
it is not repaired by testing that value for emptiness — a successful observation may legitimately be
empty, which is exactly the case that must stay distinguishable, so keying on emptiness rebuilds the
conflation one notch over. The only honest discriminator is whether an observation ever succeeded,
which means the act of observing must be accounted for separately from the content observed: how many
attempts succeeded, how many failed, and what the failing tool said in its own words. Where the
failure clause names a cause, it is owed whenever any attempt failed rather than only when all did,
since the common shape is a subject that is observable and then is not. A diagnostic also owes its
ordering to whatever consumes it: where a surface truncates, the accounting belongs ahead of any
multi-line payload, because a field rendered below the cut does not reach the person deciding. The
same family reaches past a zero value: where the two opposite causes share a _message_ rather than a
value — see **Transient classification** — the honest discriminator is again something outside the
value that was rendered.

### Finding provenance

The source location a gate carries alongside each value it rejects — which member of its scanned
corpus produced that value, and where inside it — as distinct from the value itself. A report naming
only the offending value hands its reader a search rather than an answer: the value says what is
wrong, the provenance says where to go, and only the scan step holds both, so a collector that
flattens matches into bare values destroys what nothing downstream can recover. It is the
constructive counterpart to a **Diagnostic conflation** — not a signal that carries no information,
but one that carries half of what it already had.

Provenance is cheap to keep and expensive to reconstruct, and the asymmetry is what makes dropping it
a defect rather than a terseness preference: the guard pays once to record where it looked, while
every reader of every future failure pays again to re-derive it by hand — and re-derivation is often
ambiguous, because the same offending value may legitimately appear in several members of the corpus
and only one of them is the offender. The cost compounds where the reader is an automated agent
rather than the author of the change, since neither has the context that would narrow the search.

Carrying provenance creates two further obligations. A report of several findings must be sorted on
the structured fields — corpus member, then position compared as a number, then value — _before_ the
findings are rendered, because sorting the rendered strings compares an embedded position as text and
places the tenth line ahead of the second, and a report whose order looks wrong makes a reader
distrust the locations themselves. And the rendered text must be what the guard's own test asserts:
a test that pins only the boolean verdict reports every possible regression identically, so the
message that provenance exists to improve is the one part of the guard left unpinned.

### Transient classification

A fixed, argued set of failure signatures matched against a tool's own error output, whose verdict
decides whether an operation is retried and which status a caller is handed to act on. It is
distinct from an ordinary error check because its output is an instruction rather than a
description: widening the match widens the action, so a signature that also covers a permanent
fault instructs a retry loop to spend its budget and instructs the caller to re-issue a request
that will fail identically forever.

A classification is consulted only after the caller's own liveness, never before it. A run killed
by a deadline or a cancellation still carries whatever the tool had already printed, and a tool
that reports incrementally as it walks a set will have printed a genuine transient line for an item
it really was contending on — so the text alone cannot separate "the work failed transiently" from
"whoever asked stopped waiting", and asking it first spends a retry budget against a dead caller
and then answers _try again_ to someone who already gave up. One message prefix commonly spans both
retryable and permanent causes, so a signature set generally needs a subtraction list of terminal
reasons beside it, and every entry on either list owes a stated argument for why it belongs; an
unargued signature cannot be reviewed, narrowed, or safely inherited by the next reader. Where the
retryable and the permanent readings of one message differ only in wording, decide by which reading
the concurrency actually produces, not by which one the wording evokes. A classification is owned
by the layer whose state it describes, which is what keeps nested retry ladders from retrying each
other's failures — but that disjointness bounds recursion only, never the product of their attempt
counts, which nothing but the caller's own deadline bounds.

## Proof capture

### Proof run

A pass over a pull request's diff that captures evidence the change works, uploads it, and attaches
it to the pull request as a comment. It is demonstration, not verification: a proof run shows the
change behaving, while tests and gates decide whether it is correct.

A proof run has read-only siblings that answer questions about it without performing it — what would
be captured for this diff, and whether this host can capture a given surface at all. Those siblings
share the run's option vocabulary but not its effects, which is why an option a run reads may be
merely tolerated by a sibling rather than consumed by it.

### Proof surface

The kind of interface a change is demonstrated through — a terminal UI, a web UI, a scripted capture
of a known route — and therefore which capture path a proof run takes. A surface is a property of
what must be shown, not of where the code lives: a diff that touches only backend code still proves a
web surface when the change's own required-proof notes name a user-visible interface.

### Surface plan

The authoritative answer, for one diff, to which proof surfaces must be captured and in what order.
It is a set rather than a choice, so it may name several surfaces — and it may legitimately be
**empty**, which is a decision rather than a missing answer: a diff with nothing demonstrable routes
the run to an honest no-surface note instead of capturing something unrelated.

A single-select classification of the same diff also exists, for deciding whether one host can
capture one surface. That answer carries a fallthrough default, so it can name a surface the plan
deliberately excluded. Where the two disagree the surface plan wins, and only the surface plan is
safe to publish — printing the single-select answer beside it offers a consumer two answers to one
question with no signal about which is authoritative.

## Flagged ambiguities

- "Capability" carries two unrelated senses. A **Tracker capability** is an adapter behavior a skill
  calls as code; the sense in **Capability preflight** and **Headless capability profile** is an
  operation a coding-agent runtime must support before a gated run may start. Neither gates the
  other, and a conformant tracker adapter implies nothing about an agent runtime's profile.
- "Preflight" carries two distinct senses answering for different scopes, and should not appear
  unqualified. A **Startup preflight** is the boss CLI checking a machine's environment before it
  will draw a session interface; a **Capability preflight** is an agent runner checking that one
  gated run's runtime can do that run's work. The first blocks a person's interface, the second
  fails a single run closed, and neither implies the other.
- "Gate" carries several unrelated senses and should never appear unqualified. A **Cron gate** is a
  command a scheduled job runs to decide whether it has work; a **Repo automation flag** gates what
  the daemon may do to a repo; a declaration gate (see **Tracker adapter**) admits an adapter; a
  **Stamp** gates a build step on an input hash; a **Delivery gate** decides whether a pane is safe
  to type into. Only the cron sense has a recorded **Gate outcome**.
- "Rate limited" had been used for both a **Limited** account, which has exhausted its own usage
  cap, and a **Probe throttle**, which is the usage endpoint refusing our polling rate — these are
  distinct, and only the former justifies a **Cooldown**.
- "The agent is prompting" had been used for both halves of the **Delivery gate** — a pane that
  blocks input and an agent that is asking a human something — and the two had at one point been
  documented as though the first were a special case of the second. A **Boot interstitial** is the
  counterexample: it blocks input and asks nothing. They are independent predicates, and a caller
  must name which one it means.
