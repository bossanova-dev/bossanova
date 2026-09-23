import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { markdownFilesUnder, scratchTokensInFiles } from '../scripts/plan-scratch-token-scan.mjs'
import {
  PLAN_SCRATCH_FAMILIES,
  PLAN_SCRATCH_ROOT,
  planScratchFamily,
  planScratchPath,
  planScratchToken,
  runScratchDir,
} from './plan-scratch-paths.mjs'

test('runScratchDir addresses scratch by run, under the scratch root', () => {
  assert.equal(runScratchDir('abc123'), '.linear-plans/run-abc123')
  assert.equal(runScratchDir('abc123', 'other-root'), 'other-root/run-abc123')
})

test('runScratchDir refuses a run id that could escape the scratch root', () => {
  assert.throws(() => runScratchDir(''), /requires a run id/)
  assert.throws(() => runScratchDir('  '), /requires a run id/)
  assert.throws(() => runScratchDir('..'), /invalid run id/)
  assert.throws(() => runScratchDir('a/b'), /invalid run id/)
})

test('planScratchPath builds every declared family inside this run directory', () => {
  const parts = {
    issueId: 'ABC-1',
    slug: 'ABC-1-a-title',
    parentId: 'ABC-9',
    key: 'k1',
    childId: 'ABC-2',
    n: '1',
  }
  for (const family of PLAN_SCRATCH_FAMILIES) {
    const path = planScratchPath('r1', family.family, parts)
    assert.ok(
      path.startsWith('.linear-plans/run-r1/'),
      `${family.family} resolved outside the run directory: ${path}`,
    )
    assert.equal(path.split('/').length, 3, `${family.family} nested below the run directory`)
    assert.doesNotMatch(path, /undefined/, `${family.family} left an unfilled template part`)
  }
})

test('planScratchPath refuses an undeclared family instead of inventing a name', () => {
  assert.throws(
    () => planScratchPath('r1', 'safe-orig', { issueId: 'ABC-1' }),
    /undeclared scratch family "safe-orig"/,
  )
})

test('planScratchPath refuses a family whose template parts were not supplied', () => {
  assert.throws(() => planScratchPath('r1', 'precheck', {}), /unusable basename/)
})

test('every declared family round-trips: its own template resolves back to it', () => {
  for (const family of PLAN_SCRATCH_FAMILIES) {
    const token = `${PLAN_SCRATCH_ROOT}/run-<RUN-SCRATCH-ID>/${family.template}`
    const result = planScratchToken(token)
    assert.ok(result.ok, `${family.family}: ${result.ok ? '' : result.reason}`)
    assert.deepEqual(
      result.families,
      [family.family],
      `${family.family} template resolved to ${JSON.stringify(result.families)}`,
    )
  }
})

test('every declared family round-trips from a concrete path too', () => {
  const parts = {
    issueId: 'ABC-1',
    slug: 'a-title',
    parentId: 'ABC-9',
    key: 'k1',
    childId: 'ABC-2',
    n: '1',
  }
  for (const family of PLAN_SCRATCH_FAMILIES) {
    const path = planScratchPath('r1', family.family, parts)
    const result = planScratchToken(path)
    assert.ok(result.ok, `${family.family}: ${result.ok ? '' : result.reason}`)
    assert.deepEqual(result.families, [family.family], `${family.family} → ${path}`)
  }
})

test('family keys and templates are unique', () => {
  const keys = PLAN_SCRATCH_FAMILIES.map((f) => f.family)
  const templates = PLAN_SCRATCH_FAMILIES.map((f) => f.template)
  assert.equal(new Set(keys).size, keys.length)
  assert.equal(new Set(templates).size, templates.length)
})

test('the four previously unnamed artifacts are declared families', () => {
  for (const family of [
    'attachment-guard-orig', // the guard safe source, once written as `<safe-orig.md>`
    'description', // the composed description, once `<new.md>` and a bare mktemp
    'deps-input', // the dependency-scan input, once `<the scratch JSON file you just wrote>`
    'epic-overview', // the epic parent overview, once composed with no path at all
  ]) {
    assert.ok(planScratchFamily(family), `${family} is not declared`)
  }
})

