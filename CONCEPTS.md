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

## Web reconnect

### Poll status

Where a polled resource in the web UI sits in its connection lifecycle: **live** — the last completed
attempt answered, or none has run yet; **reconnecting** — an attempt failed transiently and a bounded
retry ladder is armed; **exhausted** — the ladder gave up on the current failure.

The state carries a display contract, and it is the reason the three are named rather than collapsed
into an error flag. While reconnecting, the last successful data stays on screen under a
non-destructive indicator and no error is set — a transient blip must never blank a view. An error is
set when the ladder gives up — but the slot does not name its writer. A poll that also carries a
secondary request, as the accounts poll carries the Usage probe, has one dispatch path for any give-up,
and an unsuperseded probe failure can drive the poll into exhausted without the read itself having
given up. So "an error is set" (or "status is exhausted") is not the same fact as "the read gave up",
and only the read's own give-up entitles a view replacement. A resource returns to live on the next
successful attempt, from either state.

### Connection indicator

The single answer a page owes the viewer about "the connection", folded from every polled source that page mounts — one indicator however many pollers sit behind it, because two competing reports of what a person experiences as one connection is noise.

Being one surface over several sources is what sets its rules. Every field it displays and every action it offers must fold across all of them; a partial fold under-reports silently rather than failing loudly, and the field that gets folded tends to be whichever one some unrelated contract already carried. Connection health therefore travels on its own seam — routing it through a contract about something else limits the indicator to whatever that contract chose to re-export. The fold is a set of per-field decisions rather than one rule: the transient reconnecting state is a disjunction; the give-up message prefers the page's primary subject, with a secondary source's message surfacing once the primary recovers; and the staleness age is the oldest contributor's, absent entirely when any source has never answered, since an age that cannot be claimed for everything on screen is worse than no age at all. Per source, reconnecting and a give-up are mutually exclusive states; folded they are not, because they can arrive from different sources, so both are told at once.

The fold governs the annotation only. Whole-page takeover — the cold-start, empty, and error states entitled to replace the view — stays keyed on the page's primary read alone: a secondary source that cannot load is not grounds to take the page away.

### Reconnect episode

One continuous stretch of a page being unable to reach a source, from the moment the connection is first reported lost until it is reported healthy again — the unit that transient connection facts are stated about and announcements are counted against.

An episode's identity belongs to the surface that displays it rather than to any producer value: it opens on the rising edge of the lost-connection signal and is discarded on the falling edge, and a surface that outlives an episode must perform that discard or the next one inherits the previous episode's start and reports an age that never happened. Anything the viewer is told about elapsed time is measured from the captured start rather than from a running clock, so the sentence is a fixed fact about the episode instead of an approximation that drifts silently between unrelated renders. Where an indicator folds several sources, the episode is their union: it opens when the first source drops and closes only when the last recovers, so a handover between sources reads as one long episode rather than two short ones, and the elapsed reading clamps rather than running backwards — understating the age in preference to inventing one.

### Give-up notice

The notice a page raises for a failure it has stopped working on by itself: the failure message plus
the "try again" action, presented assertively so it is read out at the moment it appears rather than
whenever the reader next arrives at it. What raises it is a message being there to show — the
**folded error** — not the poll reaching exhausted. On a page whose poll also carries a secondary
request, the notice can stand on that request's one-shot give-up alone, with the read itself still
live and the poll never exhausted; see **Read give-up**.

It is the one place where the connection story stops being an annotation and becomes an interruption,
and that makes its rules unusually strict. It is mounted already carrying its text, so it must stay
assertive on every page — a polite region is generally not read out for the content it is born with,
and a notice nobody hears is worse than no notice. Its retry control has to survive its own press:
the control must remain focusable and present to assistive technology while the attempt is in flight,
and the attempt must not clear the state that renders the notice until it settles, or the person who
pressed it loses both their place and the affordance. And because such a region is read out when its
text _changes_, not when it re-renders, a retry that fails identically says nothing at all unless the
notice carries something that differs per settled attempt — the case the control exists for is
precisely the one that would otherwise be silent.

### Loadedness

A polled resource's report that **the most recent completed attempt succeeded**. Distinct from "no
error is currently being displayed", which is also true throughout reconnecting, and distinct from
"the result is non-empty", since an empty result can be the truth.

Only a committed attempt counts: an attempt that answered but was superseded before it could
commit leaves loadedness exactly as it was, so "the last attempt I started finished" and "the
resource is loaded" are different statements.

Loadedness is reported by the resource, never derived by a consumer from the absence of an error.
That rule exists because it is the gate on destructive follow-up work — pruning a persisted
selection that no longer matches the fetched options — and a derived flag silently changes meaning at
every consumer the moment the producer gains a new state.

### Read give-up

The fact that **the polled read itself** exhausted its ladder, as distinct from an error merely being
present in the poll's error slot. The two coincide only while the slot has a single writer.

It is the discriminator a destructive branch must ask for. Because a secondary request commits its
failure by the same path a read's give-up takes, a gate written as "an error is present" is satisfied
by a recoverable background failure and will hand it a primary failure's UI — worst on a cold start,
where there is no snapshot behind the error to fall back on. A consumer subtracts the secondary
owner's message rather than testing the slot for emptiness, and latches the read's own message, since
a later failure overwrites the single slot and destroys the string rather than mislabelling it.

### Folded error

A display value that prefers one failure over another, such as the accounts page's
`probeError ?? error`. Folding is legitimate only on a surface that deliberately describes both — an
inline notice that means "something is wrong here" — and never as the value a branch renders when
that branch gated on one arm alone: the panel's existence and its text then describe different
failures. The condition and the thing it renders must read the same slot.

### Supersession

A polled resource's rule that **only the newest attempt may commit**: every attempt takes the next
value of a monotonic request id, and an attempt whose id is no longer the current one is discarded
on arrival, whether it succeeded or failed.

The rule belongs to the resource, and a caller cannot reconstruct it by counting its own calls. Poll
ticks, the resume signal, an online nudge, and any other caller all start attempts and all advance
the id, so a caller's private count is a strict subset of the real one and every attempt outside
that subset is a supersession the caller cannot see. Discarding is silent by design — from the
caller's side a discarded attempt is indistinguishable from a completed one — so a caller that needs
to know must ask the resource for the current request id rather than infer it. That id is published
as a **call-time read**, never as a per-render value: its only use is comparing before and after an
awaited call, and a value sampled once per render cannot bracket one. The read is taken _after_ the
attempt starts, since starting an attempt is what claims the new id.

### Resume signal

The application's own single "the user came back" event, raised once per genuine restore and fanned
out to every subscriber from one place.

It is deliberately not a raw browser visibility event. It is **delayed** rather than immediate,
because a request issued the instant a page becomes visible can hang on a mobile engine that has not
finished re-establishing its networking — the delay is load-bearing, not politeness. It is
**deduplicated**, because one user-visible restore can emit two distinct browser signals. It is
**app-level**, because a page with several pollers would otherwise refetch once per poller on a tab
that has only just woken. A page going hidden ends the current resume lifecycle: any armed fan-out is
dropped and the dedupe window is cleared, so a re-backgrounded tab does not resume in the background
and a later genuine restore is never swallowed.

### Session expiry

