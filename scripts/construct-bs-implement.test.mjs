import assert from 'node:assert/strict'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import fs from 'node:fs'
import { constructSkill, constructReference, skillsRootAvailable } from './construct-skills.mjs'

const rootDir = fileURLToPath(new URL('..', import.meta.url))
const manifest = JSON.parse(
  fs.readFileSync(path.join(rootDir, '.claude/skills/boss-implement/construct.json'), 'utf8'),
)
// Construction reads the component skills from the installed superpowers plugin.
// Skip (don't throw at import) where it is absent — e.g. a fresh CI runner.
const sourceSkip =
  !skillsRootAvailable(manifest) && `superpowers ${manifest.superpowers_version} not installed`
const out = sourceSkip ? '' : constructSkill(manifest, { rootDir })

function referenceById(id) {
  const reference = manifest.references.find((entry) => entry.id === id)
  assert.ok(reference, `manifest must declare a reference for "${id}"`)
  return constructReference(manifest, reference, { rootDir })
}

test(
  'SDD spine is moved to a reference (not inlined) and pointed to from the body',
  { skip: sourceSkip },
  () => {
    // De-inlined: the spine text no longer lives in the always-resident body.
    assert.doesNotMatch(out, /fresh implementer subagent per task/)
    assert.doesNotMatch(out, /Skill\(subagent-driven-development\)/)
    // Still reachable: the body points at the generated reference…
    assert.match(out, /references\/subagent-driven-development\.md/)
    // …and the reference carries the spine.
    assert.match(
      referenceById('subagent-driven-development'),
      /fresh implementer subagent per task/,
    )
  },
)

test(
  'reviewer template + fix discipline are moved to references (not inlined)',
  { skip: sourceSkip },
  () => {
    assert.doesNotMatch(out, /Senior Code Reviewer/)
    assert.match(referenceById('code-reviewer-template'), /Senior Code Reviewer/)
    assert.match(referenceById('receiving-code-review'), /receiving code review/i)
  },
)

test('SDD final-review + lifecycle siblings are stripped', { skip: sourceSkip }, () => {
  const sdd = referenceById('subagent-driven-development')
  assert.doesNotMatch(sdd, /Required workflow skills/)
  assert.doesNotMatch(sdd, /finishing-a-development-branch/)
})

test('scar tissue is gone', { skip: sourceSkip }, () => {
  const sdd = referenceById('subagent-driven-development')
  assert.doesNotMatch(sdd, /sdd-ledger\.mjs/)
  assert.doesNotMatch(sdd, /single controller/i)
  assert.doesNotMatch(sdd, /phantom peer/i)
})

test('headless mode + mode-aware proof are present', { skip: sourceSkip }, () => {
  assert.match(out, /BS_HEADLESS/)
  assert.match(out, /proof\.mjs plan/)
})

// The always-resident body is bounded on BOTH sides: a hard cap (the ratchet) and a
// floor (real headroom). BOS-148 restored headroom by moving the status-rollback +
// red-flags lookup into references/troubleshooting.md; the floor stops the next
// addition from silently landing back at the cap.
const BODY_CAP_BYTES = 40960 // <40 KB — BOS-134 ratchet; never raise it.
const BODY_FLOOR_BYTES = 39936 // <39 KB headroom — BOS-148; keep the body well below the cap.
const RESIDENT_BODY_SKILLS = [
  '.claude/skills/boss-implement/SKILL.md',
  '.codex/skills/boss-implement/SKILL.md',
]

test('the always-resident body stays under the <40KB acceptance target', () => {
  // BOS-134 acceptance criterion: the always-resident body is < 40 KB (40960 bytes).
  // The pre-split constructed body was ~96 KB (all four component skills inlined); the
  // split moves them behind references. Enforcing the actual target here (not a loose
  // ceiling) makes the criterion executable: any future addition that would breach 40 KB
  // must go into a reference, not the resident body.
  for (const skillPath of RESIDENT_BODY_SKILLS) {
    const committed = fs.readFileSync(path.join(rootDir, skillPath), 'utf8')
    const byteLength = Buffer.byteLength(committed, 'utf8')
    assert.ok(
      byteLength < BODY_CAP_BYTES,
      `${skillPath} must stay under the <40KB target (${BODY_CAP_BYTES} bytes), got ${byteLength} bytes — move situational content into a reference`,
    )
  }
})

test('the always-resident body keeps real headroom under the ratchet (BOS-148 floor)', () => {
  // A floor beneath the cap: the constructed body must stay well below 40 KB so the next
  // situational addition has room without re-hitting the cliff. If this fails because the
  // body grew, move a situational section into a reference — do NOT relax the floor.
  for (const skillPath of RESIDENT_BODY_SKILLS) {
    const committed = fs.readFileSync(path.join(rootDir, skillPath), 'utf8')
    const byteLength = Buffer.byteLength(committed, 'utf8')
    assert.ok(
      byteLength < BODY_FLOOR_BYTES,
      `${skillPath} must keep headroom under the <39KB floor (${BODY_FLOOR_BYTES} bytes), got ${byteLength} bytes — move situational content into a reference, never raise the floor`,
    )
  }
})

test('every moved reference is reachable: a body pointer plus an existing file', () => {
  const skillDirs = [
    path.join(rootDir, '.claude/skills/boss-implement'),
    path.join(rootDir, '.codex/skills/boss-implement'),
  ]
  const references = [
    'references/core-spine.md',
    'references/subagent-driven-development.md',
    'references/test-driven-development.md',
    'references/code-reviewer-template.md',
    'references/receiving-code-review.md',
    'references/review-stack.md',
    'references/proof-capture.md',
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
  for (const skillDir of ['.claude/skills/boss-implement', '.codex/skills/boss-implement']) {
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

  for (const skillDir of ['.claude/skills/boss-implement', '.codex/skills/boss-implement']) {
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

test('runtime helper references are local to generated Claude and Codex skills', () => {
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
    path.join(rootDir, '.claude/skills/boss-implement'),
    path.join(rootDir, '.codex/skills/boss-implement'),
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
// (scripts/finalize/cli.mjs inject-pr-tag, which delegates to add-pr-numbers.sh); the assertions
// track that command while preserving the BOS-181 order/guard/ready invariants.
const claudeBody = () =>
  fs.readFileSync(path.join(rootDir, '.claude/skills/boss-implement/SKILL.md'), 'utf8')

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

// BOS-240: boss-implement must finalize BLOCKED (not REVIEW_READY) when it defers a *required*
// item at the wall-clock cap. These assertions pin the honest-finalize invariant into the
// shipped Claude and Codex artifacts so the terminal-state logic can't silently regress. They
// read the committed generated files (no source skip) so CI enforces them without superpowers.
const RESIDENT_BODIES = {
  claude: '.claude/skills/boss-implement/SKILL.md',
  codex: '.codex/skills/boss-implement/SKILL.md',
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
