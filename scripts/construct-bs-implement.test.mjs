import assert from 'node:assert/strict'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import fs from 'node:fs'
import { constructSkill, constructReference, skillsRootAvailable } from './construct-skills.mjs'

const rootDir = fileURLToPath(new URL('..', import.meta.url))
const manifest = JSON.parse(
  fs.readFileSync(path.join(rootDir, '.claude/skills/bs-implement/construct.json'), 'utf8'),
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
  '.claude/skills/bs-implement/SKILL.md',
  '.codex/skills/bs-implement/SKILL.md',
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
    path.join(rootDir, '.claude/skills/bs-implement'),
    path.join(rootDir, '.codex/skills/bs-implement'),
  ]
  const references = [
    'references/subagent-driven-development.md',
    'references/test-driven-development.md',
    'references/code-reviewer-template.md',
    'references/receiving-code-review.md',
    'references/review-stack.md',
    'references/proof-capture.md',
    'references/cron-gate.md',
    'references/troubleshooting.md',
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

test('Step 6c bs-review sentinel is advisory and cannot drive the run-file verdict', () => {
  for (const skillDir of ['.claude/skills/bs-implement', '.codex/skills/bs-implement']) {
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
    path.join(rootDir, '.claude/skills/bs-implement'),
    path.join(rootDir, '.codex/skills/bs-implement'),
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