The identity provider's own report that the signed-in session can no longer be renewed — a push
signal from the auth client, not a verdict inferred from failing requests.

It is deliberately distinct from a poll status of exhausted. Exhausted means retrying did not work
this time; session expiry means retrying cannot work at all, because the credential the retries would
use is dead. The two want opposite affordances: exhausted offers "try again", expiry offers "sign
in".

The application's response to it is asymmetric by design, and the asymmetry is the concept rather
than an inconsistency to tidy away. An anonymous visitor arriving cold is redirected straight to the
identity provider, because there is nothing on screen to lose. A session that expires **mid-session**
gets an explicit, actionable state instead. What that state preserves is not the work on screen —
whatever the person was holding, a half-filled wizard or a chat they were reading, is unmounted by
the state as surely as a bounce would have discarded it — but the **URL and the timing**: they are
told what happened, and they leave for the identity provider when they choose rather than being
navigated away mid-keystroke by something they never asked for. The signal is held in memory only: a reload re-runs the auth client's own initialization, which
either recovers the session or lands on the cold-load redirect, so a persisted flag could only
outlive the condition it describes.

### Login organization hint

The organization a previous signed-in session was in, remembered so the next sign-in redirect can name it and the identity provider does not ask a returning user to choose an organization the app already knows.

It is a hint and never an authority, and the rules that follow from that are the concept. It is **consumed on use**: the anonymous cold-load redirect reads it, forgets it, and only then redirects, because that hop is the one place a stale hint can fail and also the one place the failure is unobservable — the identity provider rejects a revoked or deleted organization on its own hosted page and returns the browser with nothing the app can react to. A rejected hint therefore costs one failed attempt rather than a permanent lock-out, and a successful sign-in immediately rewrites it from the live session claim. It is written only from a session that is genuinely the signer's own: an impersonation session's organization is the impersonated user's, not the admin's, and is never persisted. It is cleared on **session expiry** and on sign-out. It holds the identity provider's own organization identifier, which is a different value from the local mirror's identifier for the same organization; the mid-session re-authentication path needs no hint at all, because the expiring session's claim is still in hand.

## Web announcement contracts

### Inert control

A control that is refusing activation because it is busy with the action it was just given, while
staying focusable and present in the accessibility tree — distinct from a control that is disabled
because it is not usable yet.

The distinction is not cosmetic: a natively disabled control cannot hold focus, so a control that
disables itself in its own handler throws the pressing user back to the top of the document and
vanishes from assistive technology at the moment it is doing the work being announced. A control made
busy by its own press must therefore be inert rather than disabled. Inertness is only _declared_, so
the refusal has to be implemented as well — every event that would otherwise activate the control is
swallowed, while focus movement is deliberately left alone, since trapping it would invert the point.
The two states look identical to a sighted user by sharing one appearance rule rather than
duplicating it. When both conditions hold at once the not-usable-yet one wins and keeps native
behaviour; that precedence is not licence to derive the disabled condition from the in-flight flag,
which reinstates the focus loss in full.

### Status role ownership

The rule that a page exposes exactly one region designated as its transient-status announcer, so a
shared indicator mounted onto a page that already owns such a region yields the designation rather
than adding a second one.

A permanently mounted second status region leaves "is a notice up?" answered yes forever, which is why
the designation is treated as owned rather than claimed. Yielding costs the reader nothing: the
region still announces politely and atomically, which is exactly what the designation means, so only
the label moves. Ownership applies to the transient status annotation alone and never to the give-up
notice, which stays assertive on every page regardless of what else the page owns.

### Announcement budget

The number of times a live region is allowed to speak during one episode — the scarce resource that governs how transient status text is derived, in place of the usual assumption that re-rendering is cheap.

Every mutation of an atomic region re-reads that region in full, so each re-render inside one is a message spoken aloud to someone who did not ask for it, and the operative question is how many times a notice may speak rather than how often it may update. The budget is normally one per episode, and two consequences follow. A value that would otherwise change on a timer is pinned to an event instead, because a ticking counter spends the budget again on every tick for the life of the episode; and where a fact necessarily arrives on a later commit than the notice carrying it, that second commit is flushed before the browser paints so the pair collapses into a single announcement. Coarse wording is preferred to precise wording for the same reason — a phrasing that changes less often speaks less often. Exceeding the budget is a deliberate choice reserved for facts the viewer needs told again, never a side effect of how a value happened to be computed.

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

## Repo identity

### Canonical repo origin

The one spelling of a repository's git origin that every surface uses as that repository's identity — an https form with the transport scheme, any user prefix, the `.git` suffix and any trailing slash removed — so the several ways a person or a git provider may write a single origin all reduce to one string.

Canonicalization means reaching a fixed point, not running the reduction once. The reduction removes optional parts in a fixed order, so an origin carrying two of them in the opposite order is still not canonical after one pass. Any boundary that stores a canonical repo origin under a uniqueness constraint must therefore iterate to the fixed point, and must refuse an origin that will not settle or that cannot be parsed at all: a value stored in a spelling the canonicalizer does not itself reproduce defeats the constraint silently, and is only recoverable by a data migration plus a policy for the collisions that migration exposes. Boundaries are free to differ in how many passes they apply, so a consumer receiving an origin from another boundary must canonicalize it rather than assume it arrived canonical.

A separate, weaker reduction exists for stream deduplication; it discards the scheme and belongs to a different key space. The two are never substitutes for one another.

### Repo origin ownership

The rule that a canonical repo origin is held by at most one organization across the whole installation, rather than once per organization. Ownership is enforced as a uniqueness invariant over the stored origin, so it survives any application path that forgets to check it — which is what makes the canonical form load-bearing rather than cosmetic. Re-asserting an ownership one already holds is the same claim, not a second one; asserting one another organization holds is refused.

Ownership is deliberately asymmetric between reading and claiming. Asking who holds an origin one does not hold answers exactly as an unheld origin does, so the holder's identity is never disclosed by a read; attempting to claim such an origin is refused as already held. Occupancy is therefore discoverable through the claim and not through the read, which is why a caller cannot decide from a read alone whether an origin is free — and why the refusal a user sees is raised where the claim is made rather than where the mapping is loaded.

Ownership is a claim, not a proof of control. Nothing in the write path demonstrates that the claiming organization has any relationship to the repository the origin names, so the first claimant of an origin holds it whether or not the repository is theirs. A reader that routes on these rows must therefore authorize independently rather than treat the row as authority: the accepted route is to serve a mapped organization only to a party that is a member of it, and to refuse otherwise. That gate removes the confidentiality harm — nobody is handed another organization's data — and leaves an availability one in its place, in that an origin claimed by a stranger stops that repository being served at all until the claim is released. The trade is deliberate: silently falling back to the default scope when the mapping does not authorize would route data somewhere the mapping says it does not belong, and do it invisibly.

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

### Chat identity

The durable name a chat is known by everywhere outside its own record — what resume, routing, and
message delivery all address it by, and what the coding agent's own conversation is anchored to.
It is distinct from the record's internal key, which nothing outside storage uses.

