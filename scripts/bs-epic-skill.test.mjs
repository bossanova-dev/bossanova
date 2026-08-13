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

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')

const CLAUDE = read('../services/boss/internal/skillinstall/skills/boss-epic/SKILL.md')

// BOS-495: the up-front callback reflex + the single `callbacksAvailable` gate must
// be present in BOTH Go mirrors (byte-identical), and boss-epic must carry its own
// callback-watches reference. The canonical home is skillinstall; the plugin copy is
// the copy-skills mirror — both are asserted so a partial edit trips this gate.
const EPIC_MIRRORS = [
  '../services/boss/internal/skillinstall/skills/boss-epic',
  '../plugins/bossd-plugin-claude/skilldata/skills/boss-epic',
]

test('size ratchet', () => {
  // Ratchet = committed size rounded up to the next KiB. Never raise this
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
  const RATCHET = 53248
  const bytes = Buffer.byteLength(CLAUDE, 'utf8')
  assert.ok(bytes <= RATCHET, `CLAUDE SKILL.md is ${bytes} bytes; must stay <= ${RATCHET}`)
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
    /callback wake[\s\S]*reconcil/i,
    'a callback wake must route through authoritative reconciliation, not act on the trigger name',
  )
  // The two at-least-once safeguards must survive verbatim: a duplicate delivery is a
  // no-op (dedup by id) and a consumed one-shot watch is re-armed while work continues.
  assert.match(
    CLAUDE,
    /dedup by callback id/i,
    'boss-epic must dedup callback deliveries by id (at-least-once delivery)',
  )
  assert.match(
    CLAUDE,
    /re-arm the child's group while it\s+is still in flight/i,
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
      /Prefer a callback over blind polling/i,
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
      /Graceful degradation gated on `callbacksAvailable`/,
      `${dir}/references/callback-watches.md must frame degradation around the gate`,
    )
    assert.match(
      ref,
      /gh pr checks .*--watch --fail-fast/,
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
      /matching orchestrator.*otherwise.*matching child/i,
      `${dir} must prefer a matching orchestrator and fall back to a matching child`,
    )
    assert.match(
      prose,
      /repository.*exactly.*child PR repository/i,
      `${dir} must reject a cross-repository orchestrator target`,
    )
    assert.match(
      prose,
      /no verified target.*skip.*registration.*re-arm.*list.*cleanup/i,
      `${dir} must skip every callback operation when no target is verified`,
    )
    assert.match(
      prose,
      /cron\/poll reconciliation/i,
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
      /const \{ selectEpicCallbackTarget \} = await import\(`\$\{process\.env\.BOSS_EPIC_TOOLBOX\}\/callback\/epic-target\.mjs`\)/,
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
      /process\.stdout\.write\(typeof target\?\.chatId === "string" \? target\.chatId : ""\)/,
      `${dir} bridge must emit the selected chat or empty output`,
    )
    assert.match(
      ref,
      /if \[ -z "\$CALLBACK_CHAT" \]; then\s+echo "No verified callback target; retain cron\/poll reconciliation\. Continue to Phase 3b reconciliation and the bounded poll\/session cron\."/,
      `${dir} bridge must make the no-target cron/poll fallback explicit`,
    )
    assert.doesNotMatch(
      ref,
      /if \[ -z "\$CALLBACK_CHAT" \]; then[\s\S]{0,240}\bcontinue\b/,
      `${dir} no-target branch must fall through to reconciliation, not continue the child loop`,
    )
    assert.match(
      ref,
      /No verified callback target; retain cron\/poll reconciliation\. Continue to Phase 3b reconciliation and the bounded poll\/session cron\./,
      `${dir} no-target branch must retain mandatory reconciliation and fallback polling`,
    )

    // Register, re-arm, and reconciliation list calls carry both scopes.
    assert.match(
      ref,
      /boss callback add[^\n]*--chat "\$CALLBACK_CHAT"[^\n]*--repo "\$CHILD_PR_REPOSITORY"/,
      `${dir} register example must scope --chat and --repo`,
    )
    assert.match(
      ref,
      /boss callback list[^\n]*--chat "\$CALLBACK_CHAT"[^\n]*--repo "\$CHILD_PR_REPOSITORY"/,
      `${dir} list/re-arm example must scope --chat and --repo`,
    )
    for (const command of ['add', 'list', 'remove']) {
      assert.match(
        ref,
        new RegExp(
          `if \\[ -n "\\$CALLBACK_CHAT" \\]; then[\\s\\S]*?boss callback ${command}[\\s\\S]*?fi`,
        ),
        `${dir} callback ${command} commands must be guarded by a verified chat`,
      )
    }

    // The generic CLI intentionally accepts --chat but not --repo for remove.
    // Cleanup must discover ids via the prior scoped list, then remove by id.
    assert.match(
      ref,
      /boss callback remove "\$CALLBACK_ID" --chat "\$CALLBACK_CHAT"/,
      `${dir} cleanup must remove each returned id with --chat`,
    )
    assert.doesNotMatch(
      ref,
      /boss callback remove[^\n]*--repo/,
      `${dir} cleanup must not invent unsupported callback remove --repo`,
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
      /cli\.mjs" states 2>\/dev\/null \|\| true/,
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
    for (const role of ['planned', 'inReview']) {
      assert.ok(
        skill.includes(`resolve_state ${role}`),
        `${dir}/SKILL.md must resolve the ${role} role through the shared helper`,
      )
    }

    // 4. Fail closed only when NEITHER source answers, naming BOTH probed sources
    //    so the operator knows which of the two to populate.
    const block = skill.match(/BLOCKED: no planned state resolved[^"]*/)
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
    /no sessions spawned/,
    'the zero-launch branch must report `no sessions spawned`',
  )
  assert.match(
    CLAUDE,
    /upsert exactly one/i,
    'the zero-launch branch must upsert exactly one progress comment',
  )
  assert.match(
    CLAUDE,
    /stop success/i,
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
    /never\s+create-then-edits|no separate initial-then-final edit/i,
    'the zero-launch branch must forbid create-then-edit of two comments',
  )
})

test('passing greens require settled child chat before merge eligibility', () => {
  assert.match(
    CLAUDE,
    // BOS-522 narrowed this transition to READY_FOR_REVIEW only — a GREEN_DRAFT
    // is now a hold (pinned below, in the merge-gate test).
    /READY_FOR_REVIEW \+ DisplayStatus Passing \+ chat SETTLED/,
    'passing greens must require a settled child chat',
  )
  assert.match(
    CLAUDE,
    /Passing but chat still WORKING\/QUESTION\/LIMITED[^\n]*hold/i,
    'unsettled passing children must be held, not merged or repaired',
  )
  assert.match(
    CLAUDE,
    /treat the\s+child as \*\*not settled\*\* and re-poll; never assume settled on an unreadable status/,
    'unreadable chat status must block merge eligibility',
  )
  assert.match(
    CLAUDE,
    /STOPPED \+\s+missing `last_agent_activity_at` = settled/,
    'stopped chats with no activity timestamp must not be held until wall-clock failure',
  )
  assert.doesNotMatch(
    CLAUDE,
    /GREEN_DRAFT or READY_FOR_REVIEW \+ DisplayStatus Passing\*\*?[^+\n]*→ add to the\s+\*\*greens\*\*/,
    'the old Passing-only green transition must not return',
  )
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
    /must not use\s+`?create_session`?.*tmux_unattended/i,
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
    /implementation work uses[^\n]*createSession[^\n]*tmux_unattended/i,
    'implementation work must name the createSession capability + tmux_unattended',
  )
  assert.match(
    CLAUDE,
    /unattended[^\n]*planning[^\n]*subagent/i,
    'unattended planning fan-out must route to a subagent',
  )
  assert.match(
    CLAUDE,
    /visible planning chat uses[^\n]*createPlanningChat/i,
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
    /still schedules\/merges greens/,
    'runner fallback must not claim it can merge greens without chat settlement',
  )
  assert.match(
    CLAUDE,
    /runner without\s+readable chat status must\s+hold or fail-isolate/i,
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
  assert.match(CLAUDE, /at most one merge in flight/i)
  // repair/dispatch does not tear down an in-progress session
  assert.match(CLAUDE, /leave the session open/i)
  // the epic set boundary is a hard guarantee, never crossed
  assert.match(CLAUDE, /outside the epic set are NEVER mutated/i)
  // headless unattended discipline: no interactive prompts past preflight
  assert.match(CLAUDE, /never call AskUserQuestion after Phase 0/i)
  // wiring contract: done siblings clear via externallyCleared, never merged
  assert.match(CLAUDE, /`done` ids are folded into\s+`externallyCleared`/)
  // negative pin: the done-bucket-into-merged misstatement must never return
  // (backticked `done` = the classifyTickets bucket; the plain-prose "Done"
  // state name legitimately appears near `merged` on the 3d success path)
  assert.doesNotMatch(CLAUDE, /`done`[^\n]*into `merged`/)
  // loop termination: greens keep their concurrency slot until merged
  assert.match(CLAUDE, /`greens` is a \*\*subset\*\*\s+of `inFlight`/)
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
    /empty draft PR placeholder/,
    'skill must name the empty draft PR placeholder case',
  )
  assert.match(
    CLAUDE,
    /adopt(?:s)? the existing branch\/session/i,
    'the placeholder case must adopt the existing branch/session and continue',
  )
  assert.match(
    CLAUDE,
    /a PR number alone is (?:never|not)[^\n]*(?:completion|merge)/i,
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
    /no complete boss transport/i,
    'neither transport complete must produce a concise diagnostic naming what is missing',
  )
  assert.match(CLAUDE, /before scheduling/i, 'the tool-discovery preflight runs before scheduling')
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
      /frozen repair lease/,
      `${dir}/SKILL.md must name the frozen repair lease fail-isolate note`,
    )
    // The human-readable diagnosis cheat: the three-signal shape of a dead lease,
    // so an operator reading a stuck run recognises it without the classifier.
    assert.match(
      prose,
      /`last_repair_head_sha` unchanged and repair-chat output stale across ≥2\s?polls/,
      `${dir}/SKILL.md must cross-reference the dead-lease diagnosis cheat`,
    )
    assert.match(
      prose,
      /Never re-poll a lease already classified `stalled`/,
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
      /merge-eligible only when all five hold/i,
      `${dir}/SKILL.md must state the five-condition merge gate`,
    )
    for (const [label, re] of [
      [
        'daemon gate authoritative',
        /`gate 1` \/ DisplayStatus `Passing` — the daemon gate is authoritative/,
      ],
      ['settled chat', /the build chat has SETTLED/],
      ['non-draft PR', /the PR is \*\*not a draft\*\*/],
      // Resolved from the configured state ROLE — a literal state name here would
      // be workspace-specific and fail the published-core project-agnostic gate.
      [
        'ticket in the review state',
        /\*\*review\*\* state \(resolved from[\s\S]*?`states\.inReview`/,
      ],
      ['no do-not-merge marker', /no\*\* partial-slice or `do not merge` marker/],
    ]) {
      assert.match(prose, re, `${dir}/SKILL.md merge gate must require: ${label}`)
    }
    // The burn in one sentence: green on a draft is CI noise, not eligibility.
    assert.match(
      prose,
      /Green on a _draft_ PR is expected CI noise, not merge-eligibility/,
      `${dir}/SKILL.md must state that green-on-draft is not merge-eligible`,
    )
    // …and the Phase 3c transition must agree with the rail: a GREEN_DRAFT is a
    // hold, never admitted to the merge queue. A rail that forbids what the
    // operative transition still permits is the burn all over again.
    assert.match(
      prose,
      /\*\*GREEN_DRAFT\*\* \(Passing \+ settled, but the PR is still a \*\*draft\*\*\) → \*\*hold\*\*,\s*never green/,
      `${dir}/SKILL.md Phase 3c must hold a GREEN_DRAFT instead of queueing it`,
    )
    assert.doesNotMatch(
      prose,
      /GREEN_DRAFT or READY_FOR_REVIEW/,
      `${dir}/SKILL.md must not admit GREEN_DRAFT to the greens queue`,
    )

    // (b) The deadlock is diagnosable from the branch shape, not the check state.
    assert.match(
      prose,
      /git rev-list --merges --count origin\/<base>\.\.<branch>/,
      `${dir}/SKILL.md must carry the merge-commit-deadlock diagnosis command`,
    )
    assert.match(
      prose,
      /auto-squashes when the repo allows squash, so re-invoke `merge_session` \*\*once\*\*/,
      `${dir}/SKILL.md must carry the squash recovery (retry merge_session once)`,
    )
    assert.match(
      prose,
      // Stated WITHOUT emitting a copyable base-merge command — the cross-core
      // linear-history gate (TestPublishedCoresNeverInstructBaseMerge) rejects the
      // directive spelled out, even inside a prohibition.
      /a `git merge` of the base ref is forbidden/,
      `${dir}/SKILL.md must carry the linear-history prevention invariant`,
    )

    // (c) Any merge error is re-read against the provider before it is believed.
    assert.match(
      prose,
      /on \*\*any\*\* `merge_session` error, re-read the PR's actual provider state/i,
      `${dir}/SKILL.md must require re-reading provider state on any merge error`,
    )
    assert.match(
      prose,
      /\*\*never re-merge or fail-isolate\*\*/i,
      `${dir}/SKILL.md must forbid re-merging or fail-isolating an already-merged PR`,
    )
    assert.match(
      prose,
      /`merge_session` is idempotent, so the safe generic move is re-read, then retry once/,
      `${dir}/SKILL.md must state the idempotent re-read-then-retry-once move`,
    )

    // (d) Infra-death is a distinct state with a single wake — and is explicitly
    // carved out of the BLOCKED-nudge ban, which stays in force for BLOCKED.
    assert.match(
      prose,
      /\*\*Infra-death\*\* — a state distinct from BLOCKED/,
      `${dir}/SKILL.md Phase 3c must name infra-death as distinct from BLOCKED`,
    )
    assert.match(
      prose,
      /transient API\/5xx error/,
      `${dir}/SKILL.md must diagnose infra-death from a transient API\\/5xx error`,
    )
    assert.match(
      prose,
      /Deliver \*\*one\*\* wake-to-resume/,
      `${dir}/SKILL.md must allow exactly one wake-to-resume`,
    )
    assert.match(
      prose,
      /within one poll cycle → fail-isolate/,
      `${dir}/SKILL.md must fail-isolate when the wake does not take`,
    )
    assert.match(
      prose,
      /This is \*\*not\*\* the forbidden BLOCKED-nudge/,
      `${dir}/SKILL.md must state the wake is not the forbidden BLOCKED-nudge`,
    )
    // The ban itself must survive this carve-out.
    assert.match(
      prose,
      /A BLOCKED run gets a capped repair round or is fail-isolated — never a nudge into the original chat/,
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
      /continue from committed state; do not restart completed work/,
      `${dir}/references/merge-recovery.md must carry the wake-to-resume message`,
    )
    assert.match(
      refProse,
      /gh pr view <n> --json state,mergedAt/,
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
      /\*\*Never arm bare `checks_passed` on a child PR\.\*\*/,
      `${dir}/SKILL.md must forbid bare checks_passed on a child (draft) PR`,
    )
    assert.match(
      prose,
      /each premature fire consumes the one-shot watch/,
      `${dir}/SKILL.md must say why: a premature fire consumes the one-shot watch`,
    )
    assert.match(
      prose,
      /Arm \*\*`checks_passed_ready`\*\* \(green \*\*and\*\* not a draft/,
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
      /merge gate stays authoritative — a wake is a signal, not proof/,
      `${dir}/SKILL.md must keep the merge gate authoritative over any wake`,
    )

    // (b) The wait recipe: callbacks primary, session cron fallback, and the
    // backgrounded-watcher anti-pattern — the failure that stalls silently.
    assert.match(
      prose,
      /Callbacks are primary; a \*\*session cron\*\*/,
      `${dir}/SKILL.md must name callbacks primary with a session cron fallback`,
    )
    assert.match(
      prose,
      /\*\*Never\*\* rely on backgrounded shell watchers or sleep loops to hold the wait/,
      `${dir}/SKILL.md must forbid backgrounded watchers/sleep loops as the wait`,
    )
    assert.match(
      prose,
      /session hosts may kill them within the turn/,
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
      /\*\*Expect drift, and budget for it\.\*\*/,
      `${dir}/SKILL.md must carry the drift note`,
    )
    assert.match(
      prose,
      /\*\*opt-in proactive rebase\*\* \(a per-repo setting\)/,
      `${dir}/SKILL.md drift note must name the daemon's opt-in proactive rebase`,
    )
    assert.match(
      prose,
      /\*\*linear-history invariant\*\* — every child rebases onto the base and never merges the base back in/,
      `${dir}/SKILL.md drift note must name the linear-history invariant`,
    )
    assert.match(
      prose,
      /Treat a late drift conflict as a normal repair round/,
      `${dir}/SKILL.md drift note must route drift to a repair round, not a failure`,
    )

    // The reporting contract points at the shipped helpers rather than implying a
    // driver hand-rolls the renderer or the create-vs-update decision.
    assert.match(
      prose,
      /\*\*Do not hand-roll the renderer or the create-vs-update decision\*\*/,
      `${dir}/SKILL.md reporting contract must forbid hand-rolling the renderer`,
    )
    // The body's own legend vocabulary is wider than the helper's six statuses,
    // so the pointer must carry the mapping — otherwise a driver following the
    // legend writes a state file `validateProgressState` rejects.
    assert.match(
      prose,
      /queued → `pending` and in-flight or repairing → `building`/,
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
      /## Trigger policy: draft-aware, never bare `checks_passed`/,
      `${dir}/references/callback-watches.md must carry the trigger policy section`,
    )
    assert.match(
      refProse,
      /Green on a draft PR is expected CI noise, not merge-eligibility/,
      `${dir}/references/callback-watches.md must state green-on-draft is CI noise`,
    )
    // The armed set in the copyable snippet must match the policy above it — a
    // snippet that still loops over bare `checks_passed` is the burn shipped.
    assert.match(
      refProse,
      /for T in checks_passed_ready checks_failed merged; do/,
      `${dir}/references/callback-watches.md arm snippet must use checks_passed_ready`,
    )
    // Cron caveats: `*/N` step syntax may be rejected → enumerate the minutes, and
    // never on the herd minutes. Both were learned from a rejected schedule.
    assert.match(
      refProse,
      /\*\*enumerate the minutes\*\* instead: `4,11,18,25,32,39,46,53 \* \* \* \*`/,
      `${dir}/references/callback-watches.md must carry the enumerated-minutes caveat`,
    )
    assert.match(
      refProse,
      /Do not schedule on `:00` or `:30`/,
      `${dir}/references/callback-watches.md must carry the herd-minutes caveat`,
    )
    assert.match(
      refProse,
      /\*\*3\. Anti-pattern — backgrounded watchers\.\*\*/,
      `${dir}/references/callback-watches.md must carry the backgrounded-watcher anti-pattern`,
    )
    assert.match(
      refProse,
      /the failure is \*\*silent\*\*/,
      `${dir}/references/callback-watches.md must say the watcher failure is silent`,
    )
    // Both new rules are also invariants, so a future edit that trims the prose
    // still has to delete an explicitly-listed invariant to lose them.
    assert.match(
      refProse,
      /\*\*Draft-aware green only\.\*\*/,
      `${dir}/references/callback-watches.md must list the draft-aware-green invariant`,
    )
    assert.match(
      refProse,
      /\*\*The wait survives the turn\.\*\*/,
      `${dir}/references/callback-watches.md must list the wait-survives-the-turn invariant`,
    )
  }
})
