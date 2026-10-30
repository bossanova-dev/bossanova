import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  rmSync,
  existsSync,
  readdirSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { spawnSync } from 'node:child_process'
import { planScratchPath } from './plan-scratch-paths.mjs'
import { issueSlug } from './plan-slug.mjs'
import { verifyDraftManifest, stageChildGuards } from './plan-draft-manifest.mjs'

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'plan-manifest-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  const scratchRoot = join(root, '.linear-plans')
  const dir = join(scratchRoot, 'run-test')
  mkdirSync(dir, { recursive: true })
  const file = (family, parts = {}) =>
    planScratchPath('test', family, { issueId: 'ABC-1', ...parts }, scratchRoot)
  const put = (path, text = 'plan bytes') => {
    writeFileSync(path, text)
    return path
  }
  const children = ['store', 'service', 'cli'].map((key) => ({ key, title: `Add ${key}` }))
  const ids = ['ABC-2', 'ABC-3', 'ABC-4']
  const guards = ['child-image-guard-orig', 'child-attachment-guard-orig', 'child-image-guard-new']
  const payload = {
    epic: true,
    epicParentId: 'ABC-1',
    childIds: ids,
    epicSpecPaths: [put(file('epic-spec'), JSON.stringify({ parentId: 'ABC-1', children }))],
    childPlanPaths: Object.fromEntries(
      ids.map((id, i) => [
        id,
        put(
          file('child-plan', {
            parentId: 'ABC-1',
            key: children[i].key,
            slug: issueSlug(id, children[i].title),
          }),
        ),
      ]),
    ),
    guardScratchPaths: ids.flatMap((childId) =>
      guards.map((family) => put(file(family, { childId }))),
    ),
  }
  return {
    root,
    dir,
    file,
    put,
    payload,
    verify: (p = payload) =>
      verifyDraftManifest({
        payload: p,
        planPath: file('plan', { slug: 'parent' }),
        runScratchDir: dir,
      }),
  }
}

test('single plan must exist and contain bytes', (t) => {
  const f = fixture(t)
  const path = f.file('plan', { slug: 'parent' })
  const verify = () => f.verify({ planPath: path })
  assert.equal(verify().ok, false)
  f.put(path, '')
  assert.equal(verify().ok, false)
  f.put(path)
  assert.equal(verify().ok, true)
  assert.equal(f.verify({}).ok, false)
})

test('complete three-child epic passes', (t) => assert.equal(fixture(t).verify().ok, true))

for (const index of [0, 1, 2])
  test(`every child guard is required (${index})`, (t) => {
    const f = fixture(t)
    f.payload.guardScratchPaths.splice(index, 1)
    assert.ok(f.verify().errors.some((e) => e.code === 'guardScratchPaths'))
  })

test('only original guards may be empty', (t) => {
  const f = fixture(t)
  f.put(f.payload.guardScratchPaths[0], '')
  f.put(f.payload.guardScratchPaths[1], '')
  assert.equal(f.verify().ok, true)
  f.put(f.payload.guardScratchPaths[2], '')
  assert.equal(f.verify().ok, false)
})

for (const kind of ['outside', 'directory', 'missing', 'empty-header'])
  test(`declared artifacts reject ${kind}`, (t) => {
    const f = fixture(t)
    const path = kind === 'outside' ? join(f.root, 'outside.md') : join(f.dir, 'extra.md')
    if (kind === 'directory') mkdirSync(path)
    else if (kind !== 'missing') f.put(path, kind === 'empty-header' ? '' : 'bytes')
    f.payload.attachmentHeaderPaths = [path]
    assert.equal(f.verify().ok, false)
  })

for (const kind of ['wrong-name', 'wrong-parent', 'invalid-json', 'no-spec', 'no-key', 'non-array'])
  test(`spec identity rejects ${kind}`, (t) => {
    const f = fixture(t)
    const path = f.payload.epicSpecPaths[0]
    if (kind === 'wrong-name')
      f.payload.epicSpecPaths = [f.put(join(f.dir, 'wrong.json'), readFileSync(path))]
    if (kind === 'wrong-parent') f.put(path, JSON.stringify({ parentId: 'ABC-99', children: [] }))
    if (kind === 'invalid-json') f.put(path, '{')
    if (kind === 'no-spec') f.payload.epicSpecPaths = []
    if (kind === 'no-key')
      f.put(path, JSON.stringify({ parentId: 'ABC-1', children: [{ title: 'Oops' }] }))
    if (kind === 'non-array') f.payload.epicSpecPaths = path
    assert.equal(f.verify().ok, false)
  })