Identity is not recoverable, and that asymmetry is the whole reason it is a concept rather than a
field. A chat that keeps its identity keeps the provider's conversation, so reopening it continues
the same discussion; a chat whose record is destroyed and replaced by a fresh one answering to the
same name comes back looking entirely healthy while the agent has forgotten everything said to it.
Nothing later can tell those two apart, which makes every restore path an identity-preservation
problem first and a process-restart problem second. Because the identity is also the only thing a
restore has to go on, it must be single-valued: if two records could answer to one name, the writes
addressed to it would be ambiguous rather than merely wrong.

### Rebind

Restarting a chat's agent process against the chat's **existing** record — the chat-level
counterpart of a session-level **Resurrect**, and the only sanctioned way to reopen a chat whose
**Chat pane** is gone.

A rebind writes only the fields the restore actually re-establishes and leaves every other field
standing, because the fields it does not name are the **Chat identity** it exists to preserve. It
refuses when no record carries the identity it was asked to restore, rather than falling back to
creating one: a create would satisfy the request and destroy exactly what the rebind existed to
keep, so the refusal is load-bearing and must not be routed through any error that existing
recovery paths already read as "then make one".

### Partial session read

A session listing that reached some of the caller's organizations but not all of them, and which
names the organizations it could not read alongside the sessions it did.

A read across many organizations has three outcomes, not two: it served everything, it served part
and can say which part is missing, or it served nothing. Collapsing the middle outcome into either
neighbour is a defect in both directions. Reported as a failure, a single unreachable organization
hides every session that was successfully read; reported as a plain success, the listing silently
presents itself as complete while sessions are absent from it, which is indistinguishable on screen
from the user simply having none. The missing organizations are therefore carried beside the results
rather than in place of them, and a failure signal keeps its stronger meaning: nothing was read at
all.

Partial-ness is a property of the fan-out, not of every reader. A caller reading a single source
that either answers or fails has no partial outcome to report, and its empty list of unreachable
organizations is a true statement about its own topology rather than a value standing in for an
answer nobody computed.

Widening a listing across organizations also silently separates it from the single-item read that
opens one of its rows. A list that surfaces sessions the caller cannot then open is worse than the
narrower list it replaced, so the scope of the item read is verified alongside the scope of the
list.

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

### Local-only mode

The posture in which a Bossanova process does no orchestrator communication at all — no upstream
sync, no account or subscription check — running entirely against local state.

Local-only mode is a property of one process, not of the machine or the account: it is selected by
giving a process an explicitly empty orchestrator endpoint, and a sibling process that did not
receive it keeps talking upstream. Two things make it easy to half-apply. It is reached by an
explicitly empty endpoint rather than an absent one, so it can only be expressed where the reading
code distinguishes an empty value from an unset one — where a read collapses those two, the setting
is inert and silently does nothing. And because the daemon and the terminal UI each read the
endpoint from their own environment, applying it to one leaves the other fully connected; the result
is a partial effect, which is a weaker failure signal than none at all.

## Boss-build runs

### NO_CHANGE

A `boss-build` terminal state meaning the run intentionally produced no implementation change because
there was no eligible work it could safely own, such as no candidate, a lost claim, or foreign work.

`NO_CHANGE` is evidence-bearing: when a run has touched tracker state, it must preserve a short
diagnostic breadcrumb and restore only state that the run itself produced, so a later run can tell a
healthy yield from an untouched ticket without clobbering a third-party owner.

### Run bookkeeping

Every surface on which a `boss-build`, `boss-plan`, or `boss-repair` run **records what it did**, as
opposed to the work itself: the route receipt and its stamps, the run-note and findings ledgers, and
the installed-skills drift report. None of it is work state, so none of it is one of `BLOCKED`'s four
causes — a failure in any of it emits one uniform, greppable
`warning: <what failed> — bookkeeping only, work state unaffected` line and the run continues to the
terminal state its work earned. The uniform wording is what keeps an accounting gap auditable in a
transcript after it has stopped being terminal.

The axis that decides whether a check may be demoted this way is **recording versus capability**. A
check that records is advisory: a drift report only says which payload this run read, and a
stale-but-present tree still runs. A check that establishes a _capability_ is not — a missing install,
a missing toolbox directory, or a helper that produces no verdict on stdout means nothing downstream
can execute at all, so those stay hard stops. Relaxing the first kind while accidentally relaxing the
second is the characteristic defect of that change, because the blocking version never had to tell the
two causes apart.

Advisory governs the receipt **as a record**, not the side effects its stamps attest to. Stamps such
as `lock-released` mark mutations of _shared_ state, so a missing one still obliges the run to perform
or re-attempt the cleanup it names; what the missing stamp alone may not do is change the terminal
state. Compare **Gate outcome**, which draws the same "ran and said no" versus "could not be
evaluated at all" line on the fail-closed side.

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

### Child wall clock

The per-child time budget after which the driver stops waiting on that child
and disposes of it as a permanent failure. It is measured per child from that
child's own start, never against the epic as a whole, so a long-running child
expires on its own schedule rather than being cut short by elapsed epic time.

The budget is operator-configurable with a built-in default, and is resolved
through the skill-config seam rather than read as a literal, so an absent
configuration block yields the default rather than an unset value — an unset
budget would make the expiry comparison always false and produce a child that
can never expire. Every phase that compares against it resolves it in its own
shell and fails closed if it cannot, because the resolution is not carried
across phases.

### Isolate

The permanent-failure disposition for a child: leave the session and its
work open for a human, mark the ticket failed inside the run, and skip its
transitive dependents. Isolation never stops or deletes the session —
evidence is preserved.

Isolation is strictly scoped to the one child it names. Siblings already
running continue untouched, and only that child's transitive dependents are
skipped; a child expiring its **Child wall clock** therefore removes that
child from the run and nothing else.

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

Realising a bound Account's credential into the environment an agent process is actually started with — assembling the Account home and handing the process the environment pointing at it. Injection for a bound Account fails closed: a failure refuses the spawn rather than degrading it to the implicit system-default account.

The degrade it replaces was indistinguishable from success at the moment it happened, and stayed indistinguishable for as long as the ambient login happened to hold the same credential that was intended — the session ran normally to completion under a credential nobody selected, attributed and rate-limited against the wrong identity, while the bound account recorded no usage at all. A degrade announced only in a log is, in practice, not announced. A run that cannot be given the identity it was bound to therefore does not run. Only a spawn with no binding to honour still uses the ambient login, which there is the explicit choice rather than a substitution for one.

A refusal is typed by the layer that knows why it failed, and the type separates a credential that cannot serve from an evaluation that could not be completed — the second fails the spawn just as closed, but must never be reported as a credential fault or recorded as durable credential state, because a guard that cannot tell the two apart reports every outage as a bad credential. No caller re-derives that distinction by matching on the message text. The refusal is recorded on the Account itself, where every surface that already lists accounts renders it, and its operator-facing form is masked while the raw form is kept for the log and for unwrapping.

### Account health

Whether an Account is currently usable, as distinct from whether an operator has enabled it: Rotation selects only an account that is both enabled and healthy. Several unrelated events write it — a credential the provider rejected, a confirmed suspension, a Credential injection that could not be completed — and each records a reason alongside it saying which, because the remedies differ and the health value alone does not separate them.