test('planScratchToken accepts the scratch root and this run directory', () => {
  assert.deepEqual(planScratchToken('.linear-plans'), { ok: true, kind: 'root', families: [] })
  assert.deepEqual(planScratchToken('.linear-plans/'), { ok: true, kind: 'root', families: [] })
  assert.deepEqual(planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>'), {
    ok: true,
    kind: 'run-dir',
    families: [],
  })
  assert.deepEqual(planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>/'), {
    ok: true,
    kind: 'run-dir',
    families: [],
  })
})

test('planScratchToken rejects an artifact that is not run-scoped', () => {
  const result = planScratchToken('.linear-plans/<ISSUE-ID>.precheck.json')
  assert.equal(result.ok, false)
  assert.match(result.reason, /must be run-scoped/)
})

test('planScratchToken rejects an ad-hoc name even inside the run directory', () => {
  for (const adhoc of [
    '<ISSUE-ID>.safe-orig.md',
    '<ISSUE-ID>.raw.json',
    '<ISSUE-ID>.desc-new.md',
  ]) {
    const result = planScratchToken(`.linear-plans/run-<RUN-SCRATCH-ID>/${adhoc}`)
    assert.equal(result.ok, false, `${adhoc} was accepted`)
    assert.match(result.reason, /no declared scratch family/)
  }
})

test('planScratchToken rejects nesting below the run directory', () => {
  const result = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/children/<ISSUE-ID>-<slug>.md',
  )
  assert.equal(result.ok, false)
  assert.match(result.reason, /nests below/)
})

test('planScratchToken expands brace alternation and requires every branch to resolve', () => {
  const ok = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.{precheck,draft-metadata,premises,premise-states}.json',
  )
  assert.deepEqual(ok, {
    ok: true,
    kind: 'artifact',
    families: ['precheck', 'draft-metadata', 'premises', 'premise-states'],
  })

  const bad = planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.{precheck,raw}.json')
  assert.equal(bad.ok, false)
  assert.match(bad.reason, /no declared scratch family/)
})

test('planScratchToken distinguishes child artifacts from their parent forms', () => {
  const child = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.child-<CHILD-ID>.image-guard-orig.md',
  )
  assert.deepEqual(child.families, ['child-image-guard-orig'])

  const parent = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md',
  )
  assert.deepEqual(parent.families, ['image-guard-orig'])

  const childPlan = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/<PARENT>-child-<key>-<slug>.md',
  )
  assert.deepEqual(childPlan.families, ['child-plan'])

  const plan = planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md')
  assert.deepEqual(plan.families, ['plan'])
})

test('BOS-1290: the two Phase 4 artifacts resolve in templated AND concrete form', () => {
  // Both steps need a file on disk and neither had a declared family, so the payload's own
  // "never invent a scratch filename" rule was unfollowable at exactly those two steps — which is
  // how a peer run came to write `BOS-1280.candidates-raw.json`, a name no cleanup can match.
  // Templated is how the payload cites a path; concrete is how it lands on disk. A family that
  // resolves in only one of the two forms leaves the other spelling undeclared.
  for (const [family, templated, concrete] of [
    [
      'write-description-descriptor',
      '<ISSUE-ID>.write-description.json',
      'BOS-1290.write-description.json',
    ],
    ['candidates', '<ISSUE-ID>.candidates.json', 'BOS-1290.candidates.json'],
  ]) {
    for (const basename of [templated, concrete]) {
      const result = planScratchToken(`.linear-plans/run-<RUN-SCRATCH-ID>/${basename}`)
      assert.ok(result.ok, `${basename}: ${result.ok ? '' : result.reason}`)
      assert.deepEqual(result.families, [family], `${basename} resolved to the wrong family`)
    }
    assert.equal(
      planScratchPath('r1', family, { issueId: 'BOS-1290' }),
      `.linear-plans/run-r1/${concrete}`,
      `${family} must build the concrete basename its own template describes`,
    )
  }

  // …and the spelling that was actually invented on disk still fails, so the declarations did not
  // widen the registry into accepting anything with a plausible shape.
  const invented = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.candidates-raw.json',
  )
  assert.equal(invented.ok, false)
  assert.match(invented.reason, /no declared scratch family/)
})

test('planScratchToken rejects a path outside the scratch root', () => {
  const result = planScratchToken('docs/plans/<ISSUE-ID>-<slug>.md')
  assert.equal(result.ok, false)
  assert.match(result.reason, /not a plan-scratch path/)
})

