// Content/contract test for the boss-epic skill (BOS-177).
//
// boss-epic orchestrates an entire epic of planned Linear tickets to merged PRs,
// unattended: it assembles the epic's sub-issues, computes a dependency-ordered
// schedule, spawns parallel boss-build sessions, drives repair on failures,
// serializes merges, and reports progress on the parent issue. This test follows
// the BOS-144 content-test pattern (scripts/bs-<skill>-skill.test.mjs, mirroring
// scripts/boss-plan-skill.test.mjs). It pins:
//   * the shared helper module + contract symbols the SKILL references,
//   * the merge-serialization and session-isolation safety statements,
//   * the never-mutate-outside-the-epic-set guarantee,
//   * the no-interactive-questions-after-preflight rule,
//   * frontmatter identity + default agent,
//   * a size-ratchet keeping SKILL.md below the committed baseline rounded up,
//   * a NEGATIVE assertion against stub/placeholder prose.
//
// BOS-271 collapsed the published cores onto the boss-repair single-source
// topology: the canonical committed home is the embedded skillinstall payload
// (services/boss/internal/skillinstall/skills/boss-epic/), with no .claude/.codex
// committed copy. This test reads that canonical home; there is no codex-mirror
// copy of the four published cores to compare against anymore.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { assertArtifactSet, assertExactSize, measureFile } from './size-ratchet-lib.mjs'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const abs = (rel) => fileURLToPath(new URL(rel, import.meta.url))

const CLAUDE = read('../services/boss/internal/skillinstall/skills/boss-epic/SKILL.md')
const CALLBACKS = read(
  '../services/boss/internal/skillinstall/skills/boss-epic/references/callback-watches.md',
)

// BOS-495: the up-front callback reflex + the single `callbacksAvailable` gate must
// be present in BOTH Go mirrors (byte-identical), and boss-epic must carry its own
// callback-watches reference. The canonical home is skillinstall; the plugin copy is
// the copy-skills mirror — both are asserted so a partial edit trips this gate.
const EPIC_MIRRORS = [
  '../services/boss/internal/skillinstall/skills/boss-epic',
  '../plugins/bossd-plugin-claude/skilldata/skills/boss-epic',
]

test('size ratchet', () => {
  // Exact pin, not a ceiling (BOS-768). This used to be the committed size rounded
  // up to the next KiB, compared one-sidedly — which left 67 bytes of slack the body
  // could regrow into with nothing going red, and meant a genuine trim bought nothing
  // because it only widened the slack. `assertExactSize` reds in BOTH directions, so
  // a reduction is only cleared by banking it in RATCHET. Never raise this
  // casually — a growing SKILL.md erodes the headless context budget; split
  // situational sections into references/ (boss-plan-skill precedent) before
  // bumping. Bumped 19456 → 24576 for BOS-179, the "make boss-epic work
  // headlessly" ticket: Phase 3 was rewritten from live-chat steering to
  // headless detached dispatch (detach+model create_session, unattended
  // preamble, fresh /boss-repair watch runs, merge-time external re-check,
  // ./bin/boss fallback) — the core deliverable, prose trimmed to keep the
  // bump to one KiB above actual.
  // Bumped 24576 → 25600 for BOS-243: Phase 3c now documents the
  // `attached_existing` create_session signal (attach vs orphan-PR fallback)
  // so the repair driver can distinguish an attach from a fresh session.
  // Bumped 25600 → 27648 for BOS-198, the "rebuild boss-epic on the extracted
  // adapters" ticket: the SKILL now exposes the three pluggable seams (pure DAG
  // scheduler dag-scheduler.mjs, resolveTrackerAdapter, resolveSessionRunnerAdapter)
  // and routes assembly/state/progress + session choreography + sub-skill
  // dispatch (subSkills.implement/repair) through them — the core deliverable.
  // Bumped 27648 → 28672 for the settled-green merge gate: Phase 3b/3c now require
  // the child session to have SETTLED (chat IDLE + stale last_agent_activity_at,
  // or STOPPED + stale/missing last_agent_activity_at) before a Passing green is merge-eligible, fixing
  // premature merges of still-working children whose own boss-build review +
  // comment resolution had not finished (recurser/bossanova#1174). This makes the
  // classified greens actually match nextToMerge's "passed-review" contract.
  // Bumped 28672 → 29696 for BOS-322, the "planning-only epic work must not
  // spawn PR-backed sessions or surface false pr_no_changes" ticket: the
  // Operating Contract now carries a concrete three-case routing contract naming
  // the session-runner capabilities (implementation → `createSession` +
  // `tmux_unattended`; unattended planning → subagent; visible planning →
  // `createPlanningChat` / `quick_chat: true`) and Phase 3a states the
  // `createSession` block is implementation-only — the core deliverable.
  // Bumped 29696 → 30720 for BOS-301: Phase 0 step 3 now runs a deterministic
  // boss MCP tool-discovery preflight (requiredBossToolsForEpic source of truth)
  // that fails fast naming absent tools before scheduling — the core deliverable.
  // Bumped 30720 → 32768 for BOS-302: Phase 1 step 3 now defines an explicit
  // zero-launch (no-ready/no-inflight) branch — one upserted progress comment
  // carrying `no sessions spawned`, stop success, no create-then-edit — with
  // matching Reporting + Edge-cases wording, the core deliverable.
  // Bumped 32768 → 33792 for BOS-303: a Safety-rails "a PR number alone is
  // never completion or merge-readiness" rail plus an "Empty draft PR
  // placeholder" Edge-cases bullet (adopt the existing branch/session and
  // continue, distinct from the settled BOS-179 no-op) — the empty-placeholder
  // guidance addition, trimmed to one KiB above actual.
  // Bumped 33792 → 34816 for BOS-456 (epic BOS-449 capstone): the classify
  // snippet now resolves the tracker's CONFIGURED planned-state name from
  // .boss-skills.json and passes it to classifyTickets(tickets, plannedState),
  // so the last hard-coded `Todo` state literal leaves the published boss-epic
  // core (bs-epic-lib.mjs) and knownIdentityLeaks reaches empty — the epic's
  // core deliverable; the added resolver is the minimal viable de-identification.
  // Bumped 34816 → 35840 for BOS-470: Phase 3 now adopts one-shot GitHub callbacks
  // — a "Callback notifier" adapter seam (resolveCallbackAdapter, watchTriggers
  // checks_passed/checks_failed/merged armed as a per-child group) plus a Phase 3b
  // note that a callback wake trims the poll cadence but still routes through
  // authoritative reconciliation (dedup by id, re-arm while in flight, poll as
  // bounded fallback) — the core deliverable, trimmed to one KiB above actual.
  // BOS-495: bumped 35840 → 36864 for the up-front "prefer a callback over blind
  // polling" reflex (Operating Contract) + the single `callbacksAvailable(env)`
  // gate threaded through the seam bullet and Phase 3b. The full arm/reconcile/
  // re-arm/cleanup protocol moved OUT to references/callback-watches.md (parity
  // with boss-build), which offsets most of the growth; the residual is the
  // reflex + gate, which must not be shrunk to sneak under — re-baselined to one
  // KiB above actual per the ratchet convention.
  // BOS-520: bumped 36864 → 37888 for the Phase 3c frozen-repair-lease escape.
  // The old rule ("a repairer holds the lease → count a round and re-poll") had
  // no terminating condition against a stuck lease + dead repair chat, so the
  // driver re-polled forever. Phase 3c now classifies the lease with
  // classifyRepairLease and routes 'stalled' to exhausted-round → next round or
  // fail-isolate, with the two driver-state fields it needs documented in the
  // Phase 3 state shape — the core deliverable. Re-baselined to one KiB above
  // actual per the ratchet convention.
  // BOS-522: bumped 37888 → 40960 for the merge-gate hardening. The published
  // merge gate was chat-settled + Passing + real changes; a production burn
  // merged a partial-slice DRAFT on green + idle alone, so the resident gate now
  // enumerates all five conditions (daemon gate, settled chat, non-draft, ticket
  // in the review state, no do-not-merge marker) — a list that cannot be shortened
  // without re-opening the burn. Alongside it: the infra-death wake-to-resume
  // state (Phase 3c) and the merge-commit-deadlock / stranded-merge edge cases,
  // each kept to its decision rule. The rationale, diagnosis cheat sheet, and
  // recovery recipes moved OUT to references/merge-recovery.md, which absorbs the
  // bulk. Re-baselined to one KiB above actual per the ratchet convention.
  // BOS-523: bumped 40960 → 44032 for the draft-aware callback policy + the
  // session-hosted wait recipe. Three residents that cannot be deferred to a
  // reference: the never-bare-`checks_passed` rule (the trigger a driver arms is
  // decided at Phase 3b, not while reading a reference), the two-mechanism wait
  // (callbacks primary + session cron; backgrounded watchers stall SILENTLY), and
  // the Phase 3d drift note. The trigger table, the why-bare-is-wrong rationale,
  // the full wait recipe and its cron caveats moved OUT to
  // references/callback-watches.md, which absorbs the bulk. Re-baselined to one
  // KiB above actual per the ratchet convention.
  // BOS-524: bumped 44032 → 45056 for adapter-first state resolution in Phase 0. The
  // planned state was resolved from `.boss-skills.json` ALONE, so a repo fully
  // functional through a vendored tracker adapter that already knows its own states
  // self-BLOCKed for a value the adapter was holding. Phase 0 now probes the adapter's
  // optional `states` capability first, falls back to the config walk, resolves both
  // the planned and the review role through the one `resolveStateRole` helper so they
  // cannot diverge, and BLOCKs only when BOTH sources come back empty — naming both in
  // the message. Re-baselined to one KiB above actual per the ratchet convention.
  // BOS-614 extends BOS-605's resident callback contract with scoped target selection.
  // Re-baselined to one KiB above actual per the ratchet convention.
  // BOS-663 adds the Tier-1 path-load clause to the notes-extension dispatch (+310 bytes): the
  // notes extension declares `disable-model-invocation: true`, so a Skill-tool load by descriptor
  // `name` is refused and the layer goes inert silently. Re-baselined 47104 → 48128.
  // BOS-825 rewrites Phase 0 step 3 as a transport preflight (+1706 bytes): step 3 now
  // validates MCP *or* the boss CLI via bossEpicTransportPreflight and BLOCKs only when
  // neither carrier is complete, step 4 gains the CLI-mode $BOSS_REPO_ID rule, and the
  // session-adapter bullet documents the per-capability `cli` transport. Re-baselined
  // 48128 → 50176.
  // BOS-827 adds the `partial` report to Phase 0's opening-line contract (+186 bytes net,
  // after trimming the paragraph it extends): a CLI-transport run must name the capabilities
  // the CLI carries INCOMPLETELY (today getSession, whose missing routing signals are hang
  // detection, auth-death and conflict-after-green), not only the cli: null ones. The same
  // edit corrects the now-false claim that a managed spawn always wires the boss MCP server.
  // Re-baselined 50176 → 50362.
  // BOS-804 wires the shared tracker preflight into Phase 0 step 2 (+1761 bytes): the step now
  // re-resolves $BOSS_EPIC_TOOLBOX in its own block, best-effort gathers the chat's resolved MCP
  // servers, classifies the cheap selectPlanned read with trackerMcpPreflight, and stops
  // BLOCKED: <message> naming which of the two opposite fixes applies — replacing a bare
  // "tracker MCP unreachable" that sent the reader to fix a correct declaration. Re-baselined
  // 50362 → 52224.
  // BOS-842 gives merge-eligibility condition (5) its first producer (+421 bytes): the
  // do-not-merge marker had no writer anywhere in the suite, so the condition was prose with
  // nothing behind it. A child build run that ends in the new `PARTIAL` terminal state now
  // writes it, and both condition-(5) sites quote the marker literal verbatim so the gate
  // reads the same bytes boss-build's `references/review-stack.md` specifies — a paraphrase on
  // either side silently unlatches the hold. Resident because the driver decides the merge at
  // the rail, not while reading a reference. Re-baselined 52224 → 53248.
  // BOS-788 routes the `BOSS` resolution through `resolveBossBinary` (+2098 bytes): the old
  // paragraph named the right candidate ORDER but never tested that the preferred one exists,
  // so a cron session's inherited-then-reaped $BOSS_BIN short-circuited the chain and every
  // call-site died with an opaque ENOENT a run read as "capability unavailable". The block now
  // stats each candidate and states the two-way branch — resolved `path`, or
  // `BLOCKED: boss CLI unavailable — <reason>` quoting the resolver verbatim. Resident, not a
  // reference: this is Phase 0 preflight and every later call-site depends on `"$BOSS"`, and
  // the unavailable branch is only obeyed if the agent reads it at the rail. Re-baselined
  // 53248 → 55296.
  // BOS-800 repairs the two Phase 3 rails that were built on a misread status value
  // (+799 bytes). Phase 3b previously had NO push oracle at all and the driver inferred a
  // push from a `get_session` state transition — which fires when the daemon re-polls checks
  // that already exist, so on one run it fired once per child while every branch still held
  // only its bootstrap commit. Phase 3c defined settled as `last_output_at` staleness, and
  // that value collided to the nanosecond across every chat for a dozen cycles, so the test
  // as written could never pass and a green child would be held forever. Both replacements
  // are resident for the same reason condition (5) is: the driver decides push-vs-not and
  // settled-vs-not AT the poll rail, and a rail it has to leave the file to read is a rail it
  // does not apply. Re-baselined 55296 → 56320.
  // BOS-768 re-pins 56320 → 56253, the exact measured body: the old rounded ceiling
  // carried 67 bytes of unbanked slack. Re-measured 2026-08-19.
  // Rebased onto main at 5978bd850: #2090 rewrote shell positionals out of the published
  // bodies, growing this one by 13 B. That is a correctness rewrite of code the body must
  // carry, not new prose, so the pin absorbs exactly it. Re-pinned 56253 → 56266, the exact
  // measured body, re-measured 2026-08-19 (BOS-768).
  // BOS-908 banks 56266 → 56261 by tightening the getSession CLI partial prose.
  // BOS-755 re-baselines 56261 → 57368 for resident epic orchestration rails: repair terminal-line
  // outcome classification, remote-head push/no-work disambiguation, external-blocker best-case
  // accounting (including the cannot-evaluate-here non-mergeable bucket), and the fail-isolated
  // teardown exception. Re-measured 2026-08-22.
  // BOS-752 banks 57368 → 57353 by replacing the stale per-child callback group contract with the
  // per-trigger watch contract; detailed mechanics remain in callback-watches.md.
  // BOS-1013 banks 57353 → 57204 while naming `boss env --json` as the transport inventory
  // source; adjacent outcome prose was tightened so the resident body still shrinks.
  // BOS-997 re-baselines 57204 → 59107 for the resident Phase 3b/3c liveness-routing
  // rail: the body now calls `classifyChildLiveness`, enumerates WAITING/UNSPECIFIED,
  // names the usage-limit resume lane, and forbids unknown/wall-clock repair. The
  // expanded diagnosis stays in merge-recovery.md.
  // BOS-1030 banks 59107 → 59038 while naming Codex's fresh awaited notes-extension dispatch pair.
  const RATCHET = 59038

  // Eight separate gates in this file iterate EPIC_MIRRORS. A list that silently shortened
  // would leave every one of them asserting against a single mirror with nothing going red
  // to say the other stopped being checked — the vacuity assertArtifactSet exists for.
  assertArtifactSet(EPIC_MIRRORS, 2, 'EPIC_MIRRORS')

  assertExactSize({
    constFile: 'scripts/bs-epic-skill.test.mjs',
    constName: 'RATCHET',
    expected: RATCHET,
    label: 'boss-epic resident SKILL.md',
    measured: measureFile(abs('../services/boss/internal/skillinstall/skills/boss-epic/SKILL.md')),
    path: 'services/boss/internal/skillinstall/skills/boss-epic/SKILL.md',
    residual:
      'the references/ files the body routes to, and the plugin mirror — this pin measures the ' +
      'canonical skillinstall copy only',
  })
})

