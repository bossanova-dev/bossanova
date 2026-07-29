import assert from 'node:assert/strict'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import fs from 'node:fs'

const rootDir = fileURLToPath(new URL('..', import.meta.url))

// BOS-271 collapsed the published cores onto the boss-repair single-source
// topology: the canonical committed home for boss-build is the embedded
// skillinstall payload, with no .claude/.codex committed copy. These tests read
// that canonical home. (The boss-build-superpowers *extension* stays repo-local
// and dual-mirrored under .claude/.codex, so its own assertions below are unchanged.)
const CORE = 'services/boss/internal/skillinstall/skills/boss-build'

const RESIDENT_BODY_SKILLS = [`${CORE}/SKILL.md`]

// BOS-495: the up-front callback reflex + the single `callbacksAvailable` gate must
// be present in BOTH Go mirrors (byte-identical). The canonical home is skillinstall;
// the plugin copy is the copy-skills mirror. Both are asserted so a partial edit trips
// this gate.
const BUILD_MIRRORS = [CORE, 'plugins/bossd-plugin-claude/skilldata/skills/boss-build']

test('headless mode + mode-aware proof are present', () => {
  for (const skillPath of RESIDENT_BODY_SKILLS) {
    const skill = fs.readFileSync(path.join(rootDir, skillPath), 'utf8')
    assert.match(skill, /BS_HEADLESS/)
    assert.match(skill, /proof\.mjs plan/)
  }
})