The writers differ in what they prove, so they differ in how their records may be withdrawn. A failure that indicts the credential itself must stand until something re-establishes the credential, and no later success on an unrelated path may clear it. A Credential injection failure is a local condition that says nothing about the credential, so it withdraws itself on the next injection that succeeds — but only its own record: a reason another writer left is preserved, and each writer's automatic clear is scoped to the reasons attributable to it, so no writer can launder another's diagnosis into a self-clearing one. A record that erased state it could not restore would not be withdrawable at all, which is why a self-clearing writer must leave everything outside its own reason untouched. Withdrawal also depends on a later attempt actually being made, and an unhealthy account is one Rotation will not normally select — so the automatic clear is a convenience, not a guarantee.

Health answers only whether the account is usable right now, which is why it does not subsume the Credential check: health has no way to say that nobody has asked, and no way to distinguish a provider that rejected the credential from a provider that could not be reached to ask. The two are recorded separately and rendered side by side for that reason.

### Credential check

The daemon's own recorded verdict on whether a stored credential still works, kept on the Account with when it was obtained. It is a durable, redacted record rather than a live probe result, so every surface reads the same answer and none of them has to re-ask.

Its point is to keep three answers apart that a single health cell collapses. _Nobody has asked_ is not a clean bill of health, so it renders as its own value and carries no age — inventing one would claim a verification that never happened. _Asked and accepted_ is the only clean answer. _Asked and rejected_ is a confirmed fault in the credential itself, and _asked but no verdict reached_ — rate-limited, provider transient, runner unavailable — is not evidence about the credential at all.

The reason is always a short token drawn from a closed set, never the provider's own message: that text can embed credential material, so it is kept for unwrapping and never rendered. An unrecognised value classifies as undetermined, never as invalid.

A confirmed-rejected credential is refused before anything is materialised, so a known-dead Account never reaches the keyring, a worktree, or a spawned agent. That verdict stands until a new credential replaces the rejected one; no later success on an unrelated path withdraws it.

### Reauthentication

Re-running a provider's interactive login and storing the result on the **existing** Account, replacing only the secret. The id, label, priority, and every session binding survive it — which is the whole point: adding a fresh Account instead leaves the rejected row in place, still named by whatever sessions were bound to it, and turns one broken binding into two rows an operator has to reconcile.

It is a distinct verb from the two neighbours it is easily confused with, because they differ in what they need and what they prove: one _acquires_ a credential through the provider, one _writes_ a credential the operator already holds, and one _asks_ the provider whether the stored credential still works and records the answer.

Success is reported only once the daemon's post-save verification has returned a verdict; a save that merely did not error is not a success, and a verification that could not run is reported as such rather than rounded up. A failed verification leaves the Account in place rather than removing it, so a partial recovery never silently destroys the binding it was trying to repair.

### Rotation

The daemon's automatic response to a usage-capped account: put the limited account on cooldown, select the next eligible account for the provider (active, healthy, not cooling, lowest priority first), and respawn/resume the interrupted session under it — posting an in-chat notice, and never auto-resending the interrupted prompt. On by default once extra accounts are registered, with per-repo opt-outs; `managed_accounts.enabled=false` (set via `boss settings --no-managed-accounts`; `--no-rotation` is a deprecated alias) is the global kill-switch that halts all automatic rotation, re-read live per decision, while manual `boss account switch` keeps working. Every decision is audit-logged as a rotation event (labels only, never credentials).

### Switch

The daemon operation that moves a chat from its currently bound Account to a different target: validate the target Account, stop the chat's pane, rebind it, and resume or start fresh under the new Account. A manual account change, a Rotation, and a Respawn-in-place all ultimately issue a Switch.

Validation happens before anything about the chat is touched, so a Switch can be refused — the target Account is disabled, failed, or cooling, or the chat is mid-turn — without disturbing the chat at all. That refused-before-touched distinction matters to anything that charges a budget per Switch attempt: a refusal that touched nothing must not spend the same budget as an attempt that reached the chat and then failed. A Switch rebinds the chat and deliberately leaves the session's own Account seed untouched, so everything that later resolves an Account for that chat must read the chat's binding rather than the session's — see Account authority.

### Account authority

Which layer's Account binding governs a particular spawn: a chat's own binding when it has one, otherwise the session's seed. Because a Switch rebinds the chat and leaves the session's seed alone, the two can legitimately disagree — and a chat that has never bound an Account of its own is a distinct state from one explicitly bound to the system-default account 0.

Authority is a property of the layer that supplied the binding, never of the value it supplied: a chat's Account frequently equals its session's, so comparing the two values cannot distinguish a chat-bound spawn from a session-seeded one. Every resolver on the spawn path must therefore be keyed on the authority the caller already determined rather than re-deriving it — including the one that tells the Failover proxy which Account's credential to present downstream. A resolver still keyed on the session after a chat-scoped Switch does not fail: it presents a valid credential for the wrong Account, and the run proceeds, and bills, normally.

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

A read-only check of how much of an account's provider quota remains, reported as one Utilization window per quota period, which keeps rotation's eligibility decisions current. It runs periodically on the daemon and on demand when a user asks for it in the web UI — one request shape carrying a refresh flag, so the same probe failure can arrive either in the background or in direct response to a tap. Where the provider exposes a dedicated usage endpoint the probe is free; where it does not, or where the credential lacks the scope to read it, the fallback path issues a real metered call — so a probe that silently takes the fallback is an ongoing cost, not a cosmetic defect.

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

## Cloud billing

### Cloud account

A billing identity that ties a Bossanova user or organization to the external customer record used
for Cloud subscription access.

For organization billing, the Cloud account is the bridge between an organization and its
subscription provider customer. Work that starts from a subscription event resolves through that
bridge before it can know which organization to reconcile.

### Checkout claim

The single-winner admission a caller must take before a hosted subscription checkout may be created
for a Cloud account, granted by a compare-and-swap on the account's setup state so that exactly one
concurrent caller proceeds.

A caller that loses the claim is refused, not queued: the winner is holding a live checkout for the
same account, so the honest instruction to a loser is to retry, which resumes the winner's session
rather than minting a second one. The claim is deliberately _not_ released when the payment provider
errors — releasing it would return the account to a state outside the double-charge guard, and the
next attempt would mint a second completable session. Because an unreleased claim leaves the account
holding an abandoned checkout, keeping the claim is only safe in a system where an abandoned checkout
is resumable.

### Abandoned checkout

A Cloud account holding a checkout session that was created but never returned from — the tab was
closed, the caller crashed, or a claim was kept after a provider error.

An abandoned checkout is not a subscription in flight. The account keeps its existing access decision
and its ability to start a checkout; the only thing it additionally reports is that a session was
already started, so a client can offer to _resume_ rather than restart. That distinction is carried
on the wire as a pair of booleans — checkout started, and checkout still creatable — and the two read
together: both true means abandoned, whereas a started checkout that may no longer be recreated means
an entitlement is genuinely landing. Confusing the two is what strands an account: treating an
abandoned checkout as an activation in progress puts the client into a poll loop that promotes the
account into a pending entitlement it will never reach.

### Pending entitlement

The state of a Cloud account that is understood to be returning from checkout but whose subscription
entitlement has not yet been observed, so access is withheld pending a refresh.