test('repair terminal-line outcome is read and classified (BOS-755)', () => {
  assert.ok(
    CLAUDE.includes("Read the repair skill's final terminal line from the tracked repair chat"),
    'boss-epic must read boss-repair terminal outcome',
  )
  assert.ok(
    CLAUDE.includes('Normalize the token by') &&
      CLAUDE.includes('stripping backticks, emphasis, any leading label'),
    'boss-epic must normalize the repair terminal token',
  )
  assert.ok(
    CLAUDE.includes('`inner-cap-exhausted`') && CLAUDE.includes('is not `no-progress`'),
    'boss-epic must branch inner-cap exhaustion away from no-progress',
  )
  assert.ok(
    CLAUDE.includes('`repair-unclassifiable`, counts') && CLAUDE.includes('is never success'),
    'boss-epic must count unclassifiable repair outcomes without treating them as success',
  )
})

test('child progress distinguishes unmoving head causes (BOS-755)', () => {
  assert.ok(
    CLAUDE.includes('An unmoving remote head means') &&
      CLAUDE.includes(
        'either the child produced no commit or it has local commits that never pushed',
      ),
    'boss-epic must name both causes of an unmoving remote head',
  )
  assert.ok(
    /establish\s+whether\s+a\s+push\s+happened\s+before\s+reading\s+the\s+head,\s+and\s+record\s+which\s+cause\s+was\s+observed/.test(
      CLAUDE,
    ),
    'boss-epic must establish push state first and record the observed cause',
  )
})

test('external blocker reporting includes best-case count and cannot-evaluate state (BOS-755)', () => {
  assert.ok(
    CLAUDE.includes('record the best-case merge') &&
      CLAUDE.includes(
        'eligible - uncleared-external-blocked - cannot-evaluate-here - cascade-skipped',
      ),
    'boss-epic must record the up-front best-case merge count',
  )
  assert.ok(
    CLAUDE.includes('reported as `cannot-evaluate-here`') &&
      CLAUDE.includes('distinct from both cleared and blocked'),
    'boss-epic must carry a third state for blockers outside this epic',
  )
})

test('teardown preserves fail-isolated live sessions in resident and callback reference (BOS-755)', () => {
  for (const [label, body] of [
    ['resident', CLAUDE],
    ['callback reference', CALLBACKS],
  ]) {
    assert.ok(
      body.includes('fail-isolated session is still live'),
      `${label} must name the fail-isolated live-session exception`,
    )
    assert.ok(
      body.includes('cron/callback') && /settled\s+child\s+watches/.test(body),
      `${label} must keep only needed cron/callback teardown and settled watches`,
    )
  }
})