for (const kind of [
  'wrong-basename',
  'duplicate',
  'count',
  'missing-map',
  'extra-map',
  'no-ids',
  'no-parent',
])
  test(`child identity rejects ${kind}`, (t) => {
    const f = fixture(t)
    if (kind === 'wrong-basename')
      f.payload.childPlanPaths['ABC-2'] = f.put(join(f.dir, 'wrong.md'))
    if (kind === 'duplicate') f.payload.childPlanPaths['ABC-3'] = f.payload.childPlanPaths['ABC-2']
    if (kind === 'count') f.payload.childIds.pop()
    if (kind === 'missing-map') delete f.payload.childPlanPaths['ABC-2']
    if (kind === 'extra-map') f.payload.childPlanPaths['ABC-99'] = f.payload.childPlanPaths['ABC-2']
    if (kind === 'no-ids') f.payload.childIds = []
    if (kind === 'no-parent') delete f.payload.epicParentId
    assert.equal(f.verify().ok, false)
  })

test('guards must be an array', (t) => {
  const f = fixture(t)
  f.payload.guardScratchPaths = f.payload.guardScratchPaths[0]
  assert.ok(f.verify().errors.some((e) => e.code === 'guardScratchPaths'))
})

test('staged created and adopted guards preserve bytes and verify', (t) => {
  const f = fixture(t)
  const prior = process.cwd()
  process.chdir(f.root)
  t.after(() => process.chdir(prior))
  f.put(f.file('image-guard-orig'), 'raw source\n')
  f.put(f.file('attachment-guard-orig'), 'safe source\n')
  const descriptionFile = f.put(
    f.file('child-description-draft', { key: 'store' }),
    'stored description\n',
  )
  for (const adopted of [false, true]) {
    const paths = stageChildGuards({
      runId: 'test',
      parentId: 'ABC-1',
      childId: 'ABC-2',
      descriptionFile,
      adopted,
    })
    assert.deepEqual(
      paths.map((path) => path.split('/').pop()),
      [
        'ABC-1.child-ABC-2.image-guard-orig.md',
        'ABC-1.child-ABC-2.attachment-guard-orig.md',
        'ABC-1.child-ABC-2.image-guard-new.md',
      ],
    )
    assert.equal(readFileSync(paths[0], 'utf8'), adopted ? '' : 'raw source\n')
    assert.equal(readFileSync(paths[1], 'utf8'), adopted ? '' : 'safe source\n')
    assert.equal(readFileSync(paths[2], 'utf8'), 'stored description\n')
    assert.equal(f.verify().ok, true)
  }
})

test('staging refuses missing sources or empty descriptions without partial files', (t) => {
  const f = fixture(t)
  const prior = process.cwd()
  process.chdir(f.root)
  t.after(() => process.chdir(prior))
  const descriptionFile = f.put(f.file('child-description-draft', { key: 'store' }), '')
  const before = readdirSync(f.dir)
  assert.throws(() =>
    stageChildGuards({ runId: 'test', parentId: 'ABC-1', childId: 'ABC-9', descriptionFile }),
  )
  assert.deepEqual(readdirSync(f.dir), before)
  f.put(descriptionFile)
  assert.throws(() =>
    stageChildGuards({ runId: 'test', parentId: 'ABC-1', childId: 'ABC-9', descriptionFile }),
  )
  assert.equal(existsSync(f.file('child-image-guard-new', { childId: 'ABC-9' })), false)
})

test('CLI distinguishes valid, invalid and usage exits', (t) => {
  const f = fixture(t)
  const cli = resolve('skills-toolbox/plan/plan-draft-manifest.mjs')
  const args = [
    'verify',
    '--payload',
    JSON.stringify(f.payload),
    '--plan-path',
    f.file('plan', { slug: 'parent' }),
    '--run-scratch',
    f.dir,
  ]
  assert.equal(spawnSync(process.execPath, [cli, ...args]).status, 0)
  f.payload.guardScratchPaths.pop()
  args[2] = JSON.stringify(f.payload)
  const bad = spawnSync(process.execPath, [cli, ...args, '--label', 'dispatch-failure'], {
    encoding: 'utf8',
  })
  assert.equal(bad.status, 1)
  assert.match(
    bad.stderr,
    /dispatch-failure: sentinel ok but artifact missing\/empty or wrong path \(guardScratchPaths\)/,
  )
  assert.equal(spawnSync(process.execPath, [cli, '--unknown']).status, 2)
})