It is the one Cloud billing state a client is right to _wait out_ — polling resolves it — which is
exactly why it must not be entered speculatively. An account promoted here without a real payment
behind it has no event coming to release it, and no checkout affordance to escape through, and the
promotion is sticky. The promotion is also, today, inferred rather than observed: any entitlement
poll that arrives while a checkout is started looks the same as a genuine return, so a client that
polls eagerly after opening checkout can strand its own account. Distinguishing the two needs an
explicit returning-from-checkout signal; narrowing the inference alone is not the fix, because during
the provider's propagation window it would tell a user who has just paid that their checkout is
unfinished.

### Organization mirror

The local record of an organization whose authoritative identity lives in the external identity
directory, holding the reference that binds the two together.

The mirror is what every surface in the product reads, so an organization the mirror does not know
about is invisible here even when the directory still has it. That asymmetry decides the ordering of
any operation spanning both stores: the two cannot be written atomically, so a failure between them
leaves an inconsistency either way, and the tolerable direction is the one that stays visible and
therefore actionable — a mirror record whose directory organization is gone, rather than a live
directory organization no mirror record references, which is reachable only from the provider's own
console. Choosing that direction is only safe when a repeated attempt can actually finish the job,
which requires the directory half to treat "already gone" as its own success rather than as a
failure; a mirror record may also exist before it is ever bound to a directory organization, in which
case it has no directory half at all.

### Organization membership

The project-specific relationship that grants a user access inside an organization and carries the
role used by organization-scoped authorization.

Membership changes are access changes first. Adding a membership can require billing capacity before
the access grant; removing one must revoke access even when billing reconciliation is temporarily
unavailable.

### Pending organization invitation

A directory-provider invitation to join an organization that has not yet become an Organization
membership.

It may appear beside active members in a directory view, but it grants no access and does not count
as a member or billable seat. Its identity and lifecycle remain provider-owned until acceptance
creates the real user-to-organization relationship.

### Claimed organization

The single organization a request asserts it is acting within, as distinct from the set of
organizations the caller belongs to.

The two are not interchangeable, and the gap between them is an authorization boundary rather than a
convenience. A claim is an assertion the caller makes, honoured only once it has been checked against
membership; the member set is derived server-side and is the whole of what a cross-organization read
may span. Because the answer depends on who is asking, a verdict that is correct for one caller can
be wrong for another whose member set differs. Such verdicts are caller-relative, and they cannot be
adjusted after the fact from the response alone, because by then the response no longer carries the
caller who earned it.

An access question therefore resolves in one of two modes: judged from the claimed organization
alone, or folded across every organization the caller belongs to. The folded reading is the default,
so a caller that expresses no opinion — an internal process with no client on the other end — gets
the broader answer, and only a request serving a client that predates the widening is narrowed back
to its claim.

Where the fold spans several organizations it is ordered rather than arbitrary, and the ordering
exists to keep the fan-out from costing the caller anything: a failed sibling read may degrade an
answer that would otherwise be a denial, but never one the claim alone already authorises. Without
that asymmetry the widening could revoke access it was never able to grant.

### Resolved organization

The single organization selected server-side when an organization-spanning lookup finds the resource
a deferred operation will act on, distinct from both the caller's Claimed organization and the full
set of Organization memberships.

Resolution fixes resource identity, not future authority. A token-bound continuation carries the
Resolved organization so later lookups cannot repeat broad discovery and silently select a duplicate
identifier in another organization; it still refreshes Organization membership before use, and a
failure to refresh remains an infrastructure error rather than proof that access was revoked.

### Trial eligibility

Whether a person may be granted an introductory free trial on their next subscription
checkout, decided server-side from the subscription provider's own record of their prior
subscriptions rather than inferred from local state.

Eligibility has three answers, not two: eligible, ineligible, and undetermined — the
provider could not be asked. The distinction is load-bearing because the same question
is asked twice on one journey, once to render the offer and once to create the
checkout. An undetermined answer fails the checkout rather than quietly withdrawing the
trial the offer may have just promised, while a determined ineligibility proceeds
without one; the offer copy correspondingly withholds the promise instead of asserting
its negative. Eligibility is scoped to the person, not to the Cloud account: a Cloud
account is minted per organization, so the question is resolved across every Cloud
account sharing the caller's user id and a trial consumed under any one of them makes
the person ineligible under all of them. Sameness is a shared account holder and nothing
more — a second sign-up email is still a second person.

Resolving over several accounts makes a partial answer possible, and the fold is ordered
so that a determined answer outranks a missing one: a provider query that succeeds and
reports prior history settles the question as ineligible whatever the other accounts
did, eligibility requires every query to have succeeded and none to report history, and
only "no history found, and at least one account could not be asked" is undetermined. A
resolution that yields no accounts is never eligible; the caller's own account is always
in the set.

### Seat reconciliation

The Cloud billing process that sets billed subscription quantity from the authoritative organization
membership count instead of applying relative increments or decrements.

Seat reconciliation treats membership mutations and subscription webhooks as repair opportunities:
when the observed billed quantity already matches the desired membership count, it writes nothing;
when they differ, it sets the billed quantity to the desired count.

### Account setup state

The rung a Cloud account currently occupies on the ladder from merely linked to a billing customer,
through a checkout in flight, to actively subscribed — the value the access decision reads and the
value the checkout claim compare-and-swaps on.

Movement up and down the ladder is not symmetric. Read-time refresh only re-examines accounts that
could still be waiting on a subscription, plus those already active, so an account demoted off the
active rung is outside the set anything ever re-checks. Over-granting access therefore costs at most
one staleness window and repairs itself; wrongly demoting an account strands it until a human
intervenes or the customer pays a second time. Any write that moves an account down the ladder on
the strength of an external assertion must be treated as irreversible and refuse to proceed on an
answer it could not confirm.

### Read-time subscription verification

The bounded check that asks the payment provider whether a Cloud account's customer still holds a
live subscription, memoised per customer for a short window so the entitlement path is not a provider
call per request.

It survives the arrival of pushed subscription events rather than being displaced by them. Event
delivery is at-least-once but not guaranteed, and a delivery is recorded as seen before its effect
runs, so a redelivery of an event whose effect failed is acknowledged and dropped. A failed demotion
leaves the account on the active rung, which is exactly what the read-time check re-examines — making
it the floor the entitlement state self-heals down to, not duplicated work. Only successful answers
are memoised; a provider error must not be cached as a verdict.

## Chat coordination

### Callback

A standing, one-shot request to be told when a pull request reaches a named state — its checks
resolving, the PR merging or closing, or the PR becoming ready for review or ready to merge — so a
waiting run can be woken promptly when delivery succeeds, while a bounded poll remains its safety
net. Registering one is an alternative to waiting, never a substitute for deciding: the woken run
still reconciles the real state authoritatively before acting.

For callback-wait workflows using the CLI adapter, callbacks are usable only where two independent
things hold: the run is daemon-managed, so something is behind the callback interface to answer,
**and** the CLI that issues the registration actually resolves here as a file this process can
execute. An environment can supply the first without the second — a scheduled one may export the
managed-session marker while leaving the CLI off its search path — so a gate keyed on the marker
alone reports callbacks usable, arms a registration that cannot run, and burns the attempt. Where
callbacks are unavailable the waiting run degrades to bounded polling, and because that fallback is
legitimate behavior rather than an error, the unavailability must carry a stated reason: nothing
else in the system will ever raise it, so an unexplained degrade can be mistaken for normal
operation across successful runs.