test('frontmatter identifies the skill', () => {
  assert.match(CLAUDE, /^---\r?\nname: boss-epic\r?\n/, 'frontmatter must declare name: boss-epic')
})

test('session titles lead with the tracker ID', () => {
  for (const dir of EPIC_MIRRORS) {
    const skill = readFileSync(new URL(`${dir}/SKILL.md`, import.meta.url), 'utf8')
    assert.ok(
      skill.includes('`[<TICKET>] <ticket title>`'),
      `${dir}/SKILL.md must document ID-first child titles`,
    )
    assert.ok(
      skill.includes('title:  "[<TICKET>] <ticket title>"'),
      `${dir}/SKILL.md must launch ID-first child titles`,
    )
    assert.ok(
      skill.includes('title: "[<TICKET>] Repair (round N)"'),
      `${dir}/SKILL.md must use ID-first repair titles`,
    )
    assert.ok(
      !skill.includes('boss-epic <TICKET>: <ticket title>'),
      `${dir}/SKILL.md must not retain the old child title`,
    )
    assert.ok(
      !skill.includes('boss-epic repair <TICKET> (round N)'),
      `${dir}/SKILL.md must not retain the old repair title`,
    )
  }
})

test('contract tokens present in the canonical skill', () => {
  for (const body of [CLAUDE]) {
    for (const token of [
      'bs-epic-lib.mjs',
      'merge_session',
      'confirm',
      'get_session_statuses',
      'list_check_snapshots',
      'create_session',
      'send_chat_message',
      'transitiveDependents',
      'nextToMerge',
      'boss-epic-progress',
      '/boss-repair watch',
    ]) {
      assert.ok(body.includes(token), `missing token: ${token}`)
    }
  }
})

test('Phase 3 adopts one-shot callbacks with authoritative reconciliation (BOS-470)', () => {
  // The callback-notifier seam + the Phase-3b reconciliation contract: a callback
  // wake trims the poll cadence but never replaces the authoritative state read,
  // is deduped, re-armed while in flight, and degrades to the poll when callbacks
  // are unavailable. Pin the tokens so it can never regress to a naked poll.
  for (const token of [
    'resolveCallbackAdapter',
    'toolbox/callback/adapter.mjs',
    'boss callback',
    'policy.watchTriggers',
    'policy.fallbackPoll',
  ]) {
    assert.ok(CLAUDE.includes(token), `boss-epic SKILL.md must document callback token: ${token}`)
  }
  assert.match(
    CLAUDE,
    /callback\s+wake[\s\S]*reconcil/i,
    'a callback wake must route through authoritative reconciliation, not act on the trigger name',
  )
  // The two at-least-once safeguards must survive verbatim: a duplicate delivery is a
  // no-op (dedup by id) and a consumed one-shot watch is re-armed while work continues.
  assert.match(
    CLAUDE,
    /dedup\s+by\s+callback\s+id/i,
    'boss-epic must dedup callback deliveries by id (at-least-once delivery)',
  )
  assert.match(
    CLAUDE,
    /re-arm\s+the\s+child's\s+consumed\s+watches\s+while\s+it\s+is\s+still\s+in\s+flight/i,
    'boss-epic must re-arm the consumed one-shot watch while the child is still in flight',
  )
})

test('BOS-495: up-front callback reflex + callbacksAvailable gate + own reference (both mirrors)', () => {
  // The awareness fix: "prefer a callback over blind polling" is an up-front reflex
  // gated on the single `callbacksAvailable(env)` signal, present byte-identically in
  // BOTH Go mirrors, and boss-epic carries its own callback-watches reference framing
  // graceful degradation around the gate (skip registerWatch → fallbackPoll, never a
  // failed wait). Pin all three so the discoverability fix can never silently regress.
  for (const dir of EPIC_MIRRORS) {
    const skill = readFileSync(new URL(`${dir}/SKILL.md`, import.meta.url), 'utf8')
    // Up-front reflex: prefer a callback over blind polling, gated on callbacksAvailable.
    assert.match(
      skill,
      /Prefer\s+a\s+callback\s+over\s+blind\s+polling/i,
      `${dir}/SKILL.md must carry the up-front "prefer a callback over blind polling" reflex`,
    )
    assert.ok(
      skill.includes('callbacksAvailable'),
      `${dir}/SKILL.md must gate callback arming on callbacksAvailable`,
    )
    // The body points at boss-epic's own callback reference (moved out of Phase 3b).
    assert.match(
      skill,
      /references\/callback-watches\.md/,
      `${dir}/SKILL.md must point at its own references/callback-watches.md`,
    )

    // The reference exists and frames degradation around the gate.
    const ref = readFileSync(
      new URL(`${dir}/references/callback-watches.md`, import.meta.url),
      'utf8',
    )
    assert.ok(
      ref.includes('callbacksAvailable'),
      `${dir}/references/callback-watches.md must name the callbacksAvailable gate`,
    )
    assert.match(
      ref,
      /skip `registerWatch`/,
      `${dir}/references/callback-watches.md must say gate false ⇒ skip registerWatch`,
    )
    assert.match(
      ref,
      /Graceful\s+degradation\s+gated\s+on `callbacksAvailable`/,
      `${dir}/references/callback-watches.md must frame degradation around the gate`,
    )
    assert.match(
      ref,
      /gh\s+pr\s+checks .*--watch --fail-fast/,
      `${dir}/references/callback-watches.md must keep the bounded fallback poll`,
    )
    // Published-core invariant: no host-specific tracker/MCP identity leaks in.
    assert.doesNotMatch(
      ref,
      /bossanova-(linear|sentry)/,
      `${dir}/references/callback-watches.md must stay project-agnostic (no project MCP names)`,
    )
  }

  // The "byte-identical in BOTH Go mirrors" claim above is only real if something
  // diffs the two copies: the per-mirror phrase checks pass even if one mirror drifts
  // in whitespace/wording or a `make copy-skills` is skipped. Enforce it directly.
  const [canonicalDir, pluginDir] = EPIC_MIRRORS
  for (const rel of ['SKILL.md', 'references/callback-watches.md']) {
    assert.equal(
      read(`${pluginDir}/${rel}`),
      read(`${canonicalDir}/${rel}`),
      `${pluginDir}/${rel} must be byte-identical to the canonical mirror (run \`make copy-skills\`)`,
    )
  }
})