test('every moved reference is reachable: a body pointer plus an existing file', () => {
  const skillDirs = [path.join(rootDir, CORE)]
  const references = [
    'references/core-spine.md',
    'references/code-reviewer-template.md',
    'references/receiving-code-review.md',
    'references/review-stack.md',
    'references/proof-capture.md',
    'references/callback-watches.md',
    'references/cron-gate.md',
    'references/troubleshooting.md',
    'references/resume-assessment.md',
    'references/standalone-mode.md',
  ]

  for (const skillDir of skillDirs) {
    const skill = fs.readFileSync(path.join(skillDir, 'SKILL.md'), 'utf8')
    for (const reference of references) {
      assert.match(
        skill,
        new RegExp(reference.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
        `${skillDir}/SKILL.md must point at ${reference}`,
      )
      assert.equal(
        fs.existsSync(path.join(skillDir, reference)),
        true,
        `${reference} must exist under ${skillDir}`,
      )
    }
  }
})

test('methodology extension carries moved SDD/TDD references in both mirrors', () => {
  const extensionDirs = [
    path.join(rootDir, '.claude/skills/boss-build-superpowers'),
    path.join(rootDir, '.codex/skills/boss-build-superpowers'),
  ]
  const references = [
    'references/subagent-driven-development.md',
    'references/test-driven-development.md',
  ]

  for (const skillDir of extensionDirs) {
    const skill = fs.readFileSync(path.join(skillDir, 'SKILL.md'), 'utf8')
    assert.match(skill, /role: methodology/)
    for (const reference of references) {
      assert.match(
        skill,
        new RegExp(reference.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
        `${skillDir}/SKILL.md must point at ${reference}`,
      )
      assert.equal(
        fs.existsSync(path.join(skillDir, reference)),
        true,
        `${reference} must exist under ${skillDir}`,
      )
    }
  }
})

test('Step 6 routes the review verdict from a run-file sentinel, not returned prose (BOS-148)', () => {
  // The BOS-144 file-sentinel convention retrofitted onto the Step-6 review dispatch:
  // the review subagent writes its terminal `bs-review clean:` / `bs-review capped:`
  // sentinel to a run file; the orchestrator classifies FROM THE FILE ONLY via
  // matchSentinel, and a missing/stale sentinel becomes a distinct dispatch-failure that
  // takes the safe non-clean (BLOCKED) branch — never clean. Pin the contract tokens in
  // BOTH mirrors so the routing can never regress to reading the reply.
  const contractTokens = [
    'bs-run-sentinel.mjs', // the run-file sentinel helper (BOS-144)
    'matchSentinel', // the classifier the orchestrator routes on
    'dispatch-failure', // the synthesized dead/missing-sentinel verdict
    'bs-review clean:', // byte-stable clean prefix
    'bs-review capped:', // byte-stable capped prefix
  ]
  for (const skillPath of RESIDENT_BODY_SKILLS) {
    const skill = fs.readFileSync(path.join(rootDir, skillPath), 'utf8')
    for (const token of contractTokens) {
      assert.ok(
        skill.includes(token),
        `${skillPath} Step 6 must document the file-sentinel contract token "${token}"`,
      )
    }
    // The verdict is read from the file, never from the returned prose.
    assert.match(
      skill,
      /from the (run )?file only|never from (returned |the )?(reply|prose)/i,
      `${skillPath} must state the verdict is routed from the run file, not returned prose`,
    )
  }
})

test('Step 6c boss-review sentinel is advisory and cannot drive the run-file verdict', () => {
  for (const skillDir of [CORE]) {
    const reviewStack = fs.readFileSync(
      path.join(rootDir, skillDir, 'references/review-stack.md'),
      'utf8',
    )
    assert.match(
      reviewStack,
      /Step 6c.*advisory.*does not drive the run-file verdict/is,
      `${skillDir}/references/review-stack.md must keep Step 6c advisory and non-routing`,
    )
    assert.doesNotMatch(
      reviewStack,
      /bs-review capped:[\s\S]{0,160}proceed to Step 7\s+anyway/i,
      `${skillDir}/references/review-stack.md must not let bs-review capped look like the run-file capped/BLOCKED verdict`,
    )
    assert.doesNotMatch(
      reviewStack,
      /bs-review capped:[\s\S]{0,220}not[\s\S]{0,40}BLOCKED condition/i,
      `${skillDir}/references/review-stack.md must not describe bs-review capped as a competing terminal-state route`,
    )
  }
})

test('Step 11 names proof.mjs run as the single proof channel (BOS-138)', () => {
  // P1d skill-path enforcement: the structured note posted by `proof.mjs run` is the
  // ONLY proof channel. Sessions never hand-write "proof skipped" prose, and the env is
  // daemon-injected (doctor reports gaps) rather than sourced from .env. Pin the clause in
  // BOTH generated mirrors and in the proof-capture reference so it can never regress to
  // the old hand-written-skip-note guidance.
  for (const skillPath of RESIDENT_BODY_SKILLS) {
    const skill = fs.readFileSync(path.join(rootDir, skillPath), 'utf8')
    assert.match(
      skill,
      /is the only proof channel/i,
      `${skillPath} Step 11 must name proof.mjs run's note as the only proof channel`,
    )
    assert.match(
      skill,
      /proof\.mjs doctor/,
      `${skillPath} Step 11 must point at proof.mjs doctor for a missing env, not sourcing .env`,
    )
    assert.doesNotMatch(
      skill,
      /set -a; \. \.\/\.env/,
      `${skillPath} Step 11 must not tell sessions to source .env`,
    )
  }

  for (const skillDir of [CORE]) {
    const proofCapture = fs.readFileSync(
      path.join(rootDir, skillDir, 'references/proof-capture.md'),
      'utf8',
    )
    assert.match(
      proofCapture,
      /\*\*only\*\*/,
      `${skillDir}/references/proof-capture.md must emphasize proof.mjs run's note as the only channel`,
    )
    assert.match(
      proofCapture,
      /proof\s+channel/i,
      `${skillDir}/references/proof-capture.md must name proof.mjs run's note as the only proof channel`,
    )
    assert.match(
      proofCapture,
      /daemon-injected/,
      `${skillDir}/references/proof-capture.md must state the proof env is daemon-injected`,
    )
    assert.doesNotMatch(
      proofCapture,
      /set -a; \. \.\/\.env/,
      `${skillDir}/references/proof-capture.md must not tell sessions to source .env`,
    )
  }
})

test('Step 5 directs a TUI PR to author + commit a proof scenario before finalization (BOS-220)', () => {
  // BOS-220: the deterministic TUI safety net only helps if it gets authored before
  // Step 8 finalizes the PR. Keep the detailed workflow in proof-capture (byte-budget-safe),
  // but pin the Step-5 requirement and pointer here. Deliberately avoid exact fixture-preset
  // names, which are owned downstream (BOS-217).
  for (const skillPath of RESIDENT_BODY_SKILLS) {
    const skill = fs.readFileSync(path.join(rootDir, skillPath), 'utf8')
    const step5 = skill.slice(skill.indexOf('## Step 5:'), skill.indexOf('## Step 6:'))
    assert.match(
      step5,
      /proof\/scenarios\/\*\.scenario\.json/,
      `${skillPath} Step 5 must name the proof/scenarios/*.scenario.json a TUI PR must commit`,
    )
    assert.match(
      step5,
      /references\/proof-capture\.md/,
      `${skillPath} Step 5 must point TUI scenario authoring at proof-capture`,
    )
  }

  for (const skillDir of [CORE]) {
    const proofCapture = fs.readFileSync(
      path.join(rootDir, skillDir, 'references/proof-capture.md'),
      'utf8',
    )
    assert.match(
      proofCapture,
      /must commit a `proof\/scenarios\/\*\.scenario\.json`/,
      `${skillDir}/references/proof-capture.md must mandate committing a scenario for a TUI PR`,
    )
    assert.match(
      proofCapture,
      /scenario validate/,
      `${skillDir}/references/proof-capture.md must document the scenario validate authoring loop`,
    )
    assert.match(
      proofCapture,
      /scenario run [^\n]*--dry-run/,
      `${skillDir}/references/proof-capture.md must document the scenario run --dry-run iterate loop`,
    )
    assert.match(
      proofCapture,
      /gates \*\*only its own PR\*\*|only its own PR/,
      `${skillDir}/references/proof-capture.md must state a scenario gates only its own PR (no path rules)`,
    )
  }
})

test('runtime helper references are local to methodology extension mirrors', () => {
  // The support helper scripts are now referenced from the moved SDD/TDD references,
  // not the resident SKILL.md. They must still be reachable (and present) in both mirrors.
  const expectedHelpers = [
    'support/superpowers-6.0.3/subagent-driven-development/implementer-prompt.md',
    'support/superpowers-6.0.3/subagent-driven-development/task-reviewer-prompt.md',
    'support/superpowers-6.0.3/subagent-driven-development/scripts/review-package',
    'support/superpowers-6.0.3/subagent-driven-development/scripts/task-brief',
    'support/superpowers-6.0.3/test-driven-development/testing-anti-patterns.md',
  ]

  const skillDirs = [
    path.join(rootDir, '.claude/skills/boss-build-superpowers'),
    path.join(rootDir, '.codex/skills/boss-build-superpowers'),
  ]

  for (const skillDir of skillDirs) {
    const skill = fs.readFileSync(path.join(skillDir, 'SKILL.md'), 'utf8')
    assert.doesNotMatch(
      skill,
      /\.claude\/skills\/_construct/,
      `${skillDir} must not reference construct inputs`,
    )

    const referenceText = [
      'references/subagent-driven-development.md',
      'references/test-driven-development.md',
    ]
      .map((reference) => fs.readFileSync(path.join(skillDir, reference), 'utf8'))
      .join('\n')

    for (const helper of expectedHelpers) {
      assert.match(
        referenceText,
        new RegExp(helper.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
        `${helper} must be referenced from a moved reference under ${skillDir}`,
      )
      assert.equal(
        fs.existsSync(path.join(skillDir, helper)),
        true,
        `${helper} must exist under ${skillDir}`,
      )
    }
  }
})

// BOS-181: finalize is reordered so the [#PR] tag-injection + force-push runs BEFORE the
// boss-repair green gate (CI runs once on the tagged head), Step 9 becomes an idempotent guard
// that re-injects only when repair added untagged commits, and the Step 7 boss-review comment is
// unconditional. Pin all three in the committed .claude body (the shipped artifact) so the
// reorder can never silently regress to the double-CI-wait / conditional-comment shape.
// BOS-200: the tag-injection command is now the finalize adapter's inject-PR-tag capability
// (toolbox/finalize/cli.mjs inject-pr-tag, which delegates to add-pr-numbers.sh); the assertions
// track that command while preserving the BOS-181 order/guard/ready invariants.
const claudeBody = () => fs.readFileSync(path.join(rootDir, `${CORE}/SKILL.md`), 'utf8')

test('Step 8 injects the [#PR] tag before the boss-repair green gate (BOS-181)', () => {
  const skill = claudeBody()
  const step8 = skill.slice(skill.indexOf('## Step 8:'), skill.indexOf('## Step 9:'))
  const tagIdx = step8.indexOf('inject-pr-tag')
  const gateIdx = step8.search(/Then run \*\*boss-repair\*\*/)
  assert.ok(
    tagIdx !== -1,
    'Step 8 must inject the tag via the finalize adapter inject-pr-tag capability',
  )
  assert.ok(gateIdx !== -1, 'Step 8 must run the boss-repair green gate after tagging')
  assert.ok(tagIdx < gateIdx, 'tag injection must run BEFORE the boss-repair green gate')
  // The daemon-race guard is preserved on the (now-earlier) push.
  assert.match(step8, /--force-with-lease/)
  assert.match(step8, /HEAD == @\{u\}/)
})

test('Step 9 re-injects the tag only via an idempotent guard (BOS-181)', () => {
  const skill = claudeBody()
  const step9 = skill.slice(skill.indexOf('## Step 9:'), skill.indexOf('## Step 10:'))
  assert.match(step9, /idempotent/i, 'Step 9 must document the idempotent guard')
  // Guarded re-inject: conditional on commits still lacking the tag, not an unconditional rewrite.
  assert.match(step9, /if git log[^\n]*grep -qv/, 'Step 9 re-inject must be conditional')
  assert.match(step9, /inject-pr-tag/, 'Step 9 must keep the re-inject capability')
  assert.match(step9, /gh pr ready/, 'Step 9 must ready the PR')
})

test('Step 7 always posts the boss-review comment, unconditionally (BOS-181)', () => {
  const skill = claudeBody()
  const step7 = skill.slice(skill.indexOf('## Step 7:'), skill.indexOf('## Step 8:'))
  // The old conditional-skip clause is gone.
  assert.doesNotMatch(step7, /Skip this when Step 6c was skipped/i)
  // Always upsert one marker comment, with an honest fallback when there is no report.
  assert.match(step7, /Post the boss-review comment \(always\)/)
  assert.match(step7, /fallback note/i)
})

// BOS-240: boss-build must finalize BLOCKED (not REVIEW_READY) when it defers a *required*
// item at the wall-clock cap. These assertions pin the honest-finalize invariant into the
// shipped Claude and Codex artifacts so the terminal-state logic can't silently regress. They
// read the committed generated files (no source skip) so CI enforces them without superpowers.
const RESIDENT_BODIES = {
  canonical: `${CORE}/SKILL.md`,
}
const readSkill = (rel) => fs.readFileSync(path.join(rootDir, rel), 'utf8')
const readRef = (mirror, ref) =>
  fs.readFileSync(
    path.join(rootDir, path.dirname(RESIDENT_BODIES[mirror]), 'references', ref),
    'utf8',
  )

test('BOS-240: resident body pins the required-deferred → BLOCKED finalize invariant (both mirrors)', () => {
  for (const [mirror, rel] of Object.entries(RESIDENT_BODIES)) {
    const body = readSkill(rel)
    // The distinction + invariant is stated in the always-resident body.
    assert.match(
      body,
      /Required-deferred/,
      `${mirror}: body must define the required-deferred distinction`,
    )
    assert.match(
      body,
      /BLOCKED, never REVIEW_READY/,
      `${mirror}: body must state required-deferred ⇒ BLOCKED, never REVIEW_READY`,
    )
    // Required = API-version bump for an observable bossanova.v1 change + open must-fix findings.
    assert.match(body, /API-version bump/, `${mirror}: required must name the API-version bump`)
    assert.match(body, /must-fix findings/, `${mirror}: required must name open must-fix findings`)
    assert.match(
      body,
      /bossanova\.v1/,
      `${mirror}: required item is an observable bossanova.v1 change`,
    )
    // Optional stays non-fatal (Minor findings + best-effort proof).
    assert.match(
      body,
      /_?optional_?[^\n]*Minor findings[^\n]*best-effort proof[^\n]*non-fatal/i,
      `${mirror}: optional (Minor findings + best-effort proof) must stay non-fatal`,
    )
    // The wall-clock breaker no longer grants "usually BLOCKED" latitude.
    assert.doesNotMatch(
      body,
      /usually BLOCKED/,
      `${mirror}: wall-clock breaker must not permit REVIEW_READY with an unaddressed required item`,
    )
    // Enforced at the finalize gate (Step 9) and terminal-state selection (Step 12).
    const step9 = body.slice(body.indexOf('## Step 9:'), body.indexOf('## Step 10:'))
    assert.match(
      step9,
      /no required item was deferred/,
      `${mirror}: Step 9 must assert no required item was deferred before readying`,
    )
    const step12 = body.slice(body.indexOf('## Step 12:'))
    assert.match(
      step12,
      /REVIEW_READY only with no deferred required item/,
      `${mirror}: Step 12 must pick REVIEW_READY only with no deferred required item`,
    )
  }
})

test('BOS-240: review-stack adds a conditional API-surface required check (both mirrors)', () => {
  for (const mirror of Object.keys(RESIDENT_BODIES)) {
    const reviewStack = readRef(mirror, 'review-stack.md')
    assert.match(
      reviewStack,
      /API-surface check/,
      `${mirror}: review-stack must add the conditional API-surface check`,
    )
    assert.match(
      reviewStack,
      /proto\/bossanova\/v1/,
      `${mirror}: API-surface check must trigger on proto/bossanova/v1 paths`,
    )
    assert.match(
      reviewStack,
      /lib\/bossalib\/apiversion/,
      `${mirror}: API-surface check must reference the apiversion registry`,
    )
    // A missing version bump is a required must-fix, and a deferred one routes to BLOCKED.
    assert.match(
      reviewStack,
      /required-deferred/,
      `${mirror}: a missing required version bump must be a required-deferred item`,
    )
    assert.match(
      reviewStack,
      /BLOCKED[^\n]*never REVIEW_READY/,
      `${mirror}: a deferred required version bump routes to BLOCKED, never REVIEW_READY`,
    )
  }
})

test('BOS-303: an empty/bootstrap draft PR placeholder is adopted and resumed, not a stop condition', () => {
  // boss-epic (BOS-303) routes an empty draft PR placeholder to the child
  // boss-build to continue from existing branch/session state. This pins the
  // boss-build side of that contract: an existing empty bootstrap/draft PR is
  // adopted (Step 2.5 → fresh reuse / resume), never treated as completed work
  // and never a stop condition merely because a PR number exists.
  const skill = claudeBody()
  assert.match(
    skill,
    /bossd's bootstrap draft, an empty PR[^\n]*is \*\*adopted and\s*\n?resumed\*\*, not a stop condition/,
    'boss-build must state an existing empty/bootstrap draft PR is adopted and resumed, not a stop condition',
  )
  const step25 = skill.slice(skill.indexOf('## Step 2.5:'), skill.indexOf('## Step 3:'))
  assert.match(
    step25,
    /empty bootstrap PR is adoptable, never foreign/,
    'Step 2.5 must classify an empty bootstrap PR as adoptable, never foreign',
  )
  assert.match(
    step25,
    /the bootstrap PR \(no create\)/,
    'Step 2.5 must route a bootstrap-only PR to fresh-reuse (adopt), not restart',
  )
})

test('BOS-495: up-front callback reflex + callbacksAvailable gate in both mirrors', () => {
  // The awareness fix: "prefer a callback over blind polling" is an up-front Hard-rules
  // reflex, gated on the single `callbacksAvailable(env)` signal, present byte-identically
  // in BOTH Go mirrors, and the callback reference frames graceful degradation around the
  // gate (gate false ⇒ skip registerWatch → fallbackPoll, never a failed wait). Pin all
  // three so the discoverability fix can never silently regress to a buried wait-step hint.
  for (const dir of BUILD_MIRRORS) {
    const skill = fs.readFileSync(path.join(rootDir, dir, 'SKILL.md'), 'utf8')
    const hardRules = skill.slice(skill.indexOf('## Hard rules'), skill.indexOf('## Trust rules'))
    assert.match(
      hardRules,
      /Prefer a callback over blind polling/i,
      `${dir}/SKILL.md Hard rules must carry the up-front "prefer a callback over blind polling" reflex`,
    )
    assert.ok(
      hardRules.includes('callbacksAvailable'),
      `${dir}/SKILL.md Hard rules reflex must gate on callbacksAvailable`,
    )

    // The callback reference names the gate and frames degradation around it.
    const ref = fs.readFileSync(path.join(rootDir, dir, 'references/callback-watches.md'), 'utf8')
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
  }

  // The "byte-identical in BOTH Go mirrors" claim above is only real if something
  // diffs the two copies: the per-mirror phrase checks pass even if one mirror drifts
  // in whitespace/wording or a `make copy-skills` is skipped. Enforce it directly.
  const [canonicalDir, pluginDir] = BUILD_MIRRORS
  for (const rel of ['SKILL.md', 'references/callback-watches.md']) {
    assert.equal(
      fs.readFileSync(path.join(rootDir, pluginDir, rel), 'utf8'),
      fs.readFileSync(path.join(rootDir, canonicalDir, rel), 'utf8'),
      `${pluginDir}/${rel} must be byte-identical to the canonical mirror (run \`make copy-skills\`)`,
    )
  }
})

test('BOS-470: CI/PR waits adopt one-shot callbacks with authoritative reconciliation + safe fallback', () => {
  // The wait steps (green gate Step 8, re-inject Step 9) must arm a grouped one-shot GitHub
  // callback, reconcile against real PR state before acting, dedup at-least-once delivery, re-arm
  // while still waiting, and degrade to a bounded poll when callbacks are unavailable. Pin the
  // contract in the resident body + the deep-dive reference so it can never regress to a naked poll.
  const skill = claudeBody()
  const bodyTokens = [
    'resolveCallbackAdapter', // the callback-notifier adapter seam
    'toolbox/callback/adapter.mjs', // its path
    'boss callback add', // the generic register CLI (project-agnostic host interface)
    'policy.watchTriggers', // the grouped trigger set lives in policy, not hard-coded prose
    'reconcile against real', // authoritative reconciliation before acting
    'policy.fallbackPoll', // bounded fallback when callbacks are unavailable
  ]
  for (const token of bodyTokens) {
    assert.ok(skill.includes(token), `boss-build SKILL.md must document callback token "${token}"`)
  }
  // Both wait points (Step 8 green gate, Step 9 re-inject) point at the callback reference.
  const step8 = skill.slice(skill.indexOf('## Step 8:'), skill.indexOf('## Step 9:'))
  const step9 = skill.slice(skill.indexOf('## Step 9:'), skill.indexOf('## Step 10:'))
  for (const [name, step] of [
    ['Step 8', step8],
    ['Step 9', step9],
  ]) {
    assert.match(
      step,
      /references\/callback-watches\.md/,
      `${name} must point the CI/PR wait at references/callback-watches.md`,
    )
  }

  // The reference nails the four hard invariants: grouped triggers, reconcile-before-act,
  // idempotent under duplicate delivery, and graceful degradation to the poll.
  const ref = fs.readFileSync(path.join(rootDir, CORE, 'references/callback-watches.md'), 'utf8')
  assert.match(
    ref,
    /checks_passed[\s\S]*checks_failed[\s\S]*merged/,
    'reference must name the grouped triggers',
  )
  assert.match(ref, /Reconcile before act/i, 'reference must state reconcile-before-act')
  assert.match(
    ref,
    /Idempotent under duplicate/i,
    'reference must state idempotent-under-duplicate delivery',
  )
  assert.match(
    ref,
    /Graceful degradation/i,
    'reference must state graceful degradation to the poll',
  )
  assert.match(
    ref,
    /gh pr checks .*--watch --fail-fast/,
    'reference must keep the bounded fallback poll',
  )
  // Published-core invariant: no host-specific tracker/MCP identity leaks into the reference.
  assert.doesNotMatch(
    ref,
    /bossanova-(linear|sentry)/,
    'reference must stay project-agnostic (no project MCP names)',
  )
})

test('BOS-240: troubleshooting adds the required-deferred rows without weakening optional proof (both mirrors)', () => {
  for (const mirror of Object.keys(RESIDENT_BODIES)) {
    const troubleshooting = readRef(mirror, 'troubleshooting.md')
    // New status-rollback row: required item deferred at cap → In Progress / draft / names the item.
    assert.match(
      troubleshooting,
      /Required item deferred at cap/,
      `${mirror}: status-rollback table must cover the required-deferred-at-cap case`,
    )
    // New red-flag row: skipping the API version as optional past the cap is wrong.
    assert.match(
      troubleshooting,
      /skip the API version as optional/,
      `${mirror}: red-flags must catch "skip the API version as optional past the cap"`,
    )
    // The existing optional-proof red-flag must NOT be weakened: proof stays non-fatal.
    assert.match(
      troubleshooting,
      /Proof is optional and non-fatal/,
      `${mirror}: the optional-proof red-flag must remain (proof stays non-fatal)`,
    )
  }
})

// baseMergeDirectives returns every "merge the base ref into the branch" instruction in doc.
// It mirrors the Go classifier of the same name (skills_manifest_test.go): scan each `git
// merge` / `git pull` invocation up to a backtick or newline (so prose that only NAMES the
// forbidden command is not itself a directive), ignore a trailing `#` comment, exempt
// `--rebase`/`-r` pulls (they linearize), and flag any operand naming a base ref.
const BASE_REF_OPERAND =
  /^(?:remotes\/)?(?:origin|upstream)\/|^\$\{?BASE_BRANCH|^FETCH_HEAD$|^(?:main|master|develop|staging|production|trunk)$/
function baseMergeDirectives(doc) {
  const hits = []
  for (const match of doc.matchAll(/git\s+(?:merge|pull)\s+([^\n`]*)/g)) {
    let rebasing = false
    const operands = []
    for (const rawField of match[1].split(/\s+/)) {
      const field = rawField.replace(/^["'`.,;:()]+|["'`.,;:()]+$/g, '')
      if (field.startsWith('#')) break
      if (field.startsWith('-')) {
        const flag = field.split('=')[0]
        if (flag === '--rebase' || flag === '-r') rebasing = true
        continue
      }
      if (field) operands.push(field)
    }
    if (rebasing) continue
    if (operands.some((operand) => BASE_REF_OPERAND.test(operand))) hits.push(match[0].trim())
  }
  return hits
}

test('BOS-514: the finalize-adjacent base sync points at the linear-history invariant (both mirrors)', () => {
  // boss-build rebases the session branch onto the PR base before/inside finalize. It must
  // never contradict the invariant boss-repair owns: base sync is ALWAYS a rebase, because a
  // merge commit on the PR branch makes GitHub's rebase-merge structurally refuse the merge
  // and deadlocks the PR. Pin one pointer line plus the absence of any base-merge directive.
  for (const dir of BUILD_MIRRORS) {
    const skill = fs.readFileSync(path.join(rootDir, dir, 'SKILL.md'), 'utf8')
    assert.match(
      skill,
      /Linear history:/,
      `${dir}/SKILL.md must point at the linear-history invariant where it syncs with the base`,
    )
    assert.match(
      skill,
      /rev-list --merges --count/,
      `${dir}/SKILL.md must name the zero-merge-commit preflight`,
    )
    assert.deepStrictEqual(
      baseMergeDirectives(skill),
      [],
      `${dir}/SKILL.md must not instruct a base merge — sync with the base by rebasing`,
    )
  }
})

test('BOS-514: the base-merge classifier catches the spellings it must catch', () => {
  // Parity check for the Go gate (TestBaseMergeDirectivesDetection in
  // services/boss/internal/skillinstall/skills_manifest_test.go). Keep the two in sync: a
  // classifier that stops classifying turns both gates green while gating nothing.
  for (const cmd of [
    'git merge origin/main',
    'git merge "origin/$BASE_BRANCH"',
    'git merge --no-ff "origin/${BASE_BRANCH}"',
    'git merge -X ours origin/main',
    'git merge remotes/origin/main',
    'git merge upstream/main',
    'git merge FETCH_HEAD',
    'git merge main',
    'git pull origin main',
    'git pull --no-rebase origin "$BASE_BRANCH"',
  ]) {
    assert.notDeepStrictEqual(baseMergeDirectives(cmd), [], `${cmd} must be flagged`)
  }
  for (const cmd of [
    'git merge-base "origin/$BASE_BRANCH" HEAD',
    'git rebase "origin/$BASE_BRANCH"',
    'git pull --rebase',
    'git pull --rebase origin "$BASE_BRANCH"',
    'git merge --abort',
    'A `git merge` of the base ref is FORBIDDEN.',
    'git merge --abort  # never merge main into the branch',
  ]) {
    assert.deepStrictEqual(baseMergeDirectives(cmd), [], `${cmd} must not be flagged`)
  }
})

// BOS-519: implementation subagents must commit per task and never return dirty, the
// orchestrator must verify (clean tree + advanced log) and recover residue instead of
// hard-failing, and a resume must dispatch only the remainder. Pin all three so the
// per-task-commit contract can't regress to an end-of-run batch commit — which is what
// makes a mid-run subagent death lose the whole run instead of one task.
test('BOS-519: commit-before-return contract reaches all three dispatch paths (both mirrors)', () => {
  for (const dir of BUILD_MIRRORS) {
    const skill = fs.readFileSync(path.join(rootDir, dir, 'SKILL.md'), 'utf8')
    const step5 = skill.slice(skill.indexOf('## Step 5:'), skill.indexOf('## Step 6:'))

    // Slice on a marker that must exist: a missing marker makes `indexOf` return -1, and
    // `slice(0, -1)` would silently hand back nearly all of Step 5 — so an overlay-scoped
    // assertion would start passing against the tier-3 text instead of failing loudly.
    const at = (marker) => {
      const index = step5.indexOf(marker)
      assert.notEqual(index, -1, `${dir}/SKILL.md Step 5 must still contain the marker "${marker}"`)
      return index
    }
    const overlay = () => step5.slice(0, at('Resolve the implementation methodology'))
    const tier3Section = () => step5.slice(at('### Inline TDD methodology (tier 3)'))

    // Path 1 — the boss-build overlay contract paragraph every brief carries.
    assert.match(
      step5,
      /\*\*Commit-before-return contract\.\*\*/,
      `${dir}/SKILL.md Step 5 must name the commit-before-return contract`,
    )
    assert.match(
      step5,
      /Never batch the whole assignment into one end-of-run commit/i,
      `${dir}/SKILL.md Step 5 must forbid one end-of-run commit for the whole assignment`,
    )
    assert.match(
      step5,
      /\*\*Never return with uncommitted work\.\*\*/,
      `${dir}/SKILL.md Step 5 must forbid returning with uncommitted work`,
    )
    // The subagent-facing check is bounded to the subagent's OWN changes. A bare "status must
    // be empty, commit whatever remains" would make task 1 of every run commit the Step 4 plan
    // deliverable — and, in any repo where the host artifacts are not gitignored, sweep those
    // onto the branch too. Pin the scoping at both subagent-facing sites (overlay + tier 3).
    for (const [name, section] of [
      ['overlay', overlay()],
      ['tier 3', tier3Section()],
    ]) {
      assert.match(
        section,
        /`git status --porcelain` → nothing left from[\s\S]{0,12}(\*\*)?your own(\*\*)? changes/,
        `${dir}/SKILL.md ${name} must bound the return-time status check to the subagent's own changes`,
      )
      assert.match(
        section,
        /staging only[\s\S]{0,20}the paths you touched[\s\S]{0,20}never `git add -A`/,
        `${dir}/SKILL.md ${name} must stage only the paths the subagent touched, never git add -A`,
      )
    }
    assert.match(
      step5,
      /not\s+yours to commit[\s\S]{0,200}belong to the orchestrator/,
      `${dir}/SKILL.md Step 5 must tell the subagent the plan deliverable and host artifacts are not its to commit`,
    )
    // A failing hook is a surfaced task failure, not a licence to return dirty — but only
    // after one adapt-and-retry, so a host hook that dictates a subject format (a mandatory
    // PR tag, say) does not turn every task of a resume into a reported failure.
    assert.match(
      step5,
      /commit hook rejects the message[\s\S]{0,200}retry once/i,
      `${dir}/SKILL.md Step 5 must let a subagent adapt to the hook's own error and retry once`,
    )
    assert.match(
      step5,
      /never a value you invented/,
      `${dir}/SKILL.md Step 5 hook-retry must not license inventing a tag`,
    )
    // "never return dirty" + "a failed commit is a task failure" with no third branch is a
    // trap: the only way out an obedient subagent can see is to REVERT its own work. Carve
    // the exception explicitly, and make it hand the residue to the orchestrator.
    assert.match(
      step5,
      /\*\*leave the work in the tree and never revert it\*\*/,
      `${dir}/SKILL.md Step 5 must forbid reverting work that cannot be committed`,
    )
    assert.match(
      step5,
      /name the uncommitted paths[\s\S]{0,120}residue recovery/i,
      `${dir}/SKILL.md Step 5 must hand the uncommitted paths to the orchestrator's recovery`,
    )
    assert.match(
      step5,
      /reported task\s*\n?\s*failure — never a silent one/,
      `${dir}/SKILL.md Step 5 must keep an uncommittable change a reported task failure`,
    )
    // Rationale survives in the text (kept project-agnostic: no ticket ids in a published core).
    assert.match(
      step5,
      /inject-PR-tag rebase fail/i,
      `${dir}/SKILL.md Step 5 must record why uncommitted subagent edits are fatal`,
    )
    assert.match(
      step5,
      /bound the blast radius[^.]*one task/i,
      `${dir}/SKILL.md Step 5 must record the blast-radius rationale for per-task commits`,
    )
    // Subagents must not guess a tag, and must keep subjects short.
    assert.match(
      step5,
      /need \*\*no\*\* PR tag/,
      `${dir}/SKILL.md Step 5 must say task commits need no PR tag`,
    )
    assert.match(
      step5,
      /over 100 characters is[\s\S]{0,8}skipped by the tag injector/,
      `${dir}/SKILL.md Step 5 must warn that an over-long tagged subject is skipped`,
    )

    // The fixed short contract gains a "commits made" field, so a subagent has a defined
    // slot for the commits it made — and for the no-commit exception the orchestrator
    // verification below depends on. Pinned at BOTH definition sites (the boss-build
    // overlay and the tier-3 return line) plus the portable spine.
    for (const [name, section] of [
      ['overlay', overlay()],
      ['tier 3', tier3Section()],
    ]) {
      assert.match(
        section,
        /commits made[\s\S]{0,20}\(short SHA \+[\s\S]{0,12}subject/i,
        `${dir}/SKILL.md ${name} fixed short contract must include the commits-made field`,
      )
      assert.match(
        section,
        /no commit —[\s\S]{0,12}verification only/i,
        `${dir}/SKILL.md ${name} fixed short contract must define the no-commit note`,
      )
    }
    const spine = fs.readFileSync(path.join(rootDir, dir, 'references/core-spine.md'), 'utf8')
    assert.match(
      spine,
      /commits it made[\s\S]{0,20}\(short SHA \+[\s\S]{0,12}subject/i,
      `${dir}/references/core-spine.md must carry the commits-made field in the fixed short contract`,
    )
    // The troubleshooting catalog enumerates the contract's fields; a stale five-field list
    // there teaches the orchestrator to drop the sixth when threading contracts forward.
    const troubles = fs.readFileSync(
      path.join(rootDir, dir, 'references/troubleshooting.md'),
      'utf8',
    )
    assert.match(
      troubles,
      /residual risks, commits made/,
      `${dir}/references/troubleshooting.md fixed-short-contract row must list the commits-made field`,
    )
    assert.match(
      troubles,
      /commit everything once the whole assignment is done/,
      `${dir}/references/troubleshooting.md must red-flag batching the run into one end-of-run commit`,
    )

    // Path 2 — tier-1 methodology extensions inherit and pass the contract down.
    const tier1 = step5.slice(
      step5.indexOf('Tier 1 — discovered methodology extensions'),
      step5.indexOf('Tier 2 — host built-in'),
    )
    assert.match(
      tier1,
      /\*\*commit-before-return contract\*\*/,
      `${dir}/SKILL.md tier 1 must pass the commit-before-return contract to each extension`,
    )
    assert.match(
      tier1,
      /pass it down to its own implementation subagents/,
      `${dir}/SKILL.md tier 1 must require extensions to pass the contract down`,
    )

    // Path 2b — tier 2 hands a host-native affordance the same contract. Without this the one
    // dispatch route that delegates to the host's own implementation loop silently opts out of
    // committing per task, and its subagents return dirty like the pre-BOS-519 behaviour.
    const tier2 = step5.slice(
      step5.indexOf('Tier 2 — host built-in'),
      step5.indexOf('Tier 3 — inline TDD methodology'),
    )
    assert.match(
      tier2,
      /\*\*commit-before-return contract\*\*/,
      `${dir}/SKILL.md tier 2 must hand the commit-before-return contract to the host affordance`,
    )
    assert.match(
      tier2,
      /not an\s*\n?\s*exemption from committing per\s*\n?\s*task/i,
      `${dir}/SKILL.md tier 2 must deny a host-native path any exemption from per-task commits`,
    )
    // The verification stays with the orchestrator: telling tier 2 to "verify it the same way"
    // would put a second writer on the fixed snapshot path.
    assert.match(
      tier2,
      /run\s*\n?\s*the same after-return check yourself once the affordance returns/i,
      `${dir}/SKILL.md tier 2 must keep the after-return check with the orchestrator`,
    )

    // Path 3 — the inline tier-3 TDD loop carries it too (bare-host last resort).
    const tier3 = tier3Section()
    assert.match(
      tier3,
      /\*\*commit-before-return contract\*\*/,
      `${dir}/SKILL.md tier 3 must honour the commit-before-return contract`,
    )
    assert.match(
      tier3,
      /never return with uncommitted work/i,
      `${dir}/SKILL.md tier 3 must forbid returning with uncommitted work`,
    )

    // The subagent's own commit needs the same `--only` scoping the recovery commit got: a plain
    // `git commit` sweeps in whatever the orchestrator staged before dispatch (the plan
    // deliverable, a host artifact) — invisible to the subagent and not its to commit.
    assert.match(
      overlay(),
      /git commit --only -m "…" -- <files>/,
      `${dir}/SKILL.md overlay must path-scope each task commit`,
    )
    assert.match(
      overlay(),
      /plain\s*\n?\s*`git commit` commits the whole index/,
      `${dir}/SKILL.md overlay must say why the task commit is path-scoped`,
    )
  }
})

test('BOS-519: orchestrator verifies clean tree + advanced log and recovers residue (both mirrors)', () => {
  for (const dir of BUILD_MIRRORS) {
    const skill = fs.readFileSync(path.join(rootDir, dir, 'SKILL.md'), 'utf8')
    const step5 = skill.slice(skill.indexOf('## Step 5:'), skill.indexOf('## Step 6:'))

    assert.match(
      step5,
      /\*\*Orchestrator verification\.\*\*/,
      `${dir}/SKILL.md Step 5 must define the orchestrator verification step`,
    )
    // The cadence is the property this ticket buys: verifying after EACH subagent bounds a
    // mid-run death to one task. Degrading it to a single end-of-run check would leave every
    // other assertion here green, so pin the word.
    assert.match(
      step5,
      /after\s+\*\*each\*\*\s+subagent\s+returns/,
      `${dir}/SKILL.md Step 5 must verify after each subagent, not once at the end`,
    )
    // Both halves: the recorded pre-dispatch HEAD and the log range against it. The HEAD is
    // persisted to a file, not a shell variable — the two blocks straddle the dispatch and run
    // in different shells, so a variable would be unset by the time the log range needs it.
    assert.ok(
      step5.includes('>"$(git rev-parse --git-dir)/boss-build-pre-dispatch-head"'),
      `${dir}/SKILL.md Step 5 must persist the pre-dispatch HEAD across the dispatch boundary`,
    )
    assert.ok(
      step5.includes(
        `git log --oneline "$(cut -d' ' -f1 "$(git rev-parse --git-dir)/boss-build-pre-dispatch-head")..HEAD"`,
      ),
      `${dir}/SKILL.md Step 5 must check the log advanced since the pre-dispatch HEAD (SHA field only)`,
    )
    // /tmp is shared: a sibling worktree's concurrent run would clobber the recorded HEAD and
    // fake an advanced (or empty) log range. The git dir is per-worktree.
    assert.doesNotMatch(
      step5,
      /\/tmp\/pre-dispatch-head/,
      `${dir}/SKILL.md Step 5 must not keep the pre-dispatch HEAD in a shared /tmp path`,
    )
    assert.match(
      step5,
      /concurrent runs in sibling worktrees cannot overwrite/,
      `${dir}/SKILL.md Step 5 must explain why the HEAD file is worktree-local`,
    )
    // Recording HEAD before the cleanup commit would make a task that landed nothing read as done.
    assert.match(
      step5,
      /re-run this whole block afterwards\*\*[\s\S]{0,240}reads as done/,
      `${dir}/SKILL.md Step 5 must re-record the pre-dispatch HEAD after resolving pre-existing dirt`,
    )
    assert.match(
      step5,
      /cannot be attributed to one, do \*\*not\*\* dispatch on top of it[\s\S]{0,120}BLOCKED/,
      `${dir}/SKILL.md Step 5 must block rather than dispatch onto un-attributable dirt`,
    )
    assert.match(
      step5,
      /nothing set in the first block survives into the second/,
      `${dir}/SKILL.md Step 5 must state that the pre- and post-dispatch blocks are separate shells`,
    )
    // The clean-tree check must be SCOPED. A bare `git status --porcelain` is non-empty on
    // every run — Step 4 copies the plan deliverable and defers committing it to Step 6 — so
    // an unscoped check would report a violation every time and stop discriminating. Pin the
    // exclusions (plan deliverable + the daemon artifacts Step 6's gate also excludes).
    assert.ok(
      step5.includes('git status --porcelain --untracked-files=all -- .'),
      `${dir}/SKILL.md Step 5 must check the tree is clean after each subagent`,
    )
    // Anchor the expectation to the SCOPED command: a bare "clean" comment would keep passing if
    // the pathspec were dropped and the comment moved back onto an unscoped status.
    assert.match(
      step5,
      /':\(exclude\)\.claude\/settings\.local\.json'\n# must be empty/,
      `${dir}/SKILL.md Step 5 must state the expected empty result of the after-return status`,
    )
    for (const excluded of [
      '":(exclude)${PLAN_DOC:?',
      "':(exclude).claude/scheduled_tasks.lock'",
      "':(exclude).claude/settings.local.json'",
    ]) {
      assert.ok(
        step5.includes(excluded),
        `${dir}/SKILL.md Step 5 clean-tree check must exclude expected non-residue ${excluded}`,
      )
    }
    // Exclude the ONE plan file this run copied, never the directory: a directory-wide
    // exclusion also hides a subagent's stray edit to another plan doc, which IS residue.
    assert.ok(
      !step5.includes("':(exclude)docs/plans'"),
      `${dir}/SKILL.md Step 5 must not exclude the whole docs/plans directory`,
    )
    assert.match(
      skill,
      /PLAN_DOC="docs\/plans\//,
      `${dir}/SKILL.md Step 4 must name the copied plan deliverable in PLAN_DOC`,
    )
    // Each snippet runs in a fresh shell, so PLAN_DOC must be re-set inside Step 5 itself — and
    // guarded: an unset var makes the exclude a bare `:(exclude)`, which excludes EVERYTHING and
    // turns the residue check into a silent pass.
    assert.match(
      step5,
      /PLAN_DOC="docs\/plans\//,
      `${dir}/SKILL.md Step 5 must re-set PLAN_DOC rather than assume Step 4's shell survived`,
    )
    assert.doesNotMatch(
      step5,
      /:\(exclude\)\$PLAN_DOC/,
      `${dir}/SKILL.md Step 5 must not use an unguarded $PLAN_DOC in the exclude pathspec`,
    )
    assert.match(
      step5,
      /excludes _everything_[\s\S]{0,80}silent\s*\n?\s*pass/i,
      `${dir}/SKILL.md Step 5 must explain why an unset PLAN_DOC must abort`,
    )
    // Residue is what the subagent ADDED, not everything dirty: pre-existing dirt belongs to
    // nobody's task and must not be swept into a recovery commit.
    assert.match(
      step5,
      /pre-dispatch status must already be empty/i,
      `${dir}/SKILL.md Step 5 must require a clean tree before dispatching a task`,
    )
    assert.match(
      step5,
      /no way to tell pre-existing dirt[\s\S]{0,200}subagent's own residue/i,
      `${dir}/SKILL.md Step 5 must explain why dispatching onto dirt destroys residue attribution`,
    )
    assert.match(
      step5,
      /tree was clean at dispatch, everything this status lists is \*\*this\*\*\s*\n?\s*subagent's residue/i,
      `${dir}/SKILL.md Step 5 must attribute post-dispatch dirt to the returning subagent`,
    )
    // The recovery commit hits the same hooks the subagent's did — it needs the same escape.
    assert.match(
      step5,
      /recovery commit goes through the same hooks[\s\S]{0,400}BLOCKED/,
      `${dir}/SKILL.md Step 5 must handle a hook rejecting the residue-recovery commit`,
    )
    assert.match(
      step5,
      /stays untracked until Step 6 commits it/,
      `${dir}/SKILL.md Step 5 must explain why the plan deliverable is not residue`,
    )
    // `--untracked-files=all` is load-bearing, not decoration: at the default `-unormal` git
    // collapses `.claude/` to one directory entry that no per-file exclusion matches.
    assert.match(
      step5,
      /Keep\s+`--untracked-files=all`[\s\S]{0,200}collapses an untracked directory/,
      `${dir}/SKILL.md Step 5 must explain why --untracked-files=all is load-bearing`,
    )
    assert.match(
      step5,
      /never the whole `docs\/plans`\s*\n?\s*directory/,
      `${dir}/SKILL.md Step 5 must forbid a directory-wide docs/plans exclusion`,
    )
    // A no-commit task is recorded, not failed — via the commits-made field that exists for it.
    assert.match(
      step5,
      /legitimately produces no commit[\s\S]{0,120}commits\n?\s*made\*\* field/,
      `${dir}/SKILL.md Step 5 must route a no-commit task through the commits-made field`,
    )
    // Violation ⇒ recovery, not hard failure.
    assert.match(
      step5,
      /\*\*recover rather than[\s\S]{0,4}hard-fail\*\*/,
      `${dir}/SKILL.md Step 5 must recover from a contract violation instead of hard-failing`,
    )
    // `--only` is load-bearing: a plain `git commit` commits the whole index, so a path the
    // residue check deliberately excluded ($PLAN_DOC, a daemon artifact) that was staged earlier
    // rides along invisibly. Verified empirically: `git add -- <paths>` + `git commit --only --
    // <paths>` commits exactly those paths and leaves an unrelated pre-staged file staged.
    assert.ok(
      step5.includes('git commit --only -m "chore(task-N): recover uncommitted subagent work"'),
      `${dir}/SKILL.md Step 5 must give the concrete residue-recovery commit command`,
    )
    assert.doesNotMatch(
      step5,
      /\n\s*git commit -m "chore\(task-N\)/,
      `${dir}/SKILL.md Step 5 must not use a whole-index git commit for residue recovery`,
    )
    assert.match(
      step5,
      /whole index[\s\S]{0,200}swept in silently/i,
      `${dir}/SKILL.md Step 5 must say why the recovery commit is path-scoped`,
    )
    // `task-N` is a template, not a literal: the substitution must be spelled out.
    assert.match(
      step5,
      /substitute the task's number for `?N/,
      `${dir}/SKILL.md Step 5 must tell the orchestrator to substitute the real task number`,
    )
    // Recovery must not contradict Step 6's "never a blanket git add -A" rule.
    assert.match(
      step5,
      /\*\*not\*\*\s+a\s*\n?\s*licence for a[\s\S]{0,20}blanket `git add -A`/,
      `${dir}/SKILL.md Step 5 residue recovery must not license a blanket git add -A`,
    )
    // Recovery preserves the work but proves nothing about completeness: a subagent that died
    // mid-task leaves a partial implementation that commits cleanly. Step 5 must re-assess, the
    // way the resume reference already does, or a half-done task advances as if finished.
    assert.match(
      step5,
      /does not prove the task is \*\*done\*\*/i,
      `${dir}/SKILL.md must not treat a recovery commit as proof the task is complete`,
    )
    assert.match(
      step5,
      /re-assess the recovered task against its acceptance criteria[\s\S]{0,200}re-dispatch it/i,
      `${dir}/SKILL.md must re-assess and re-dispatch a recovered task that falls short`,
    )
    // Guard every spelling of the blanket stage, not just `-A`: `git add .` and `git add --all`
    // sweep daemon artifacts and unrelated scratch onto the branch exactly the same way.
    assert.doesNotMatch(
      step5,
      /^\s*git add\s+(-A\b|--all\b|\.\s*$)/m,
      `${dir}/SKILL.md Step 5 must not issue a blanket stage-everything command`,
    )
    // The verification has two halves, so it needs two remedies. An empty log range with a
    // clean tree is NOT residue — "commit the residue yourself" is a no-op there, and an
    // orchestrator that follows it silently drops the task.
    assert.match(
      step5,
      /empty log range[\s\S]{0,400}re-dispatch that task/i,
      `${dir}/SKILL.md Step 5 must give the empty-log-range half its own re-dispatch remedy`,
    )
    // The snapshot path is fixed, so it is only safe with a single writer: a nested layer that ran
    // the same procedure would clobber and then delete the outer orchestrator's baseline.
    assert.match(
      step5,
      /nothing you dispatch runs it again[\s\S]{0,320}never this verification/i,
      `${dir}/SKILL.md must keep the snapshot procedure with the orchestrator, not hand it down`,
    )
    assert.match(
      step5,
      /overwrite and then delete the file you wrote[\s\S]{0,160}no\s*\n?\s*baseline to read/i,
      `${dir}/SKILL.md must name the nested-snapshot failure the single-writer rule prevents`,
    )
    // A clean pre-dispatch tree only excludes dirt that predates the dispatch. When the subagent
    // returned, its **files touched** field is the second opinion that keeps a stray concurrent
    // write from being committed under this task's `chore(task-N)`.
    assert.match(
      step5,
      /clean start only rules out dirt that predates the dispatch/i,
      `${dir}/SKILL.md must bound what the clean-tree precondition actually proves`,
    )
    assert.match(
      step5,
      /\*\*files touched\*\* field[\s\S]{0,240}does _not_ name as\s*\n?\s*unattributed/i,
      `${dir}/SKILL.md must cross-check residue against the returned contract's files-touched field`,
    )
    // …and the recovery command must stage that attributed subset, not every status path, or the
    // cross-check above is prose the very next code block contradicts.
    assert.ok(
      step5.includes('git add -- <the attributed residue paths>'),
      `${dir}/SKILL.md recovery must stage the attributed subset, not every path git status listed`,
    )
    assert.match(
      step5,
      /could \*\*not\*\* attribute stays out of that commit[\s\S]{0,200}BLOCKED naming those\s*\n?\s*paths/i,
      `${dir}/SKILL.md must block on un-attributable residue rather than committing it under the task`,
    )
    // A subagent that dies without returning never trips the after-return check at all, and on
    // a fresh run Step 4.5 never executed — so Step 5 must carry the inventory rule itself.
    assert.match(
      step5,
      /never returns[\s\S]{0,400}fresh[\s\S]{0,600}continue from committed\s+state; do not redo committed tasks/i,
      `${dir}/SKILL.md Step 5 must cover a subagent that dies without returning, including on a fresh run`,
    )
    // A non-empty log range alone proves nothing: any commit reaching HEAD lands in it, so the
    // reported SHAs must be cross-checked or a concurrent commit makes a task that did nothing
    // read as done.
    assert.match(
      step5,
      /necessary but not sufficient[\s\S]{0,200}commits\s+made\*\* field actually appear in it/i,
      `${dir}/SKILL.md must cross-check the reported commits against the post-dispatch log range`,
    )
    // Detecting the mismatch is useless without a remedy: a cross-check that only says "look" lets
    // the orchestrator note the discrepancy and advance anyway.
    assert.match(
      step5,
      /empty-log-range case wearing a disguise[\s\S]{0,160}re-dispatch the task/i,
      `${dir}/SKILL.md must give the reported-SHA mismatch the empty-range re-dispatch remedy`,
    )
    // The snapshot is consumed on success, so its presence means "a dispatch is in flight" rather
    // than "some earlier task left this behind" — the premise the restart branch below relies on.
    assert.ok(
      step5.includes('rm "$(git rev-parse --git-dir)/boss-build-pre-dispatch-head"'),
      `${dir}/SKILL.md must delete the pre-dispatch snapshot once the range checks out`,
    )
    assert.match(
      step5,
      /\*\*any\*\* resolved outcome[\s\S]{0,220}not\s*\n?\s*only on the clean path/i,
      `${dir}/SKILL.md must consume the snapshot on every resolved outcome, not just the clean one`,
    )
    // Consuming it *before* the recovery commit lands would discard the clean-tree guarantee that
    // made the residue attributable, exactly when a crash needs it most.
    assert.match(
      step5,
      /\*\*after\*\* the recovery commit lands, never before you start it/i,
      `${dir}/SKILL.md must order the snapshot deletion after the recovery commit`,
    )
    assert.match(
      step5,
      /left behind on the no-commit or recovery\s*\n?\s*paths would make a finished task look interrupted/i,
      `${dir}/SKILL.md must name the stale-snapshot failure the deletion prevents`,
    )
    assert.match(
      step5,
      /exists \*\*only\*\* while a dispatch is in\s*\n?\s*flight/i,
      `${dir}/SKILL.md must state the consumed snapshot's invariant`,
    )
    // The blanket recovery command is only safe because the tree was verified clean at dispatch.
    // A restarted orchestrator has no such snapshot, so the never-returns path must branch to
    // per-path attribution instead of sweeping every dirty path into a recovery commit.
    assert.match(
      step5,
      /the snapshot, not on whether your process restarted\*\*/i,
      `${dir}/SKILL.md must branch residue recovery on the snapshot, not on a process restart`,
    )
    assert.match(
      step5,
      /is present[\s\S]{0,260}use the command above\s*\n?\s*unchanged/i,
      `${dir}/SKILL.md must gate the blanket residue recovery on the surviving pre-dispatch snapshot`,
    )
    assert.match(
      step5,
      /file is \*\*absent\*\*[\s\S]{0,240}attribute each dirty path[\s\S]{0,160}cannot attribute/i,
      `${dir}/SKILL.md must require per-path attribution when the pre-dispatch snapshot is gone`,
    )
    // A SHA alone does not say which task was in flight, and the log only shows the tasks that
    // finished — so a restarted orchestrator would have to guess the N in `chore(task-N)`.
    assert.ok(
      step5.includes(
        `printf '%s task-N\\n' "$(git rev-parse HEAD)" \\\n  >"$(git rev-parse --git-dir)/boss-build-pre-dispatch-head"`,
      ),
      `${dir}/SKILL.md must record the dispatched task id alongside the pre-dispatch HEAD`,
    )
    assert.match(
      step5,
      /second field is the `task-N`[\s\S]{0,320}Read `N` from the file rather than guessing/i,
      `${dir}/SKILL.md must make the restarted orchestrator read the interrupted task id, not guess it`,
    )
  }
})

test('BOS-519: resume dispatches only the remainder from committed state (both mirrors)', () => {
  for (const dir of BUILD_MIRRORS) {
    const skill = fs.readFileSync(path.join(rootDir, dir, 'SKILL.md'), 'utf8')
    const step45 = skill.slice(skill.indexOf('## Step 4.5:'), skill.indexOf('## Step 5:'))

    // The resident sentence lives in Step 4.5 so a resume can't miss it.
    assert.match(
      step45,
      /inventory committed state/i,
      `${dir}/SKILL.md Step 4.5 must inventory committed state on a resume`,
    )
    assert.match(
      step45,
      /continue from committed\s+state; do not redo committed tasks/,
      `${dir}/SKILL.md Step 4.5 must carry the continue-from-committed-state instruction`,
    )
    assert.match(
      step45,
      /dispatch \*\*only\*\* the remainder/,
      `${dir}/SKILL.md Step 4.5 must dispatch only the remaining tasks`,
    )

    // The fuller procedure lives in the reference.
    const ref = fs.readFileSync(path.join(rootDir, dir, 'references/resume-assessment.md'), 'utf8')
    assert.match(
      ref,
      /## Continue from committed state/,
      `${dir}/references/resume-assessment.md must document the continue-from-committed-state procedure`,
    )
    assert.match(
      ref,
      /continue from committed\s+state; do not redo committed tasks/,
      `${dir}/references/resume-assessment.md must carry the standing instruction verbatim`,
    )
    assert.ok(
      ref.includes('git log --oneline "$BASE_BRANCH..HEAD"'),
      `${dir}/references/resume-assessment.md must inventory the branch log`,
    )
    assert.match(
      ref,
      /chore\(task-N\): recover uncommitted subagent work/,
      `${dir}/references/resume-assessment.md must recover residue left by a dead subagent`,
    )
    assert.match(
      ref,
      /never a blanket `git add -A`/,
      `${dir}/references/resume-assessment.md residue recovery must not license a blanket git add -A`,
    )
    // The resume residue probe must be scoped exactly like the Step 5 check — an unscoped
    // status here would classify the plan deliverable and host artifacts as dead-subagent
    // residue and produce a spurious `chore(task-N)` recovery commit on every resume.
    assert.ok(
      ref.includes('git status --porcelain --untracked-files=all -- .'),
      `${dir}/references/resume-assessment.md residue probe must be scoped like the Step 5 check`,
    )
    for (const excluded of [
      '":(exclude)${PLAN_DOC:?',
      "':(exclude).claude/scheduled_tasks.lock'",
      "':(exclude).claude/settings.local.json'",
    ]) {
      assert.ok(
        ref.includes(excluded),
        `${dir}/references/resume-assessment.md residue probe must exclude ${excluded}`,
      )
    }
    assert.ok(
      !ref.includes("':(exclude)docs/plans'"),
      `${dir}/references/resume-assessment.md must not exclude the whole docs/plans directory`,
    )
    assert.doesNotMatch(
      ref,
      /:\(exclude\)\$PLAN_DOC/,
      `${dir}/references/resume-assessment.md must not use an unguarded $PLAN_DOC in the exclude pathspec`,
    )
    assert.match(
      ref,
      /PLAN_DOC="docs\/plans\//,
      `${dir}/references/resume-assessment.md must re-set PLAN_DOC in the probe's own shell`,
    )
    // The snapshot is consumed on every resolved outcome, so it survives a crash and still means
    // "a dispatch was in flight from a verified-clean tree". The resume path must branch on it the
    // same way Step 5 does, or it contradicts the durable-snapshot design it inherits.
    assert.ok(
      ref.includes('"$(git rev-parse --git-dir)/boss-build-pre-dispatch-head"'),
      `${dir}/references/resume-assessment.md must branch resume recovery on the pre-dispatch snapshot`,
    )
    assert.match(
      ref,
      /\*\*Present\*\*[\s\S]{0,160}recover it the way Step 5 does/,
      `${dir}/references/resume-assessment.md must recover residue Step 5's way when the snapshot survives`,
    )
    assert.match(
      ref,
      /`task-N`[\s\S]{0,160}second field rather than guessing which task was in flight/,
      `${dir}/references/resume-assessment.md must read the interrupted task id from the snapshot`,
    )
    // Only the absent branch may fall back to per-path attribution — blind recovery without the
    // clean-tree guarantee would sweep a human's in-flight edits into a `chore(task-N)` commit.
    assert.match(
      ref,
      /it\s+is\s*\n?\s*\*\*absent\*\*[\s\S]{0,280}Attribute each path before\s*\n?\s*staging it/,
      `${dir}/references/resume-assessment.md must attribute dirt only when the snapshot is absent`,
    )
    // Must agree with Step 5's clean-tree precondition: un-attributable dirt blocks, it does not
    // get dispatched on top of.
    assert.match(
      ref,
      /cannot be attributed with confidence blocks the run[\s\S]{0,200}BLOCKED/,
      `${dir}/references/resume-assessment.md must block on un-attributable dirt like Step 5 does`,
    )
  }

  // Mirror parity for the reference this ticket edits (SKILL.md parity is pinned by BOS-495).
  const [canonicalDir, pluginDir] = BUILD_MIRRORS
  const rel = 'references/resume-assessment.md'
  assert.equal(
    fs.readFileSync(path.join(rootDir, pluginDir, rel), 'utf8'),
    fs.readFileSync(path.join(rootDir, canonicalDir, rel), 'utf8'),
    `${pluginDir}/${rel} must be byte-identical to the canonical mirror (run \`make copy-skills\`)`,
  )
})