Callback capability and callback selection are separate. Capability is the complete vocabulary a
callback interface accepts; selection is the narrower set a particular waiting workflow arms by
default. Widening the former never widens the latter implicitly. Watches match the current state by
default; transition matching is an explicit opt-in that suppresses an initially satisfied match and
makes the watch eligible on a later evaluation. It does not guarantee a false-to-true edge.

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

A stamped step is a caching decision, not an enforcement point. A correctness check mounted on one inherits the skip, and it inherits it precisely for changes outside the hashed input set — so a check that reads a tree the key does not cover goes silently non-gating on exactly the change class it exists to police. The same reasoning governs a CI path filter, a Make prerequisite, and the local affected-test selector: whatever triggers a check must be keyed on everything the check reads, not on where it lives. These are separate routing tables, not mirrors of one another — a path added to one does not follow into the others, and an input can be covered on one surface and absent from the next.

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

The same reasoning governs the whole installed **skill tree**: a `boss skills check --gate`
discrepancy warns rather than blocks, because the report only says which payload this run read. Its
limit is the one drawn under **Run bookkeeping** — drift is a recording discrepancy, but an installed
tree that is _absent_ rather than stale is an absent capability, and still blocks.

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

A gate can become vacuous at any of nine layers, and each has been observed independently: its
**trigger reach**, when the routing tables that decide whether it runs are keyed on where it lives
rather than on everything it reads, so it never executes on the change class it exists for — and a
green check is then indistinguishable from a real pass; its
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
with itself; its **observation environment**, when the check runs in a substitute runtime that does
not compute the property the assertion reads, so the reading is a constant no subject can move — the
assertion then holds identically for a correct subject, a broken one, and one where the property was
never defined at all, and it is worst when that constant happens to equal the value the requirement
wanted; its **observation moment**, when the check reads a subject that has not arrived yet, so the
artifact it certifies predates the state it claims about — the runtime is real and the property is
genuinely computed, merely at a time when the answer is not yet the one under test, which is why
this layer resolves as a race whose two outcomes are a correct pass and a silently wrong pass but
never a failure; and its input discovery, when the set of subjects it scans comes back empty and that
emptiness is reported as a pass rather than as an inability to evaluate. The failure is silent by
construction, because a gate's job is to be quiet when nothing is wrong.

Discovery can come back empty without the discovery expression being wrong. Where a check reads its
subjects off disk rather than carrying them, the corpus is assembled by whichever runner executes
the check, and runners differ both in what they place within reach and in where they start looking.
One unchanged check is then sound under the runner an author invokes by hand and empty under the
runner that gates the merge — so which runner produced a green is part of the verdict, and a green
from the convenient one is not evidence about the deciding one.

The first two layers and the last are the three a **Falsification** usually cannot reach, and so the
ones that most need checking by hand: the other, mis-answering layers each mis-answer a value that
arrives at the check, while a reach gap means no value ever arrives, and no adversarial input you can construct will demonstrate
it. Trigger reach is the purest case — the check does not execute, so there is no run to feed. The
widened surface is the exception that fixes the rule's shape — there the offending value
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

All nine are properties of the check. The mirror case is a property of the **defect**: a check that is
sound at every one of these layers, answering correctly the question it was built to answer, over a
corpus that does contain the damaged artifact — and still green, because the defect does not violate
the property that check tests. That is a **Laundered defect**, not a vacuous gate, and the two want
opposite responses: one is repaired, the other needs a new detector.

### String-space consumer

A reference to a removable thing — a backend, a driver, a dialect, a mode, a directory — that names it
as text rather than as a symbol, so removing the thing leaves the reference intact and no compiler,
type checker or link step reports it. Environment variables and their defaults, configuration and
workflow files, harness and fixture setup, hand-written schema or seed data, build and task targets,
suppression comments naming a rule or a path, generated instruction mirrors, and ordinary prose are
all string space; call sites, imports, constructors and enum symbols are symbol space.

The distinction matters because the two spaces are enumerated by different tools and finish at
different times. Symbol space is enumerated exhaustively and for free: it is done when the build is
green, which is why a deletion feels complete long before it is. String space has no enumerator at
all — only a text search whose spelling the searcher chooses — so its consumers are found by
deliberately listing the surfaces that select a thing by name, and are otherwise found by whoever
next runs the surface that still names the removed value. Their failures land far from the deletion
and out of order: a startup that rejects a value that used to be legal, a job that runs against a
dependency nothing provisions, a target that exits successfully having executed nothing, a document
that confidently describes a path that no longer exists. A review scoped to the deletion's own diff
cannot see any of them, because string-space consumers live in the files the diff never touched —
its green is a statement about the hunks, not about the consumers the deletion orphaned. Keeping a
removed name gone once its consumers are cleaned is a **Retirement ratchet**; the enumeration that
finds them in the first place is upstream of that, and is the half with no gate.

### Retirement ratchet

A standing assertion that a name already removed from the codebase — a hostname, provider, resource,
environment variable, label — stays removed, so the removal is enforced by a check rather than by
whoever remembers it. It is the durable half of a retirement: the edit that removes the last
reference is cheap and the ratchet is what makes it stick.

A retirement ratchet is structurally self-obstructing, because it has to name the very string it
forbids, and a retirement is usually accompanied by a sweep asserting that string appears nowhere —
so the guard answers its own sweep. Two remedies exist and they are not interchangeable: where the
text is code the team owns, the forbidden name is assembled at runtime from fragments that no literal
search matches, and nothing has to be excluded; where the text is prose that must quote the name, the
sweep is scoped to skip it, at the cost of an exclusion list that grows with every later mention.
Prose explaining a retirement is therefore the sweep's most likely future false positive, and is
written in the assembled form for the same reason the guard is.

Coverage is the other recurring failure. A ratchet is named after the concept retired, not after the
first file that happened to reference it, because the surfaces a retirement touches outlive any one
of them; and each guarded path needs an existence assertion of its own, since an absence check over a
path that does not exist is a **Vacuous gate** — it passes on a renamed or deleted file exactly as it
passes on a clean one. Where part of a retirement is deferred — an infrastructure teardown a human
must apply — the ratchet cannot yet assert the absence, and the obligation belongs beside the check
whose coverage the teardown restores rather than only in the planning record.

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
what was verified. Three causes produce it, and all are properties of the criterion rather than gaps
in the work: the signal it measures does not exist yet, because the distinction it asks to split on
is precisely the distinction the change introduces and no historical data was ever told about it;
or the change itself closes the only window in which the measurement could be taken, so merging is
what makes the criterion permanently unmeetable; or it demands provenance from a corpus that never
existed, as when a **Pure relocation** is asked to show its new assertions were lifted unchanged
from earlier ones while nothing was reachable to lift from until the move itself made it so.

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

### Unreachable evidence

A criterion that is genuinely true or false in the world, but whose deciding agent sits outside the
system under test, so no harness the project runs can observe the outcome either way. The claim is
worth keeping; only its proof is out of reach.