// ---------------------------------------------------------------------------
// The payload ratchet.
//
// The registry above is only worth having if the published skill actually uses
// it. This walks every markdown file in the published boss-plan payload, pulls
// out every `.linear-plans/…` path token it cites, and requires each to resolve
// through `planScratchToken`. It is the gate that fails on the next scratch path
// an agent was told to invent rather than to look up.
// ---------------------------------------------------------------------------

const PAYLOAD_DIR = fileURLToPath(
  new URL('../services/boss/internal/skillinstall/skills/boss-plan/', import.meta.url),
)

// The extraction rules live in scripts/plan-scratch-token-scan.mjs so that this gate and the
// payload contract gate in scripts/bs-plan-skill.test.mjs cannot disagree about what counts as a
// cited token — a second copy would let one of them miss a spelling and still report a clean scan.

/** @returns {{token: string, file: string, line: number}[]} */
export function payloadScratchTokens(files = markdownFilesUnder(PAYLOAD_DIR)) {
  return scratchTokensInFiles(files, { relativeTo: PAYLOAD_DIR })
}

test('the published payload cites at least one scratch path (the scan is not empty)', () => {
  const tokens = payloadScratchTokens()
  assert.ok(
    tokens.length > 20,
    `expected the payload to cite many scratch paths, found ${tokens.length} — ` +
      'an empty scan would make this ratchet vacuous',
  )
})

test('every .linear-plans token in the published payload resolves to a declared family', () => {
  const violations = []
  for (const { token, file, line } of payloadScratchTokens()) {
    const result = planScratchToken(token)
    if (!result.ok) violations.push(`${file}:${line}: ${result.reason}`)
  }
  assert.deepEqual(
    violations,
    [],
    `undeclared scratch paths in the published boss-plan payload:\n${violations.join('\n')}`,
  )
})

test('the ratchet rejects an ad-hoc token injected into payload text', () => {
  // Non-vacuity, checked in-process: the same scan over a file carrying an
  // invented name must fail. If this passes, the assertion above proves nothing.
  const dir = mkdtempSync(join(tmpdir(), 'payload-ratchet-'))
  const file = join(dir, 'SKILL.md')
  writeFileSync(
    file,
    'Write the snapshot to `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.safe-orig.md` before the guard.\n',
  )
  const tokens = payloadScratchTokens([file])
  assert.equal(tokens.length, 1)
  const result = planScratchToken(tokens[0].token)
  assert.equal(result.ok, false)
  assert.match(result.reason, /no declared scratch family/)
})

test('the ratchet rejects a flat, issue-scoped token injected into payload text', () => {
  const dir = mkdtempSync(join(tmpdir(), 'payload-ratchet-flat-'))
  const file = join(dir, 'SKILL.md')
  writeFileSync(file, 'Write it to `.linear-plans/<ISSUE-ID>.precheck.json` first.\n')
  const result = planScratchToken(payloadScratchTokens([file])[0].token)
  assert.equal(result.ok, false)
  assert.match(result.reason, /must be run-scoped/)
})

test('BOS-1278: the dispatch heartbeat is a declared family in both spellings', () => {
  // The liveness artifact an awaited dispatch touches while it works. Declaring it
  // here is what makes it reachable by cleanup and the TTL reap; a heartbeat
  // invented at the dispatch site would be a name no removal pattern matches.
  const family = planScratchFamily('dispatch-heartbeat')
  assert.equal(family.template, '<ISSUE-ID>.dispatch-heartbeat.json')
  assert.equal(family.basename({ issueId: 'BOS-1278' }), 'BOS-1278.dispatch-heartbeat.json')
  assert.equal(
    planScratchPath('r1', 'dispatch-heartbeat', { issueId: 'BOS-1278' }),
    '.linear-plans/run-r1/BOS-1278.dispatch-heartbeat.json',
  )
  for (const basename of [
    '<ISSUE-ID>.dispatch-heartbeat.json',
    'BOS-1278.dispatch-heartbeat.json',
  ]) {
    const result = planScratchToken(`.linear-plans/run-<RUN-SCRATCH-ID>/${basename}`)
    assert.ok(result.ok, `${basename}: ${result.ok ? '' : result.reason}`)
    assert.deepEqual(result.families, ['dispatch-heartbeat'])
  }
  // It is NOT the draft-metadata family it sits beside — a pattern that swallowed
  // its sibling would make cleanup and the token scan report the wrong artifact.
  assert.deepEqual(
    planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>/BOS-1278.draft-metadata.json').families,
    ['draft-metadata'],
  )
})