test('BOS-614: callbacks use a verified scoped target and safe cleanup (both mirrors)', () => {
  // A managed but unrelated chat is never an eligible callback target. The
  // selector prefers a repository-matching orchestrator and only then a
  // repository-matching child. Without either, callbacks are entirely skipped
  // while the existing cron/poll reconciliation remains active.
  for (const dir of EPIC_MIRRORS) {
    const skill = readFileSync(new URL(`${dir}/SKILL.md`, import.meta.url), 'utf8')
    const ref = readFileSync(
      new URL(`${dir}/references/callback-watches.md`, import.meta.url),
      'utf8',
    )
    const prose = `${skill}\n${ref}`.replace(/\s+/g, ' ')

    assert.ok(
      prose.includes('selectEpicCallbackTarget'),
      `${dir} must resolve callbacks through selectEpicCallbackTarget`,
    )
    assert.match(
      prose,
      /matching\s+orchestrator.*otherwise.*matching\s+child/i,
      `${dir} must prefer a matching orchestrator and fall back to a matching child`,
    )
    assert.match(
      prose,
      /repository.*exactly.*child\s+PR\s+repository/i,
      `${dir} must reject a cross-repository orchestrator target`,
    )
    assert.match(
      prose,
      /no\s+verified\s+target.*skip.*registration.*re-arm.*list.*cleanup/i,
      `${dir} must skip every callback operation when no target is verified`,
    )
    assert.match(
      prose,
      /cron\/poll\s+reconciliation/i,
      `${dir} must retain cron/poll reconciliation when callbacks are skipped`,
    )

    // The target selector runs in Node, so the shell must receive a concrete chat
    // value from a JSON bridge. A JavaScript property expression in Bash (the old
    // `CALLBACK_CHAT="$callbackTarget.chatId"`) is invalid and must never return.
    assert.doesNotMatch(
      ref,
      /CALLBACK_CHAT="\$callbackTarget\.chatId"/,
      `${dir} must not assign a JavaScript property expression in Bash`,
    )
    assert.match(
      ref,
      /const \{ selectEpicCallbackTarget \} = await[ ]import\(`\$\{process\.env\.BOSS_EPIC_TOOLBOX\}\/callback\/epic-target\.mjs`\)/,
      `${dir} bridge must dynamically import the installed selector`,
    )
    for (const envName of [
      'CHILD_PR_REPOSITORY',
      'ORCHESTRATOR_CHAT',
      'ORCHESTRATOR_REPOSITORY',
      'CHILD_CHAT',
      'CHILD_REPOSITORY',
    ]) {
      assert.ok(
        ref.includes(envName),
        `${dir} bridge must accept explicitly verified ${envName} identity`,
      )
    }
    assert.match(
      ref,
      /CALLBACK_TARGET_JSON="\$\(/,
      `${dir} bridge must capture the selector result as JSON`,
    )
    assert.match(
      ref,
      /process\.stdout\.write\(JSON\.stringify\(callbackTarget\)\)/,
      `${dir} bridge must serialize the selector result safely`,
    )
    assert.match(
      ref,
      /JSON\.parse\(process\.env\.CALLBACK_TARGET_JSON \?\? "null"\)/,
      `${dir} bridge must read the selector JSON before assigning Bash`,
    )
    assert.match(
      ref,
      /process\.stdout\.write\(typeof[ ]target\?\.chatId === "string" \? target\.chatId : ""\)/,
      `${dir} bridge must emit the selected chat or empty output`,
    )
    assert.match(
      ref,
      /if \[ -z "\$CALLBACK_CHAT" \]; then\s+echo "No\s+verified\s+callback\s+target; retain\s+cron\/poll\s+reconciliation\. Continue\s+to\s+Phase\s+3b\s+reconciliation\s+and\s+the\s+bounded\s+poll\/session\s+cron\."/,
      `${dir} bridge must make the no-target cron/poll fallback explicit`,
    )
    assert.doesNotMatch(
      ref,
      /if \[ -z "\$CALLBACK_CHAT" \]; then[\s\S]{0,240}\bcontinue\b/,
      `${dir} no-target branch must fall through to reconciliation, not continue the child loop`,
    )
    assert.match(
      ref,
      /No[ ]verified[ ]callback[ ]target; retain[ ]cron\/poll[ ]reconciliation\. Continue[ ]to[ ]Phase[ ]3b[ ]reconciliation[ ]and[ ]the[ ]bounded[ ]poll\/session[ ]cron\./,
      `${dir} no-target branch must retain mandatory reconciliation and fallback polling`,
    )

    // The selected repo is normalized by the target selector. Every fenced block
    // that consumes the value is a fresh shell, so each block derives CALLBACK_REPO
    // from CALLBACK_TARGET_JSON before calling the callback CLI.
    const repoDerivations = ref.match(/CALLBACK_REPO="\$\([\s\S]{0,300}target\?\.repo/g) ?? []
    assert.equal(
      repoDerivations.length,
      3,
      `${dir} must derive CALLBACK_REPO in each register, re-arm/list, and cleanup block`,
    )
    assert.match(
      ref,
      /process\.stdout\.write\(typeof[ ]target\?\.repo === "string" \? target\.repo : ""\)/,
      `${dir} bridge must emit the selected normalized repository or empty output`,
    )

    // Register, re-arm, and reconciliation list calls carry both scopes.
    assert.match(
      ref,
      /boss[ ]callback[ ]add[^\n]*--chat "\$CALLBACK_CHAT"[^\n]*--repo "\$CALLBACK_REPO"/,
      `${dir} register example must scope --chat and selected --repo`,
    )
    assert.match(
      ref,
      /boss[ ]callback[ ]list[^\n]*--chat "\$CALLBACK_CHAT"[^\n]*--repo "\$CALLBACK_REPO"/,
      `${dir} list/re-arm example must scope --chat and selected --repo`,
    )
    assert.match(
      ref.replace(/\s+/g, ' '),
      /`registerWatch`\s+and\s+`listWatches`\s+require\s+both\s+`--chat "\$CALLBACK_CHAT"`\s+and\s+`--repo "\$CALLBACK_REPO"`/,
      `${dir} capability contract prose must require the selected callback repo`,
    )
    assert.doesNotMatch(
      ref,
      /--repo "\$CHILD_PR_REPOSITORY"/,
      `${dir} must not pass the raw child PR repository as a callback --repo argument`,
    )
    for (const command of ['add', 'list', 'remove']) {
      assert.match(
        ref,
        new RegExp(
          `if \\[ -n "\\$CALLBACK_CHAT" \\](?: && \\[ -n "\\$CALLBACK_REPO" \\])?; then[\\s\\S]*?boss callback ${command}[\\s\\S]*?fi`,
        ),
        `${dir} callback ${command} commands must be guarded by a verified chat and repo when repo is required`,
      )
    }

    // The generic CLI intentionally accepts --chat but not --repo for remove.
    // Cleanup must discover ids via the prior scoped list, then remove by id.
    assert.match(
      ref,
      /boss[ ]callback[ ]remove "\$CALLBACK_ID" --chat "\$CALLBACK_CHAT"/,
      `${dir} cleanup must remove each returned id with --chat`,
    )
    assert.doesNotMatch(
      ref,
      /boss\s+callback\s+remove[^\n]*--repo/,
      `${dir} cleanup must not invent unsupported callback remove --repo`,
    )
  }
})

test('BOS-752: epic callback waits use per-trigger groups, guarded re-arm, and three-way reconcile (both mirrors)', () => {
  for (const dir of EPIC_MIRRORS) {
    const skill = readFileSync(new URL(`${dir}/SKILL.md`, import.meta.url), 'utf8')
    const ref = readFileSync(
      new URL(`${dir}/references/callback-watches.md`, import.meta.url),
      'utf8',
    )
    const prose = `${skill}\n${ref}`.replace(/\s+/g, ' ')

    assert.match(
      skill,
      /armed\s+under\s+per-trigger\s+groups/i,
      `${dir}/SKILL.md must say child PR callback triggers use per-trigger groups`,
    )
    assert.doesNotMatch(
      skill,
      /one-shot\s+callback\s+group/i,
      `${dir}/SKILL.md must not describe the child PR wait as one shared callback group`,
    )
    assert.match(
      ref,
      /--group "epicwait-\$PR-\$T"/,
      `${dir}/references/callback-watches.md must arm each trigger with a per-trigger group`,
    )
    assert.doesNotMatch(
      ref,
      /--group "epicwait-\$PR"(?!-\$T)/,
      `${dir}/references/callback-watches.md must not use one shared epicwait group`,
    )
    assert.match(
      prose,
      /group\s+(?:two\s+)?triggers\s+only\s+when\s+at\s+most\s+one\s+of\s+them\s+can\s+ever\s+be\s+satisfied/i,
      `${dir} must state the mutual-exclusivity rule for callback groups`,
    )

    for (const outcome of ['ready', 'not-yet', 'could-not-evaluate']) {
      assert.match(
        ref,
        new RegExp(`\\b${outcome}\\b`),
        `${dir}/references/callback-watches.md must define the ${outcome} reconcile outcome`,
      )
    }
    assert.match(
      ref,
      /mergeStateStatus\s*==\s*"CLEAN"/,
      `${dir}/references/callback-watches.md ready outcome must require mergeStateStatus == "CLEAN"`,
    )
    assert.match(
      ref,
      /check\s+count\s+of\s+zero\s+is\s+not\s+a\s+pass/i,
      `${dir}/references/callback-watches.md must say an empty rollup is not green`,
    )
    assert.match(
      ref,
      /empty\s+commit\s+that\s+skips\s+CI/i,
      `${dir}/references/callback-watches.md must name the project-agnostic vacuous-green motivation`,
    )

    assert.match(
      ref,
      /Re-arm\s+a\s+trigger\s+only\s+when[\s\S]{0,180}condition\s+as\s+\*\*false\*\*/i,
      `${dir}/references/callback-watches.md must guard re-arm on a false condition`,
    )
    assert.match(
      ref,
      /record\s+the\s+skip\s+by\s+name/i,
      `${dir}/references/callback-watches.md must record skipped re-arm triggers by name`,
    )
    assert.match(
      ref,
      /could-not-evaluate[\s\S]{0,80}arm\s+nothing/i,
      `${dir}/references/callback-watches.md must arm nothing when reconcile could not evaluate`,
    )
  }
})

test('BOS-524: Phase 0 resolves states ADAPTER-FIRST with the config as fallback (both mirrors)', () => {
  // The bug this pins: Phase 0 read the planned state from `.boss-skills.json`
  // ALONE, so a repo fully functional through a vendored tracker adapter that
  // already knows its own states self-BLOCKed for a value the adapter was
  // holding. The order — adapter probe, THEN the config walk, THEN fail closed —
  // is the acceptance criterion, so pin it positionally (the probe must appear
  // BEFORE the `.boss-skills.json` read) rather than merely pinning both tokens.
  for (const dir of EPIC_MIRRORS) {
    const skill = readFileSync(new URL(`${dir}/SKILL.md`, import.meta.url), 'utf8')

    // 1. The adapter probe: the optional `states` capability read through the
    //    tracker CLI, tolerant of an adapter that does not implement it.
    const probe = skill.indexOf('BOSS_EPIC_TOOLBOX/tracker/cli.mjs" states')
    assert.ok(probe > -1, `${dir}/SKILL.md must probe the tracker adapter's states capability`)
    assert.match(
      skill,
      /cli\.mjs" states[ ]2>\/dev\/null \|\| true/,
      `${dir}/SKILL.md must tolerate an adapter with no states capability (exit 2 ⇒ empty probe)`,
    )

    // 2. The fallback: the `.boss-skills.json` upward walk, strictly AFTER the probe.
    const fallback = skill.indexOf('trackerConfig?.[a]?.states')
    assert.ok(fallback > -1, `${dir}/SKILL.md must keep the trackerConfig walk as the fallback`)
    assert.ok(
      probe < fallback,
      `${dir}/SKILL.md must probe the adapter BEFORE reading trackerConfig (adapter is primary)`,
    )

    // 3. One helper for BOTH roles, so eligibility and the merge gate cannot diverge.
    assert.ok(
      skill.includes('resolveStateRole'),
      `${dir}/SKILL.md must resolve states through the toolbox helper, not an inline read`,
    )
    // The role travels as an env-assignment PREFIX, not a positional: a slash-command invocation
    // rewrites `$1` in a published skill body before any shell runs it (check-skill-shell header
    // rule (j)), which would have handed the helper an empty role at exit 0.
    for (const role of ['planned', 'inReview']) {
      assert.ok(
        skill.includes(`ROLE=${role} resolve_state`),
        `${dir}/SKILL.md must resolve the ${role} role through the shared helper`,
      )
    }
    assert.ok(
      !/ROLE="\$1"/.test(skill),
      `${dir}/SKILL.md must not take the role as a shell positional`,
    )

    // 4. Fail closed only when NEITHER source answers, naming BOTH probed sources
    //    so the operator knows which of the two to populate.
    const block = skill.match(/BLOCKED: no[ ]planned[ ]state[ ]resolved[^"]*/)
    assert.ok(block, `${dir}/SKILL.md must BLOCK with a "no planned state resolved" message`)
    assert.match(
      block[0],
      /adapter/i,
      `${dir}/SKILL.md BLOCK message must name the adapter as a probed source`,
    )
    assert.match(
      block[0],
      /trackerConfig\.<tracker>\.states\.planned/,
      `${dir}/SKILL.md BLOCK message must name the config key as the other probed source`,
    )
  }
})

test('zero-launch no-ready run has an explicit single-comment branch (BOS-302)', () => {
  // A no-ready/no-inflight run spawns zero sessions. The skill must define one
  // explicit branch: upsert exactly one progress comment carrying
  // `no sessions spawned` and stop success — never create-then-edit two comments.
  assert.match(
    CLAUDE,
    /zero-launch/,
    'the empty-eligible path must be named as an explicit zero-launch branch',
  )
  assert.match(
    CLAUDE,
    /no\s+sessions\s+spawned/,
    'the zero-launch branch must report `no sessions spawned`',
  )
  assert.match(
    CLAUDE,
    /upsert\s+exactly\s+one/i,
    'the zero-launch branch must upsert exactly one progress comment',
  )
  assert.match(
    CLAUDE,
    /stop\s+success/i,
    'the zero-launch branch must stop successfully (clean no-op, not an error)',
  )
  // Resume idempotence for the zero-launch comment still keys off the marker.
  assert.match(
    CLAUDE,
    /<!-- boss-epic-progress -->/,
    'the zero-launch branch must reuse the boss-epic-progress marker on resume',
  )
  // The single-comment contract must forbid the old create-then-edit two-comment
  // shape for the zero-launch case.
  assert.match(
    CLAUDE,
    /never\s+create-then-edits|no\s+separate\s+initial-then-final\s+edit/i,
    'the zero-launch branch must forbid create-then-edit of two comments',
  )
})

test('passing greens require settled child chat before merge eligibility', () => {
  assert.match(
    CLAUDE,
    // BOS-522 narrowed this transition to READY_FOR_REVIEW only — a GREEN_DRAFT
    // is now a hold (pinned below, in the merge-gate test).
    /READY_FOR_REVIEW \+ DisplayStatus\s+Passing \+ chat\s+SETTLED/,
    'passing greens must require a settled child chat',
  )
  assert.match(
    CLAUDE,
    /Passing\s+but\s+chat\s+still\s+WORKING\/QUESTION\/WAITING[^\n]*hold/i,
    'unsettled passing children must be held, not merged or repaired',
  )
  assert.match(
    CLAUDE,
    /`UNSPECIFIED`\s+or\s+an\s+unreadable\s+status\s+is\s+\*\*unknown\*\*,\s+not\s+settled\s+and\s+not\s+dead;\s+investigate\/re-poll/,
    'unreadable chat status must block merge eligibility',
  )
  assert.match(
    CLAUDE,
    /STOPPED \+\s+missing `last_agent_activity_at` = settled/,
    'stopped chats with no activity timestamp must not be held until wall-clock failure',
  )
  assert.doesNotMatch(
    CLAUDE,
    /GREEN_DRAFT\s+or\s+READY_FOR_REVIEW \+ DisplayStatus\s+Passing\*\*?[^+\n]*→ add\s+to\s+the\s+\*\*greens\*\*/,
    'the old Passing-only green transition must not return',
  )
})

test('BOS-997: child liveness classifier routes ambiguous states conservatively (both mirrors)', () => {
  for (const dir of EPIC_MIRRORS) {
    const skill = readFileSync(new URL(`${dir}/SKILL.md`, import.meta.url), 'utf8')
    const recovery = readFileSync(
      new URL(`${dir}/references/merge-recovery.md`, import.meta.url),
      'utf8',
    )
    const prose = skill.replace(/\s+/g, ' ')
    const recoveryProse = recovery.replace(/\s+/g, ' ')

    assert.match(
      prose,
      /Feed\s+only\s+these\s+already-read\s+facts\s+to\s+`classifyChildLiveness`/,
      `${dir}/SKILL.md must route child liveness through the pure classifier`,
    )
    for (const token of [
      '`agent-conclusion`',
      '`usage-limit`',
      '`transient-api-error`',
      '`headShaMoved`',
      '`attention_status` reasons',
      '{verdict, action, reasons}',
    ]) {
      assert.ok(skill.includes(token), `${dir}/SKILL.md classifier input missing ${token}`)
    }
    assert.match(
      prose,
      /An\s+unmoving\s+remote\s+head\s+is\s+never\s+admissible\s+as\s+evidence\s+of\s+child\s+death/,
      `${dir}/SKILL.md must forbid remote-head liveness reads`,
    )
    assert.match(
      prose,
      /`WORKING`\/`QUESTION`\/`WAITING`\s+=\s+still\s+alive/,
      `${dir}/SKILL.md must enumerate WAITING as alive`,
    )
    assert.match(
      prose,
      /`UNSPECIFIED`\s+or\s+an\s+unreadable\s+status\s+is\s+\*\*unknown\*\*,\s+not\s+settled\s+and\s+not\s+dead/,
      `${dir}/SKILL.md must route UNSPECIFIED/unreadable status to unknown`,
    )
    assert.match(
      prose,
      /Do\s+not\s+add\s+a\s+ticket\s+to\s+`greens`\s+while\s+that\s+chat\s+is\s+WORKING,\s+QUESTION,\s+WAITING,\s+LIMITED,\s+UNSPECIFIED,\s+or\s+unreadable/,
      `${dir}/SKILL.md must prohibit WAITING and unknown statuses from greens`,
    )
    for (const route of [
      '`alive/hold` → re-poll',
      '`environmental-death/resume` → one wake-to-resume',
      '`agent-blocked/repair` → repair round below',
    ]) {
      assert.ok(skill.includes(route), `${dir}/SKILL.md missing classifier route ${route}`)
    }
    assert.match(
      prose,
      /`unknown\/investigate`\s+→\s+read\s+the\s+tracked\s+chat's\s+last\s+message\s+and\s+`waiting_reason`/,
      `${dir}/SKILL.md must investigate unknown liveness before re-polling`,
    )
    assert.match(
      prose,
      /`wall-clock-expired\/fail-isolate`\s+→\s+fail-isolate\s+and\s+\*\*never\*\*\s+repair/,
      `${dir}/SKILL.md must fail-isolate wall-clock expiry without repair`,
    )
    assert.match(
      prose,
      /An\s+`unknown`\s+verdict\s+never\s+dispatches\s+repair/,
      `${dir}/SKILL.md must bar repair from unknown liveness`,
    )
    assert.match(
      prose,
      /A\s+`BLOCKED`\s+state\s+without\s+that\s+conclusion\s+is\s+`unknown\/investigate`,\s+not\s+repair/,
      `${dir}/SKILL.md must require agent conclusion before repairing BLOCKED`,
    )
    assert.match(
      prose,
      /the\s+tracked\s+chat\s+is\s+`LIMITED`,\s+the\s+chat's\s+last\s+message\s+is\s+a\s+\*\*usage-limit\*\*\s+banner/,
      `${dir}/SKILL.md must include the usage-limit resume lane`,
    )
    assert.match(
      prose,
      /a\s+429\/rate\s+cap\s+is\s+not\s+the\s+transient-API\s+detector's\s+5xx\s+lane/,
      `${dir}/SKILL.md must distinguish 429 from transient 5xx`,
    )
    assert.match(
      prose,
      /Wall-clock\s+expiry\s+is\s+a\s+budget\s+fact,\s+not\s+death\s+evidence,\s+and\s+can\s+never\s+route\s+to\s+repair/,
      `${dir}/SKILL.md must keep wall-clock expiry out of repair`,
    )
    assert.match(
      recoveryProse,
      /Usage\s+cap\s+\/\s+429:[\s\S]*resume\s+lane/i,
      `${dir}/merge-recovery.md must diagnose usage caps as resume`,
    )
    assert.match(
      recoveryProse,
      /Parked\s+on\s+a\s+background\s+agent:[\s\S]*alive\/hold/i,
      `${dir}/merge-recovery.md must diagnose parked children as alive`,
    )
    assert.match(
      recoveryProse,
      /Autocompact\s+or\s+dead-turn\s+chrome:[\s\S]*unknown\/investigate/i,
      `${dir}/merge-recovery.md must keep autocompact unclassifiable`,
    )
  }
})

test('planning-only work is routed away from PR-backed sessions', () => {
  assert.match(CLAUDE, /planning-only/i, 'skill must explicitly classify planning-only work')
  assert.match(
    CLAUDE,
    /subagent/i,
    'unattended planning fan-out should be routed to subagents, not sessions',
  )
  assert.match(
    CLAUDE,
    /quick_chat:\s*true/,
    'visible planning conversations should use quick_chat:true',
  )
  assert.match(
    CLAUDE,
    /must\s+not\s+use\s+`?create_session`?.*tmux_unattended/i,
    'planning-only work must not use PR-backed tmux_unattended sessions',
  )
})

test('planning-only routing names concrete capability boundaries', () => {
  // BOS-322: the three routing cases must be concrete and name the session-runner
  // adapter capabilities (createSession vs createPlanningChat), not prose-only
  // advice — so a planning subtask can never regress into the PR-backed
  // implementation path. Each case must live on a single line (capability + its
  // discriminating field co-located).
  assert.match(
    CLAUDE,
    /implementation\s+work\s+uses[^\n]*createSession[^\n]*tmux_unattended/i,
    'implementation work must name the createSession capability + tmux_unattended',
  )
  assert.match(
    CLAUDE,
    /unattended[^\n]*planning[^\n]*subagent/i,
    'unattended planning fan-out must route to a subagent',
  )
  assert.match(
    CLAUDE,
    /visible\s+planning\s+chat\s+uses[^\n]*createPlanningChat/i,
    'visible planning chat must name the createPlanningChat capability',
  )
  assert.doesNotMatch(
    CLAUDE,
    /planning[- ]only[^\n]*(?:use|via|route|through)[^\n]*create_session[^\n]*tmux_unattended/i,
    'planning-only work must never be routed to create_session + tmux_unattended',
  )
})

test('non-claude runners never fall back to merging without settlement', () => {
  assert.doesNotMatch(
    CLAUDE,
    /still\s+schedules\/merges\s+greens/,
    'runner fallback must not claim it can merge greens without chat settlement',
  )
  assert.match(
    CLAUDE,
    /runner\s+without\s+readable\s+chat\s+status\s+must\s+hold\s+or\s+fail-isolate/i,
    'non-claude fallback must preserve the settled-green gate',
  )
})

test('default agent is claude', () => {
  for (const body of [CLAUDE]) {
    assert.ok(body.includes('--agent claude'), 'missing default `--agent claude`')
  }
})

test('safety pins', () => {
  // at most one merge in flight at a time (serialized merges)
  assert.match(CLAUDE, /at\s+most\s+one\s+merge\s+in\s+flight/i)
  // repair/dispatch does not tear down an in-progress session
  assert.match(CLAUDE, /leave\s+the\s+session\s+open/i)
  // the epic set boundary is a hard guarantee, never crossed
  assert.match(CLAUDE, /outside\s+the\s+epic\s+set\s+are\s+NEVER\s+mutated/i)
  // headless unattended discipline: no interactive prompts past preflight
  assert.match(CLAUDE, /never\s+call\s+AskUserQuestion\s+after\s+Phase\s+0/i)
  // wiring contract: done siblings clear via externallyCleared, never merged
  assert.match(CLAUDE, /`done` ids\s+are\s+folded\s+into\s+`externallyCleared`/)
  // negative pin: the done-bucket-into-merged misstatement must never return
  // (backticked `done` = the classifyTickets bucket; the plain-prose "Done"
  // state name legitimately appears near `merged` on the 3d success path)
  assert.doesNotMatch(CLAUDE, /`done`[^\n]*into `merged`/)
  // loop termination: greens keep their concurrency slot until merged
  assert.match(CLAUDE, /`greens` is\s+a \*\*subset\*\*\s+of `inFlight`/)
})

test('no stub or placeholder prose in the claude source', () => {
  // Case-sensitive: the Linear status name `Todo` appears legitimately
  // throughout (e.g. "eligible: `Todo` + agent-friendly"); the stub-marker
  // convention this guards against is an all-caps `TODO` note.
  assert.doesNotMatch(CLAUDE, /\bTODO\b/, 'CLAUDE SKILL.md must not contain TODO markers')
  // BOS-303 domain-term exception: "empty draft PR placeholder" (and its
  // shorter "empty draft placeholder" form) is legitimate guidance — an empty
  // bootstrap draft PR the child boss-build adopts and continues. A negative
  // lookbehind carves out exactly the "draft placeholder" / "draft PR
  // placeholder" domain phrase while keeping the guard strict against every
  // other use: bare `placeholder`, adjective-filler ("placeholder
  // implementation/value/text/…"), a `<placeholder>` template token, and an
  // all-caps `PLACEHOLDER` stub marker (the `i` flag covers case).
  assert.doesNotMatch(
    CLAUDE,
    /(?<!draft (?:PR )?)\bplaceholder\b/i,
    'CLAUDE SKILL.md must not contain placeholder stub/filler prose (only the "draft [PR] placeholder" domain term is allowed)',
  )
})

test('empty draft PR placeholder is adopt-and-continue, not completion (BOS-303)', () => {
  // A draft PR with no real changes and no check evidence, appearing before a
  // settled run, must route the child boss-build to continue from existing
  // branch/session state — never restart planning, never count the PR as done,
  // and distinct from the settled BOS-179 no-op that fail-isolates.
  assert.match(
    CLAUDE,
    /empty\s+draft\s+PR\s+placeholder/,
    'skill must name the empty draft PR placeholder case',
  )
  assert.match(
    CLAUDE,
    /adopt(?:s)? the\s+existing\s+branch\/session/i,
    'the placeholder case must adopt the existing branch/session and continue',
  )
  assert.match(
    CLAUDE,
    /a\s+PR\s+number\s+alone\s+is (?:never|not)[^\n]*(?:completion|merge)/i,
    'skill must state a PR number alone is not completion/merge-readiness',
  )
})

test('phase 0 runs a deterministic boss MCP tool-discovery preflight (BOS-301)', () => {
  // Phase 0 must prove every required boss MCP tool is discoverable before
  // scheduling, derived from the session-adapter source of truth, and fail fast
  // naming the absent tools. list_check_snapshots (the historically missed tool)
  // is reachable through that derived checklist.
  assert.match(
    CLAUDE,
    /requiredBossToolsForEpic/,
    'preflight must derive the required tool list from requiredBossToolsForEpic (source of truth)',
  )
  // BOS-825 widened the preflight from MCP-only to MCP-or-CLI, so the diagnostic moved
  // from "missing required tools" to the transport-aware BLOCK. Pin the *new* clause —
  // the fail-fast-naming-what-is-absent intent is unchanged, only its carrier is.
  assert.match(
    CLAUDE,
    /bossEpicTransportPreflight/,
    'preflight must choose the carrier through bossEpicTransportPreflight',
  )
  assert.match(
    CLAUDE,
    /no\s+complete\s+boss\s+transport/i,
    'neither transport complete must produce a concise diagnostic naming what is missing',
  )
  assert.match(
    CLAUDE,
    /before\s+scheduling/i,
    'the tool-discovery preflight runs before scheduling',
  )
})

test('BOS-458: the published core carries no hard-coded ${TRACKER:-…} shell default', () => {
  // Direct regression guard for the BOS-458 fix. The bug was a shell `${TRACKER:-linear}`
  // literal in the planned-state diagnostic that baked `linear` in when the TRACKER env was
  // unset — misnaming the config key for a non-linear repo. The runtime resolution already
  // honors adapters.tracker in JS (`process.env.TRACKER || c.adapters?.tracker || "linear"`);
  // the diagnostic now uses a generic `<tracker>` placeholder. Assert the shell
  // `${TRACKER:-<default>}` form is absent so it cannot be reintroduced into this published
  // core (the Go identity-leak guard permits `linear` as the default adapter and misses it).
  assert.equal(
    CLAUDE.split('${TRACKER:-').length - 1,
    0,
    'boss-epic SKILL.md must not hard-code a ${TRACKER:-<default>} shell fallback; resolve the tracker from adapters.tracker instead',
  )
})

test('BOS-520: Phase 3c has a TERMINATING frozen-repair-lease escape (both mirrors)', () => {
  // The bug this pins shut: Phase 3c's "a repairer holds the lease → count a
  // round and re-poll" rule had no terminating condition. Against a stuck lease
  // + dead repair chat (repair_active:true, last_repair_head_sha frozen, repair
  // chat silent) the driver re-polled forever — unable to help, unable to fail.
  // Phase 3c must now classify the lease and route 'stalled' to an exhausted
  // round → next round or fail-isolate, so the loop always terminates. Pinned in
  // BOTH Go mirrors so a partial copy-skills edit trips this gate.
  for (const dir of EPIC_MIRRORS) {
    const skill = readFileSync(new URL(`${dir}/SKILL.md`, import.meta.url), 'utf8')
    // Prose assertions run against a whitespace-collapsed copy: prettier re-wraps
    // these paragraphs on every edit, and a re-wrap must not trip a content gate.
    const prose = skill.replace(/\s+/g, ' ')

    // The classifier is the seam — Phase 3c reads it, never a bare repair_active.
    assert.ok(
      skill.includes('classifyRepairLease'),
      `${dir}/SKILL.md Phase 3c must classify the lease via classifyRepairLease`,
    )
    // Its three inputs beyond repair_active, including the older-daemon evidence path.
    for (const token of ['repair_stalled_at', 'last_repair_head_sha', 'prevLastRepairHeadSha']) {
      assert.ok(skill.includes(token), `${dir}/SKILL.md must feed the classifier from ${token}`)
    }
    // The terminating outcome: a stalled lease is an exhausted round, and at the
    // cap the ticket is fail-isolated with a named note — never re-polled.
    assert.match(
      skill,
      /frozen\s+repair\s+lease/,
      `${dir}/SKILL.md must name the frozen repair lease fail-isolate note`,
    )
    // The human-readable diagnosis cheat: the three-signal shape of a dead lease,
    // so an operator reading a stuck run recognises it without the classifier.
    assert.match(
      prose,
      /`last_repair_head_sha` unchanged\s+and\s+repair-chat\s+output\s+stale\s+across ≥2\s?polls/,
      `${dir}/SKILL.md must cross-reference the dead-lease diagnosis cheat`,
    )
    assert.match(
      prose,
      /Never\s+re-poll\s+a\s+lease\s+already\s+classified `stalled`/,
      `${dir}/SKILL.md must forbid re-polling a lease already classified stalled`,
    )
    assert.match(
      skill,
      /terminate/i,
      `${dir}/SKILL.md must state that the stalled path terminates the loop`,
    )

    // Driver state additions are documented in the Phase 3 state shape, not just
    // used in 3c — a driver that does not carry them cannot compute 'stalled'.
    assert.ok(
      skill.includes('repairStallSince'),
      `${dir}/SKILL.md Phase 3 state shape must document repairStallSince`,
    )
  }
})

test('BOS-522: full merge-eligibility gate + merge-recovery + infra-death (both mirrors)', () => {
  // Four contract gaps this pins shut, all reached through the same merge gate:
  //   (a) the published gate was chat-settled + Passing + real changes, so a
  //       partial-slice DRAFT merged on green + idle alone in production;
  //   (b) the rebase-strategy merge-commit deadlock had no diagnosis/recovery, so
  //       a driver retried a doomed merge_session forever;
  //   (c) a mid-run merge_session error whose PR had actually merged was treated
  //       as a failure — re-merged or fail-isolated live, landed work;
  //   (d) a chat killed by a transient API 5xx fit neither BLOCKED nor green, and
  //       the standing nudge ban made a strict driver fail-isolate live work.
  // Pinned in BOTH Go mirrors so a partial copy-skills edit trips this gate.
  for (const dir of EPIC_MIRRORS) {
    const skill = readFileSync(new URL(`${dir}/SKILL.md`, import.meta.url), 'utf8')
    // Prose assertions run against a whitespace-collapsed copy: prettier re-wraps
    // these paragraphs on every edit, and a re-wrap must not trip a content gate.
    const prose = skill.replace(/\s+/g, ' ')

    // (a) All five gate conditions are resident, not just the old three. Each is
    // asserted separately so dropping one cannot hide behind the others.
    assert.match(
      prose,
      /merge-eligible\s+only\s+when\s+all\s+five\s+hold/i,
      `${dir}/SKILL.md must state the five-condition merge gate`,
    )
    for (const [label, re] of [
      [
        'daemon gate authoritative',
        /`gate\s+1` \/ DisplayStatus `Passing` — the\s+daemon\s+gate\s+is\s+authoritative/,
      ],
      ['settled chat', /the\s+build\s+chat\s+has\s+SETTLED/],
      ['non-draft PR', /the\s+PR\s+is \*\*not\s+a\s+draft\*\*/],
      // Resolved from the configured state ROLE — a literal state name here would
      // be workspace-specific and fail the published-core project-agnostic gate.
      [
        'ticket in the review state',
        /\*\*review\*\* state \(resolved\s+from[\s\S]*?`states\.inReview`/,
      ],
      ['no do-not-merge marker', /no\*\* partial-slice\s+or `do\s+not\s+merge` marker/],
    ]) {
      assert.match(prose, re, `${dir}/SKILL.md merge gate must require: ${label}`)
    }
    // The burn in one sentence: green on a draft is CI noise, not eligibility.
    assert.match(
      prose,
      /Green\s+on\s+a\s+_draft_\s+PR\s+is\s+expected\s+CI\s+noise, not\s+merge-eligibility/,
      `${dir}/SKILL.md must state that green-on-draft is not merge-eligible`,
    )
    // …and the Phase 3c transition must agree with the rail: a GREEN_DRAFT is a
    // hold, never admitted to the merge queue. A rail that forbids what the
    // operative transition still permits is the burn all over again.
    assert.match(
      prose,
      /\*\*GREEN_DRAFT\*\* \(Passing \+ settled, but\s+the\s+PR\s+is\s+still\s+a \*\*draft\*\*\) → \*\*hold\*\*,\s*never\s+green/,
      `${dir}/SKILL.md Phase 3c must hold a GREEN_DRAFT instead of queueing it`,
    )
    assert.doesNotMatch(
      prose,
      /GREEN_DRAFT\s+or\s+READY_FOR_REVIEW/,
      `${dir}/SKILL.md must not admit GREEN_DRAFT to the greens queue`,
    )

    // (b) The deadlock is diagnosable from the branch shape, not the check state.
    assert.match(
      prose,
      /git\s+rev-list --merges --count\s+origin\/<base>\.\.<branch>/,
      `${dir}/SKILL.md must carry the merge-commit-deadlock diagnosis command`,
    )
    assert.match(
      prose,
      /auto-squashes\s+when\s+the\s+repo\s+allows\s+squash, so\s+re-invoke `merge_session` \*\*once\*\*/,
      `${dir}/SKILL.md must carry the squash recovery (retry merge_session once)`,
    )
    assert.match(
      prose,
      // Stated WITHOUT emitting a copyable base-merge command — the cross-core
      // linear-history gate (TestPublishedCoresNeverInstructBaseMerge) rejects the
      // directive spelled out, even inside a prohibition.
      /a `git\s+merge` of\s+the\s+base\s+ref\s+is\s+forbidden/,
      `${dir}/SKILL.md must carry the linear-history prevention invariant`,
    )

    // (c) Any merge error is re-read against the provider before it is believed.
    assert.match(
      prose,
      /on \*\*any\*\* `merge_session` error, re-read\s+the\s+PR's\s+actual\s+provider\s+state/i,
      `${dir}/SKILL.md must require re-reading provider state on any merge error`,
    )
    assert.match(
      prose,
      /\*\*never\s+re-merge\s+or\s+fail-isolate\*\*/i,
      `${dir}/SKILL.md must forbid re-merging or fail-isolating an already-merged PR`,
    )
    assert.match(
      prose,
      /`merge_session` is\s+idempotent, so\s+the\s+safe\s+generic\s+move\s+is\s+re-read, then\s+retry\s+once/,
      `${dir}/SKILL.md must state the idempotent re-read-then-retry-once move`,
    )

    // (d) Environmental-death is a distinct state with a single wake — and is explicitly
    // carved out of the BLOCKED-nudge ban, which stays in force for BLOCKED.
    assert.match(
      prose,
      /\*\*Environmental-death \/ resume\*\* — a\s+state\s+distinct\s+from\s+BLOCKED/,
      `${dir}/SKILL.md Phase 3c must name environmental death as distinct from BLOCKED`,
    )
    assert.match(
      prose,
      /transient\s+API\/5xx\s+error/,
      `${dir}/SKILL.md must diagnose infra-death from a transient API\\/5xx error`,
    )
    assert.match(
      prose,
      /Deliver \*\*one\*\* wake-to-resume/,
      `${dir}/SKILL.md must allow exactly one wake-to-resume`,
    )
    assert.match(
      prose,
      /within\s+one\s+poll\s+cycle → fail-isolate/,
      `${dir}/SKILL.md must fail-isolate when the wake does not take`,
    )
    assert.match(
      prose,
      /This\s+is \*\*not\*\* the\s+forbidden\s+BLOCKED-nudge/,
      `${dir}/SKILL.md must state the wake is not the forbidden BLOCKED-nudge`,
    )
    // The ban itself must survive this carve-out.
    assert.match(
      prose,
      /A\s+BLOCKED\s+run\s+gets\s+a\s+capped\s+repair\s+round\s+only\s+after\s+the\s+classifier\s+returns `agent-blocked\/repair`, or\s+it\s+is\s+fail-isolated — never\s+a\s+nudge\s+into\s+the\s+original\s+chat/,
      `${dir}/SKILL.md must keep the BLOCKED-nudge ban intact`,
    )

    // The body points at its own reference; the situational detail lives there.
    assert.match(
      skill,
      /references\/merge-recovery\.md/,
      `${dir}/SKILL.md must point at its own references/merge-recovery.md`,
    )

    const ref = readFileSync(
      new URL(`${dir}/references/merge-recovery.md`, import.meta.url),
      'utf8',
    )
    const refProse = ref.replace(/\s+/g, ' ')
    assert.match(
      refProse,
      /continue[ ]from[ ]committed[ ]state; do[ ]not[ ]restart[ ]completed[ ]work/,
      `${dir}/references/merge-recovery.md must carry the wake-to-resume message`,
    )
    assert.match(
      refProse,
      /gh[ ]pr[ ]view <n> --json[ ]state,mergedAt/,
      `${dir}/references/merge-recovery.md must carry the provider-state re-read`,
    )
    assert.match(
      refProse,
      /`states\.inReview`/,
      `${dir}/references/merge-recovery.md must resolve the review state from its config role`,
    )
    // Published core: the reference ships globally, so it must name no project MCP.
    assert.doesNotMatch(
      ref,
      /bossanova-(linear|sentry)/,
      `${dir}/references/merge-recovery.md must stay project-agnostic (no project MCP names)`,
    )
  }

  // Same rationale as the BOS-495 gate: per-mirror phrase checks pass even if one
  // mirror drifts in wording, so diff the new reference between mirrors directly.
  const [canonicalDir, pluginDir] = EPIC_MIRRORS
  assert.equal(
    read(`${pluginDir}/references/merge-recovery.md`),
    read(`${canonicalDir}/references/merge-recovery.md`),
    `${pluginDir}/references/merge-recovery.md must be byte-identical to the canonical mirror (run \`make copy-skills\`)`,
  )
})

test('BOS-523: draft-aware trigger policy + session-hosted wait recipe (both mirrors)', () => {
  // Three operational gaps a production epic run had to improvise around:
  //   (a) no trigger policy — a driver armed bare `checks_passed` on a child PR
  //       that boss-build opens as a DRAFT, so the one-shot watch was consumed by
  //       the first green draft commit, at a moment that can never be
  //       merge-eligible (the BOS-522 gate requires a non-draft PR);
  //   (b) no wait mechanism — "poll every 2–5 minutes" never said HOW a
  //       session-hosted driver sleeps, so a driver backgrounded a shell watcher,
  //       the host reaped it inside the turn, and the epic stalled SILENTLY;
  //   (c) no drift expectation — serialized merges + long builds leave a
  //       late-finishing child many merges behind base, and a driver with no
  //       framing reads that as a child failure instead of a normal repair round.
  // Plus the reporting-contract pointer at the shipped progress helpers, so a
  // driver never hand-rolls the renderer or the create-vs-update decision.
  // Pinned in BOTH Go mirrors so a partial copy-skills edit trips this gate.
  for (const dir of EPIC_MIRRORS) {
    const skill = readFileSync(new URL(`${dir}/SKILL.md`, import.meta.url), 'utf8')
    // Prose assertions run against a whitespace-collapsed copy: prettier re-wraps
    // these paragraphs on every edit, and a re-wrap must not trip a content gate.
    const prose = skill.replace(/\s+/g, ' ')

    // (a) The prohibition and the replacement trigger, both resident.
    assert.match(
      prose,
      /\*\*Never\s+arm\s+bare `checks_passed` on\s+a\s+child\s+PR\.\*\*/,
      `${dir}/SKILL.md must forbid bare checks_passed on a child (draft) PR`,
    )
    assert.match(
      prose,
      /each\s+premature\s+fire\s+consumes\s+the\s+one-shot\s+watch/,
      `${dir}/SKILL.md must say why: a premature fire consumes the one-shot watch`,
    )
    assert.match(
      prose,
      /Arm \*\*`checks_passed_ready`\*\* \(green \*\*and\*\* not\s+a\s+draft/,
      `${dir}/SKILL.md must prescribe checks_passed_ready as the green trigger`,
    )
    assert.match(
      prose,
      /optionally \*\*`ready_for_review`\*\*/,
      `${dir}/SKILL.md must offer ready_for_review for the un-draft flip`,
    )
    // A wake still proves nothing — the BOS-522 merge gate stays authoritative.
    assert.match(
      prose,
      /merge\s+gate\s+stays\s+authoritative — a\s+wake\s+is\s+a\s+signal, not\s+proof/,
      `${dir}/SKILL.md must keep the merge gate authoritative over any wake`,
    )

    // (b) The wait recipe: callbacks primary, session cron fallback, and the
    // backgrounded-watcher anti-pattern — the failure that stalls silently.
    assert.match(
      prose,
      /Callbacks\s+are\s+primary; a \*\*session\s+cron\*\*/,
      `${dir}/SKILL.md must name callbacks primary with a session cron fallback`,
    )
    assert.match(
      prose,
      /\*\*Never\*\* rely\s+on\s+backgrounded\s+shell\s+watchers\s+or\s+sleep\s+loops\s+to\s+hold\s+the\s+wait/,
      `${dir}/SKILL.md must forbid backgrounded watchers/sleep loops as the wait`,
    )
    assert.match(
      prose,
      /session\s+hosts\s+may\s+kill\s+them\s+within\s+the\s+turn/,
      `${dir}/SKILL.md must say why: the host may kill a backgrounded watcher`,
    )
    // Published core: the wait recipe must not name a host's concrete scheduling
    // tool — "a session cron / scheduled prompt" is the portable spelling.
    assert.doesNotMatch(
      skill,
      /CronCreate|run_in_background/,
      `${dir}/SKILL.md wait recipe must stay host-agnostic (no concrete host tool names)`,
    )

    // (c) The drift note names both mitigations, and neither is this skill's to
    // enable — a driver that thinks it must act on drift is the failure mode.
    assert.match(
      prose,
      /\*\*Expect\s+drift, and\s+budget\s+for\s+it\.\*\*/,
      `${dir}/SKILL.md must carry the drift note`,
    )
    assert.match(
      prose,
      /\*\*opt-in\s+proactive\s+rebase\*\* \(a\s+per-repo\s+setting\)/,
      `${dir}/SKILL.md drift note must name the daemon's opt-in proactive rebase`,
    )
    assert.match(
      prose,
      /\*\*linear-history\s+invariant\*\* — every\s+child\s+rebases\s+onto\s+the\s+base\s+and\s+never\s+merges\s+the\s+base\s+back\s+in/,
      `${dir}/SKILL.md drift note must name the linear-history invariant`,
    )
    assert.match(
      prose,
      /Treat\s+a\s+late\s+drift\s+conflict\s+as\s+a\s+normal\s+repair\s+round/,
      `${dir}/SKILL.md drift note must route drift to a repair round, not a failure`,
    )

    // The reporting contract points at the shipped helpers rather than implying a
    // driver hand-rolls the renderer or the create-vs-update decision.
    assert.match(
      prose,
      /\*\*Do\s+not\s+hand-roll\s+the\s+renderer\s+or\s+the\s+create-vs-update\s+decision\*\*/,
      `${dir}/SKILL.md reporting contract must forbid hand-rolling the renderer`,
    )
    // The body's own legend vocabulary is wider than the helper's six statuses,
    // so the pointer must carry the mapping — otherwise a driver following the
    // legend writes a state file `validateProgressState` rejects.
    assert.match(
      prose,
      /queued → `pending` and\s+in-flight\s+or\s+repairing → `building`/,
      `${dir}/SKILL.md must map its legend vocabulary onto PROGRESS_STATUSES`,
    )
    for (const symbol of [
      'toolbox/progress-comment.mjs',
      'validateProgressState',
      'renderProgressComment',
      'planProgressCommentUpsert',
      'PROGRESS_STATUSES',
    ]) {
      assert.ok(
        skill.includes(symbol),
        `${dir}/SKILL.md reporting contract must name the helper symbol: ${symbol}`,
      )
    }

    // The situational detail — trigger table, rationale, cron caveats — lives in
    // the callback reference the body already points at.
    const ref = readFileSync(
      new URL(`${dir}/references/callback-watches.md`, import.meta.url),
      'utf8',
    )
    const refProse = ref.replace(/\s+/g, ' ')
    assert.match(
      refProse,
      /## Trigger\s+policy: draft-aware, never\s+bare `checks_passed`/,
      `${dir}/references/callback-watches.md must carry the trigger policy section`,
    )
    assert.match(
      refProse,
      /Green\s+on\s+a\s+draft\s+PR\s+is\s+expected\s+CI\s+noise, not\s+merge-eligibility/,
      `${dir}/references/callback-watches.md must state green-on-draft is CI noise`,
    )
    // The armed set in the copyable snippet must match the policy above it — a
    // snippet that still loops over bare `checks_passed` is the burn shipped.
    assert.match(
      refProse,
      /for[ ]T[ ]in[ ]checks_passed_ready[ ]checks_failed[ ]merged; do/,
      `${dir}/references/callback-watches.md arm snippet must use checks_passed_ready`,
    )
    // Cron caveats: `*/N` step syntax may be rejected → enumerate the minutes, and
    // never on the herd minutes. Both were learned from a rejected schedule.
    assert.match(
      refProse,
      /\*\*enumerate\s+the\s+minutes\*\* instead: `4,11,18,25,32,39,46,53 \* \* \* \*`/,
      `${dir}/references/callback-watches.md must carry the enumerated-minutes caveat`,
    )
    assert.match(
      refProse,
      /Do\s+not\s+schedule\s+on `:00` or `:30`/,
      `${dir}/references/callback-watches.md must carry the herd-minutes caveat`,
    )
    assert.match(
      refProse,
      /\*\*3\. Anti-pattern — backgrounded\s+watchers\.\*\*/,
      `${dir}/references/callback-watches.md must carry the backgrounded-watcher anti-pattern`,
    )
    assert.match(
      refProse,
      /the\s+failure\s+is \*\*silent\*\*/,
      `${dir}/references/callback-watches.md must say the watcher failure is silent`,
    )
    // Both new rules are also invariants, so a future edit that trims the prose
    // still has to delete an explicitly-listed invariant to lose them.
    assert.match(
      refProse,
      /\*\*Draft-aware\s+green\s+only\.\*\*/,
      `${dir}/references/callback-watches.md must list the draft-aware-green invariant`,
    )
    assert.match(
      refProse,
      /\*\*The\s+wait\s+survives\s+the\s+turn\.\*\*/,
      `${dir}/references/callback-watches.md must list the wait-survives-the-turn invariant`,
    )
  }
})