It is neither a **Vacuous gate** nor an **Unsatisfiable criterion**, and the difference decides the
repair. A vacuous gate is a check that could have discriminated and does not, so the repair is to the
check. An unsatisfiable criterion could never be true at all, so the repair is upstream, in the prose.
Evidence is unreachable when the criterion is sound and the check is honest and no better runner
exists to promote it — choosing a different harness relocates the assertion without strengthening it.

The repair is downstream and has two halves. Scope the assertion to the part the harness genuinely
observes — typically what the code emits, rather than what the external agent does with it — and name
the test after that emission rather than after the outcome, because a test name is quoted as a
coverage claim far more often than its body is read. Then record the boundary beside the assertion as
a permanent accepted gap rather than a deferred one, since no future ticket closes it. The hazard worth
recording is that the external contract can change with no change here at all, so the emission check
stays green while the behaviour it was written for regresses unobserved.

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
found nothing. A surface can also be widened by the gate's own pipeline rather than by
its walker, which is a **Self-erasing scope**, or by a later edit to the artifact it reads, which is
a **Borrowed bound**. A surface that is correct in region, granularity and width can
still miss on **multiplicity**, when the value it binds is stated many times and the binding covers
one of them; that is a **Rename detector**.

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

### Self-erasing scope

A guard's structural boundary that is destroyed by the guard's own normalising step, because the
markers delimiting it belong to the class that step deletes. Goose's migration legs are the standard
instance: `-- +goose Up` and `-- +goose Down` are SQL comments, so a text assertion that strips
comments before matching has already deleted the only thing that told it which leg it was reading,
and every assertion built on the stripped text answers "does this token appear anywhere in the file"
rather than "does the forward migration run it". The result is a **Scan surface** too wide for a
reason no inspection of the glob or the walker will reveal, because the widening happens inside the
guard one line before the assertion.

Its distinguishing property is that both transforms are load-bearing, so the repair is not to drop
one of them. Stripping is mandatory wherever the artifact's own prose quotes the predicate the
assertion searches for, since the explanation then satisfies a raw match by itself and a deleted
conjunct goes unnoticed; bounding is mandatory because without it the section identity is gone. The
rule is therefore an ordering rather than a choice: a transform that consumes structure encoded in
what another transform deletes runs first — bound to the section against raw text, then normalise
the slice. The collision recurs wherever a delimiter shares its syntax with the noise, as `#region`
markers, YAML document splits, and fenced-block boundaries all do with their respective strippers.

Absent markers must then fail loudly rather than yield an empty region, or the repair reintroduces
the defect one level down: an empty slice contains no forbidden token and no required one, so two
empty slices compare equal and a parity check passes on nothing. A self-erasing scope is also
invisible at review altitude, since the assertion reads exactly like a correct one, and a
**Falsification** aimed at some other property of the same file passes straight through it — a
mutation that any assertion catches certifies the file, not the assertion.

### Blind clobber

A write that assigns a field it carries no opinion about, so a writer structurally unable to supply
that field erases what a better-informed writer already recorded. Nothing fails: the write succeeds,
and the field simply reads back at its type's zero value. The loss is intermittent by construction —
it appears only on records that more than one writer touches, so a record written once looks
perfectly correct and the gap reads as the recorder having missed that case.

Adding a field to a persisted record creates one blind clobber per existing constructor of that
record, which makes enumerating the constructors — rather than reading the writer that motivated the
change — the audit. Completing them is necessary and never sufficient, because some writers cannot be
completed at all: a converter whose source is a wire message with no such field must pass the zero
value forever, and the tell is that the source is an external shape rather than a local computation.
The write itself must therefore distinguish "no opinion" from "zero", which is sound only while no
writer legitimately records a zero — a precondition to state rather than assume, since where zero is
a meaningful value the same sentinel makes clearing the field silently unperformable. Both directions
need pinning, and neither test is optional: one that proves only preservation passes equally on a
sink that ignores the field outright, and one that writes a single time cannot observe the defect at
any level of effort, because the first write is always correct.

### Borrowed bound

A gate's window boundary taken from the artifact under test — a quoted sentence or phrase the
document itself carries — so the region read is a function of that document's own layout rather than
of the gate. It differs from a **Self-erasing scope**, where the widening happens inside the guard's
own pipeline: here the guard is untouched and still correct, and the artifact moves beneath it.

Its two directions are asymmetric, and the silent one is the one that ships. Deleting the borrowed
phrase is loud — the lookup finds nothing and a fail-closed helper refuses — so that direction is
already covered by ordinary discipline. Inserting matter before it is silent: the region grows,
every positive assertion still finds the phrase it names somewhere inside the enlarged span, and the
run is green. Nothing counts what was read, nothing marks the change, and the gate's own file shows
no diff at all, so review has no surface to notice it on. A helper that refuses absent or
out-of-order markers does not reach it either, because both markers are present and in order; the
slice is byte-correct and merely means something else. The result is a claim about one unit demoted
to a claim about a neighbourhood — and a widened region admits the very next edit that widens it
further, so the fault accumulates where a deletion fault would have self-limited at first contact.

The remedy is to bound the region by something the document cannot reflow past — the end of the unit
the invariant is actually stated over, or a structural boundary at that same granularity — or, where
a borrowed phrase is the only handle available, to state the region's upper bound as its own
assertion: that the span does not contain a marker belonging outside it, or that its extent has not
grown. A structural boundary is a remedy only when the structure matches the invariant's
granularity; one that spans far more than the unit is sound and useless. Where adjacency is what the
prose itself depends on — a bare deictic that resolves by position — adjacency is the thing to
assert, rather than a window relied on to imply it.

### Rename detector

A drift gate that binds a repeated value at one occurrence, so it reports a rename once rather than
enforcing a sync. It is the multiplicity failure of a **Scan surface**: the walker reads the right
region at the right granularity and the rule judges correctly, but the comparison is quantified over
one member of a set. The distinguishing behaviour is that the gate goes green on the repair a human
performs first — it reddens the canonical line, the fixer edits that line, and every other copy of the
old value ships under a clean report. Where a **Vacuous gate** checked nothing, a rename detector
checked exactly one thing, and one-of-thirteen is byte-identical in output and exit code to
thirteen-of-thirteen.

The same defect written in a matcher is an **instance-pinned pattern**: a regex naming a literal member
of the class the rule is about — one ticket id, one hostname, one version — rather than the class. The
tell is a pattern that could not have been written before the first occurrence existed. It does not
decay loudly; it keeps running, keeps costing CI time, and keeps reporting clean while a later member
of the same class ships past it, which makes its escapes retroactive and invisible in review, since
the pattern is an unchanged constant and the violation is an unremarkable line in someone else's diff.

Neither half is reachable by **Falsification**: hand the bound line the stale value and it reddens
every time, because the rule is right and is pointed at one of many things. What settles it is a count
taken from the corpus rather than from the rule — grep the value, count the sites, compare against what
the comparison quantifies over — and a widening that follows: quantify over the class and name the
genuine exceptions in an exported allowlist rather than narrowing the pattern to dodge them. Widening a
pattern usually forces the surface the other way, since a wider match now reaches teaching examples the
narrow one never did; the two dials moving in opposite directions in one edit is the expected shape,
not a contradiction.

### Class skip

