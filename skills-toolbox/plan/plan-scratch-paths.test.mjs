import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { execFileSync, spawnSync } from 'node:child_process'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  markdownFilesUnder,
  scratchTokensInFiles,
} from '../../scripts/skills/plan-scratch-token-scan.mjs'
import {
  ensurePlanScratchRoot,
  PLAN_SCRATCH_FAMILIES,
  PLAN_SCRATCH_ROOT,
  planScratchBasename,
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
    title: 'A title',
    parentId: 'ABC-9',
    key: 'k1',
    childId: 'ABC-2',
    n: '1',
    role: 'plan-reviewer',
    name: 'boss-plan-feature-map',
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

test('scratch paths accept literal undefined text while rejecting missing consumed parts', () => {
  const parts = {
    issueId: 'ABC-1',
    title: 'handle-undefined-values',
    parentId: 'ABC-9',
    key: 'undefined',
    childId: 'ABC-2',
    n: '1',
    role: 'plan-reviewer',
    name: 'undefined-check',
  }
  for (const family of PLAN_SCRATCH_FAMILIES) {
    const path = planScratchPath('r1', family.family, parts)
    assert.equal(path.split('/').length, 3)
    const consumed = new Set()
    family.basename(
      new Proxy(parts, {
        get(target, key) {
          consumed.add(key)
          return target[key]
        },
      }),
    )
    for (const key of consumed) {
      const missing = { ...parts }
      delete missing[key]
      assert.throws(() => planScratchPath('r1', family.family, missing), /unusable basename/)
      assert.throws(
        () => planScratchPath('r1', family.family, { ...parts, [key]: undefined }),
        /unusable basename/,
      )
      if (key !== 'title')
        assert.throws(
          () => planScratchPath('r1', family.family, { ...parts, [key]: 'a/b' }),
          /unusable basename/,
        )
    }
  }
  assert.equal(
    planScratchPath('r1', 'plan', parts),
    '.linear-plans/run-r1/ABC-1-handle-undefined-values.md',
  )
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
    title: 'a-title',
    parentId: 'ABC-9',
    key: 'k1',
    childId: 'ABC-2',
    n: '1',
    role: 'plan-reviewer',
    name: 'boss-plan-feature-map',
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

  const childPlan = planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>/<PARENT>-child-<key>.md')
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
  new URL('../../lib/bossalib/contract/plugin/skillpayload/skills/boss-plan/', import.meta.url),
)

// The extraction rules live in scripts/skills/plan-scratch-token-scan.mjs.

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
  const dir = tempDir('payload-ratchet-')
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
  const dir = tempDir('payload-ratchet-flat-')
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

test('BOS-1335: the epic reverify bundle and child stored read-back are declared families', () => {
  // The epic acceptance gate reads a hydrated bundle and one stored description per live child.
  // Each must resolve to its own family in templated AND concrete form, and the child stored form
  // must not be swallowed by — nor swallow — the parent `image-guard-stored` form it shares a
  // suffix with.
  for (const [family, templated, concrete, parts] of [
    [
      'epic-reverify',
      '<ISSUE-ID>.epic-reverify.json',
      'BOS-1.epic-reverify.json',
      { issueId: 'BOS-1' },
    ],
    [
      'child-image-guard-stored',
      '<ISSUE-ID>.child-<CHILD-ID>.image-guard-stored.md',
      'BOS-1.child-BOS-2.image-guard-stored.md',
      { issueId: 'BOS-1', childId: 'BOS-2' },
    ],
  ]) {
    for (const basename of [templated, concrete]) {
      const result = planScratchToken(`.linear-plans/run-X/${basename}`)
      assert.ok(result.ok, `${basename}: ${result.ok ? '' : result.reason}`)
      assert.deepEqual(result.families, [family], `${basename} resolved to the wrong family`)
    }
    assert.equal(planScratchPath('X', family, parts), `.linear-plans/run-X/${concrete}`)
  }
  assert.deepEqual(planScratchToken('.linear-plans/run-X/BOS-1.image-guard-stored.md').families, [
    'image-guard-stored',
  ])
})

test('BOS-1374: the child fan-out route, ledger and child precheck are declared families', () => {
  // The fan-out orchestrator writes the hydrated route bundle, its ledger, and one post-settle
  // idempotence payload per child. Each must resolve to its own family in templated AND concrete
  // form, and the child precheck must not be swallowed by — nor swallow — the parent `precheck`.
  for (const [family, templated, concrete, parts] of [
    [
      'fanout-route',
      '<ISSUE-ID>.fanout-route.json',
      'ABC-1.fanout-route.json',
      { issueId: 'ABC-1' },
    ],
    [
      'fanout-ledger',
      '<ISSUE-ID>.fanout-ledger.json',
      'ABC-1.fanout-ledger.json',
      { issueId: 'ABC-1' },
    ],
    [
      'child-precheck',
      '<ISSUE-ID>.child-<CHILD-ID>.precheck.json',
      'ABC-1.child-ABC-2.precheck.json',
      { issueId: 'ABC-1', childId: 'ABC-2' },
    ],
  ]) {
    assert.equal(planScratchFamily(family).template, templated)
    for (const basename of [templated, concrete]) {
      const result = planScratchToken(`.linear-plans/run-X/${basename}`)
      assert.ok(result.ok, `${basename}: ${result.ok ? '' : result.reason}`)
      assert.deepEqual(result.families, [family], `${basename} resolved to the wrong family`)
    }
    assert.equal(planScratchPath('X', family, parts), `.linear-plans/run-X/${concrete}`)
  }
  assert.deepEqual(planScratchToken('.linear-plans/run-X/ABC-1.precheck.json').families, [
    'precheck',
  ])
  assert.throws(() => planScratchPath('X', 'child-precheck', { issueId: 'ABC-1' }), /unusable/)
})

test('planning race scratch is declared without accepting neighbouring names', () => {
  for (const family of ['recheck', 'claim-comments']) {
    const file = planScratchPath('one', family, { issueId: 'TCK-1' })
    assert.equal(planScratchToken(file).ok, true)
    assert.equal(planScratchToken(file.replace('.json', '-extra.json')).ok, false)
  }
})

test('BOS-1965: extension envelopes have run-scoped paths distinct per extension', () => {
  const parts = { issueId: 'BOS-1', role: 'plan-reviewer', name: 'boss-plan-feature-map' }
  for (const [family, suffix] of [
    ['extension-out', 'json'],
    ['extension-tmp', 'tmp'],
    ['extension-heartbeat', 'heartbeat.json'],
  ]) {
    const path = planScratchPath('r1', family, parts)
    assert.equal(
      path,
      `.linear-plans/run-r1/BOS-1.ext-plan-reviewer-boss-plan-feature-map.${suffix}`,
    )
    assert.equal(join(path, '..'), runScratchDir('r1'))
    assert.notEqual(path, planScratchPath('r2', family, parts))
    assert.notEqual(path, planScratchPath('r1', family, { ...parts, name: 'other-reviewer' }))
  }
})

test('BOS-1965: extension output and temporary tokens classify without overlap', () => {
  for (const [family, suffix] of [
    ['extension-out', 'json'],
    ['extension-tmp', 'tmp'],
    ['extension-heartbeat', 'heartbeat.json'],
  ]) {
    for (const token of [
      `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.ext-plan-reviewer-<name>.${suffix}`,
      `.linear-plans/run-X/BOS-1.ext-plan-reviewer-boss-plan-feature-map.${suffix}`,
    ]) {
      assert.deepEqual(planScratchToken(token), { ok: true, kind: 'artifact', families: [family] })
    }
  }
})

test('BOS-1965: extension paths refuse missing role or name', () => {
  for (const family of ['extension-out', 'extension-tmp', 'extension-heartbeat']) {
    for (const parts of [
      { issueId: 'BOS-1', name: 'x' },
      { issueId: 'BOS-1', role: 'plan-reviewer' },
    ]) {
      assert.throws(() => planScratchPath('r1', family, parts), /unusable basename/)
    }
  }
})

test('BOS-1965: CLI builds extension paths and lists both families', () => {
  const cli = fileURLToPath(new URL('./plan-scratch-paths.mjs', import.meta.url))
  for (const family of ['extension-out', 'extension-tmp', 'extension-heartbeat']) {
    const result = spawnSync(
      process.execPath,
      [cli, 'path', 'r1', family, 'issueId=BOS-1', 'role=plan-reviewer', 'name=x'],
      { encoding: 'utf8' },
    )
    assert.equal(result.status, 0, result.stderr)
    assert.equal(
      result.stdout.trim(),
      planScratchPath('r1', family, {
        issueId: 'BOS-1',
        role: 'plan-reviewer',
        name: 'x',
      }),
    )
  }
  const listed = spawnSync(process.execPath, [cli, 'families'], { encoding: 'utf8' })
  assert.equal(listed.status, 0, listed.stderr)
  const keys = listed.stdout
    .trim()
    .split('\n')
    .map((line) => line.split('\t')[0])
  assert.ok(keys.includes('extension-out'))
  assert.ok(keys.includes('extension-tmp'))
  assert.ok(keys.includes('extension-heartbeat'))
})

test('plan scratch stays untracked until ensuring its self-ignoring root', () => {
  const repo = tempDir('plan-scratch-git-')
  execFileSync('git', ['init', '--quiet', repo])
  const root = join(repo, '.linear-plans')
  const run = join(root, 'run-abc')
  mkdirSync(run, { recursive: true })
  writeFileSync(join(run, 'BOS-1-x.md'), 'plan')
  const status = () =>
    execFileSync('git', ['status', '--porcelain', '--untracked-files=all'], {
      cwd: repo,
      encoding: 'utf8',
    })
  assert.match(status(), /\?\? \.linear-plans\/run-abc\/BOS-1-x\.md/)
  assert.deepEqual(ensurePlanScratchRoot(root), { root, created: true })
  assert.equal(status(), '')
})

test('ensuring plan scratch is idempotent and preserves customised ignore files', () => {
  const root = join(tempDir('plan-scratch-ignore-'), '.linear-plans')
  assert.deepEqual(ensurePlanScratchRoot(root), { root, created: true })
  const ignore = join(root, '.gitignore')
  const original = readFileSync(ignore, 'utf8')
  assert.deepEqual(ensurePlanScratchRoot(root), { root, created: false })
  assert.equal(readFileSync(ignore, 'utf8'), original)
  writeFileSync(ignore, '# custom rules\nrun-*\n')
  assert.deepEqual(ensurePlanScratchRoot(root), { root, created: false })
  assert.equal(readFileSync(ignore, 'utf8'), '# custom rules\nrun-*\n')
})

test('ensure-root CLI creates a self-ignoring scratch root', () => {
  const cwd = tempDir('plan-scratch-cli-')
  const cli = fileURLToPath(new URL('./plan-scratch-paths.mjs', import.meta.url))
  const output = execFileSync(process.execPath, [cli, 'ensure-root'], { cwd, encoding: 'utf8' })
  assert.equal(output.trim(), '.linear-plans')
  assert.match(readFileSync(join(cwd, '.linear-plans', '.gitignore'), 'utf8'), /^\*$/m)
})

test('precreation child drafts have distinct declared names', () => {
  for (const [family, template, concrete] of [
    [
      'child-description-draft',
      '<ISSUE-ID>.child-key-<key>.description.md',
      'ABC-1.child-key-add-the-store.description.md',
    ],
    [
      'child-plan-draft',
      '<ISSUE-ID>.child-key-<key>.plan.md',
      'ABC-1.child-key-add-the-store.plan.md',
    ],
    ['description', '<ISSUE-ID>.description.md', 'ABC-1.description.md'],
    ['child-plan', '<PARENT>-child-<key>.md', 'ABC-1-child-store-ABC-2-add-store.md'],
  ]) {
    for (const name of [template, concrete])
      assert.deepEqual(planScratchToken(`.linear-plans/run-r1/${name}`).families, [family])
  }
  assert.equal(planScratchToken('.linear-plans/run-r1/<ISSUE-ID>.child-desc-<slug>.md').ok, false)
})

test('open-prs scratch family admits only its issue snapshot filename', () => {
  assert.equal(
    planScratchPath('r1', 'open-prs', { issueId: 'ABC-1' }),
    '.linear-plans/run-r1/ABC-1.open-prs.json',
  )
  assert.deepEqual(planScratchToken('.linear-plans/run-r1/ABC-1.open-prs.json').families, [
    'open-prs',
  ])
  for (const name of ['ABC-1.open-prs.md', 'ABC-1.open-prs.extra.json', 'ABC-1.other.json']) {
    assert.equal(planScratchToken(`.linear-plans/run-r1/${name}`).ok, false)
  }
})

test('families declares a persistent premise report for writeback verification', () => {
  assert.equal(
    planScratchPath('r1', 'premise-report', { issueId: 'ABC-1' }),
    '.linear-plans/run-r1/ABC-1.premise-report.json',
  )
  const cli = spawnSync(
    process.execPath,
    [fileURLToPath(new URL('./plan-scratch-paths.mjs', import.meta.url)), 'families'],
    { encoding: 'utf8' },
  )
  assert.equal(cli.status, 0, cli.stderr)
  assert.match(cli.stdout, /^premise-report\t<ISSUE-ID>\.premise-report\.json\t/m)
})

test('returned metadata has a declared family and is accepted by the CLI', () => {
  const result = spawnSync(
    process.execPath,
    ['skills-toolbox/plan/plan-scratch-paths.mjs', 'families'],
    { encoding: 'utf8' },
  )
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /^draft-returned\t/m)
  const checked = spawnSync(
    process.execPath,
    [
      'skills-toolbox/plan/plan-scratch-paths.mjs',
      'check',
      '.linear-plans/run-one/ABC-1.draft-returned.json',
    ],
    { encoding: 'utf8' },
  )
  assert.equal(checked.status, 0, checked.stderr)
})

test('claim liveness is addressed under the current run and selected issue', () => {
  assert.equal(
    planScratchPath('run-1', 'claim-liveness', { issueId: 'APP-1' }),
    '.linear-plans/run-run-1/APP-1.claim-liveness.json',
  )
  assert.equal(planScratchToken('.linear-plans/run-run-1/APP-1.claim-liveness.json').ok, true)
})

for (const [family, suffix] of [
  ['child-plan', '.md'],
  ['child-plan-rejected', '.md.rejected'],
]) {
  test(`${family} uses the parent and spec key once`, () => {
    assert.equal(
      planScratchPath('r1', family, { parentId: 'ABC-9', key: 'add-the-store' }),
      `.linear-plans/run-r1/ABC-9-child-add-the-store${suffix}`,
    )
  })
}
for (const [family, suffix] of [
  ['plan', '.md'],
  ['plan-rejected', '.md.rejected'],
]) {
  test(`${family} computes the issue slug from title`, () => {
    assert.equal(
      planScratchPath('r1', family, { issueId: 'abc-12', title: 'Add the store!' }),
      `.linear-plans/run-r1/ABC-12-add-the-store${suffix}`,
    )
    assert.throws(() => planScratchPath('r1', family, { issueId: 'ABC-12' }))
  })
}
test('every family rejects a caller-supplied slug', () => {
  const parts = {
    issueId: 'ABC-1',
    title: 'A title',
    parentId: 'ABC-9',
    key: 'store',
    childId: 'ABC-2',
    n: '1',
    role: 'draft',
    name: 'example',
  }
  for (const { family } of PLAN_SCRATCH_FAMILIES) {
    assert.equal(typeof planScratchBasename(family, parts), 'string')
    assert.throws(() => planScratchPath('r1', family, { ...parts, slug: 'x' }))
    assert.throws(() => planScratchBasename(family, { ...parts, slug: 'x' }))
  }
  const result = spawnSync(
    process.execPath,
    ['skills-toolbox/plan/plan-scratch-paths.mjs', 'path', 'r1', 'plan', 'issueId=ABC-1', 'slug=x'],
    { encoding: 'utf8' },
  )
  assert.notEqual(result.status, 0)
  assert.equal(result.stdout, '')
})
test('legacy concrete child names classify but doubled templates do not', () => {
  assert.deepEqual(
    planScratchToken('.linear-plans/run-r1/ABC-1-child-store-ABC-2-add-store.md').families,
    ['child-plan'],
  )
  assert.equal(planScratchToken('.linear-plans/run-r1/<PARENT>-child-<key>-<slug>.md').ok, false)
})

test('epic reverify payload paths classify parent, listing and child inputs', () => {
  for (const id of ['ABC-1', '<ISSUE-ID>']) {
    for (const suffix of ['parent', 'children', 'child-ABC-2', 'child-<CHILD-ID>']) {
      const token = planScratchToken(
        `.linear-plans/run-r1/${id}.epic-reverify-payload-${suffix}.json`,
      )
      assert.deepEqual(token.families, ['epic-reverify-payload'])
    }
  }
  assert.equal(
    planScratchPath('r1', 'epic-reverify-payload', { issueId: 'ABC-1', part: 'parent' }),
    '.linear-plans/run-r1/ABC-1.epic-reverify-payload-parent.json',
  )
  assert.equal(
    planScratchPath('r1', 'epic-reverify-payload', { issueId: 'ABC-1', childId: 'ABC-2' }),
    '.linear-plans/run-r1/ABC-1.epic-reverify-payload-child-ABC-2.json',
  )
})

test('claim receipt is declared for templated and concrete run paths', () => {
  assert.equal(planScratchFamily('claim-receipt').template, '<ISSUE-ID>.claim-receipt.json')
  assert.equal(
    planScratchPath('abc123', 'claim-receipt', { issueId: 'ABC-1' }),
    '.linear-plans/run-abc123/ABC-1.claim-receipt.json',
  )
  for (const token of [
    '.linear-plans/run-<RUN-ID>/<ISSUE-ID>.claim-receipt.json',
    '.linear-plans/run-test/ABC-1.claim-receipt.json',
  ]) {
    assert.deepEqual(planScratchToken(token).families, ['claim-receipt'])
  }
})