An exclusion inside a gate that is written as a predicate matching an open set rather than as an
enumeration of entries, so the excluded set is unbounded, unreviewable, and grows on its own. It is
the per-item counterpart to a narrowed **Scan surface**: the corpus is discovered correctly and the
counts are non-zero, but individual members leave it with no counter and no message, which is why a
bounded-corpus check — would the gate speak if its inputs vanished? — passes cleanly over one. It is
not a **Self-erasing scope**, which widens the surface rather than puncturing it: there the boundary
is destroyed wholesale by a normalising step and every item is then read against the wrong region,
while here the region is right and named items fall out of it one at a time.

Every branch in a gate that reaches an input it does not recognise is one of these in waiting, and
they recur in a small set of shapes: a filename filter keyed to a naming convention, an allowlist
that cannot report what it omitted, a parser that returns its "nothing to assert here" value for
text it merely failed to read, and a rule written against one representation of a value while the
surface carries another. The distinguishing property is that the skip is indistinguishable from a
real pass at the call site, so the gate is cited in review as evidence for exactly the cases it
stopped checking.

The repair is a **named exclusion**: replace the predicate with a list of entries a reviewer can
read in a diff, so a silent class becomes one reviewable line, and pair it with a ratchet requiring
every input actually encountered to be classified as scanned or ignored — an unclassified one fails
and names itself. A named exclusion is maintained by review rather than by the gate unless it is
itself ratcheted, and that trade is stated where the list is written rather than left for a reader
to infer. Fail-closed here means the _unknown_ fails, not everything: a rule that reds on legitimate
input is switched off by the first person it blocks unfairly, so the qualifiers that keep it from
firing on ordinary content are part of the pattern, not exceptions to it.

A comment can carry the same defect. One claiming more coverage than the code delivers is a class
skip relocated into the reader's head — they stop checking the case the comment said was handled —
so a claim about what a check catches is verified against the check before it is written.

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

### Pure relocation

A claim that a diff moves existing code from one place to another without altering it, so the
review question is not whether the code is correct — that was settled where it came from — but only
whether the move was faithful.

The claim is mechanical and so is its evidence: reconstruct the moved blocks from the pre-change
revision in the order the new unit lays them out, strip only the keywords the extraction itself
added, and diff that reconstruction against the new unit. Empty is the whole proof. A reviewer
reading the same lines is asserting something weaker and unfalsifiable — that nothing _looked_
changed across a diff the tool renders as one large deletion beside one large insertion with no
line-level correspondence — which is exactly the shape an opportunistic fix, a dropped comment or a
narrowed type rides in under, attributed forever to a commit whose message says it changed nothing.

The proof is fragile in two directions and both are the author's to protect. It depends on source
order surviving the move, so reordering or reformatting in the same commit destroys the cheapest
evidence available and belongs in a follow-up. And it is a claim about the moved bytes only: it says
nothing about the call sites, which is why leaving the caller's suites unmodified — demonstrably,
from the diff, not from intent — is the complementary proof rather than an optional extra. Whatever
in the commit is not a faithful move is named in full beside the claim, because a pure-relocation
claim is credible in proportion to how completely its impure parts are listed.

Which code moves is decided by data flow, not by span: a symbol belongs in the extracted unit when
it consumes only values its caller already holds and reads none of the caller's rendering state. The
symbol that fails that test is the seam, and it stays — a decision worth stating where it stays,
because the next reader will otherwise take it for an oversight. Line numbers are not the coordinate
system for any of this. A plan that deliberately sequences itself behind its own dependencies has
guaranteed its line references will be stale by the time it runs, and its symbol list may be
incomplete as well; a symbol list degrades gracefully where a range does not, because the plan's
discriminator can be applied to a symbol the plan never knew about.

### Review verdict

A build run's own record of how its whole-branch review settled — passed, ran but was capped short of
a full pass, or produced no usable verdict at all — written down so a later step or a separately
dispatched repair pass can route on it without re-running the review.

The verdict is **run-scoped, not diff-scoped**: it says this run's review passed, not that any
particular head was reviewed, and it is deliberately not re-pinned when the settle loop or a repair
pass commits on top. Absent or unreadable reads as the no-verdict value, so a review that never
settled can never be read downstream as passed. Because one of its readers runs in a different
session — one that executes neither the preflight that clears the verdict nor the step that consumes
it — the verdict can be read back against a head no review examined; what bounds that reader is its
own acknowledge-once contract rather than the freshness of the verdict.

### Advisory review

A pull-request review that is answered but does not open a fix cycle: it earns one grouped in-thread
response carrying a per-finding reason, and consumes none of the run's fix-cycle budget.

A review becomes advisory only when it is bot-authored **and** this run's review verdict says the
branch already passed its own review. Bot authorship is decided generically from the forge's own
account-type signal, never from a roster of bot names, so a newly installed bot is classified
correctly on first contact. A human changes-requested review, or a red check, still opens a real fix
cycle regardless of the verdict. The advisory path is bounded rather than open-ended — capped rounds
per run, at most one grouped response round per head — and a fix that is nonetheless taken from an
advisory finding gets the same finalize re-verification a fix-cycle repair would, so the
reviewed-tip claim is re-established rather than inherited.

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

### Stage script

A snippet a proof run installs into the page before its first paint, so that the surface being
photographed is already in the state the capture is meant to demonstrate.

Stage scripts come in two shapes that are not interchangeable. One **extends** the shared fixture,
adding the few fields its own capture branches on and leaving everything else standing. The other
**rebuilds** the fixture into a different shape derived from what is already there. Only the
extending shape may publish a **Fixture mirror**; a rebuild must not, because its output is
deliberately partial with respect to the whole fixture.

### Fixture mirror

A second global name the same staged fixture is published under, because the application fakes
resolve the two names by precedence rather than merging them — the first name present wins outright,
so a fixture written under only one name is invisible on any page where the other name is already
installed.

Mirroring is all-or-nothing per staged fixture, never per stage script: half a mirror is worse than
no mirror, because precedence means the incomplete half is what the fake resolves. Where nothing in
a given run installs the winning name, the mirror is **latent** — written as a guard against a
future writer rather than as a fix for a live defect. That distinction is stated wherever the mirror
is written, so the guard is neither deleted as dead code nor over-claimed as a live fix.

## Flagged ambiguities

- "Normalized origin URL" carries two senses that are not interchangeable. A **Canonical repo
  origin** is a repository's identity and keeps its scheme; the stream deduplication form drops the
  scheme and keys a different space. Substituting one for the other silently changes what a
  uniqueness constraint compares.
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
- "Stamp" carries two unrelated senses. A **Stamp** in the build-caching sense is a content hash over
  a step's inputs that decides whether the step is skipped; a route-receipt stamp (see **Run
  bookkeeping**) is a token a run emits to attest that it performed an act. The first gates work, the
  second records it — and only the second is advisory.
- "Rate limited" had been used for both a **Limited** account, which has exhausted its own usage
  cap, and a **Probe throttle**, which is the usage endpoint refusing our polling rate — these are
  distinct, and only the former justifies a **Cooldown**.
- "The agent is prompting" had been used for both halves of the **Delivery gate** — a pane that
  blocks input and an agent that is asking a human something — and the two had at one point been
  documented as though the first were a special case of the second. A **Boot interstitial** is the
  counterexample: it blocks input and asks nothing. They are independent predicates, and a caller
  must name which one it means.
