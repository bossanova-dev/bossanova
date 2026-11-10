import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'

const toolbox = resolve('skills-toolbox')
const runner = join(toolbox, 'plan/boss-plan-run.sh')
const issue = 'TCK-1'
const notes =
  'Keep reporter context and implementation details for this substantive saved description.\n'
const description = `## Summary\n\nPreserve source bytes and verify the complete drafting artifacts before publication.\n\n## Original notes\n\n${notes}`
function fixture(t, shell = 'bash') {
  const dir = tempDir('plan-reference-')
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  mkdirSync(join(dir, 'app'))
  writeFileSync(join(dir, 'app/example.txt'), 'fixture')
  for (const args of [
    ['init', '-q'],
    ['add', 'app/example.txt'],
    [
      '-c',
      'commit.gpgsign=false',
      '-c',
      'user.name=Fixture',
      '-c',
      'user.email=fixture@example.invalid',
      'commit',
      '-qm',
      'fixture',
    ],
  ]) {
    const r = spawnSync('git', args, { cwd: dir, encoding: 'utf8' })
    assert.equal(r.status, 0, r.stderr)
  }
  const file = (suffix) => join(dir, `${issue}.${suffix}`)
  const put = (suffix, bytes) => writeFileSync(file(suffix), bytes)
  const env = { ...process.env, TMPDIR: dir, BOSS_GATE_OUTCOME_FILE: join(dir, 'outcomes.tsv') }
  if (shell === '/bin/bash') {
    const bin = join(dir, 'bin')
    mkdirSync(bin)
    symlinkSync(shell, join(bin, 'bash'))
    env.PATH = `${bin}:${process.env.PATH}`
  }
  const run = (step, ...args) =>
    spawnSync(shell, [runner, step, ...args], {
      cwd: dir,
      encoding: 'utf8',
      env,
    })
  const ctx = run('sentinel-ctx')
  assert.equal(ctx.status, 0, ctx.stderr)
  const values = Object.fromEntries(
    ctx.stdout
      .trim()
      .split('\n')
      .map((line) => {
        const i = line.indexOf('=')
        return [line.slice(0, i), line.slice(i + 1)]
      }),
  )
  const args = [
    '--run-scratch',
    dir,
    '--issue',
    issue,
    '--run-dir',
    values.RUN_DIR,
    '--run-id',
    values.RUN_ID,
  ]
  const sentinel = () =>
    spawnSync(
      'node',
      [join(toolbox, 'bs-run-sentinel.mjs'), 'read', values.RUN_DIR, values.RUN_ID, 'draft'],
      { encoding: 'utf8' },
    )
  put('image-guard-orig.md', notes)
  put('attachment-guard-orig.md', notes)
  put('description.md', description)
  put('plan.md', description)
  return { dir, file, put, run, args, sentinel, values }
}
test('run-tmp creates design paths and confines removal to its own temp namespace', (t) => {
  const f = fixture(t)
  const r = f.run('run-tmp', 'create', '--design', issue)
  assert.equal(r.status, 0, r.stderr)
  const rows = Object.fromEntries(
    r.stdout
      .trim()
      .split('\n')
      .map((line) => line.split('=')),
  )
  assert.ok(existsSync(rows.RUN_TMP))
  assert.equal(rows.DESIGN_DOC, join(rows.RUN_TMP, 'design', `${issue}-design.md`))
  assert.ok(existsSync(join(rows.RUN_TMP, 'design')))
  for (const target of ['/', process.env.HOME, f.dir, join(rows.RUN_TMP, '..')]) {
    const refused = f.run('run-tmp', 'remove', '--path', target)
    assert.equal(refused.status, 64, refused.stderr)
    assert.ok(existsSync(target))
  }
  assert.equal(f.run('run-tmp', 'remove', '--path', rows.RUN_TMP).status, 0)
  assert.equal(existsSync(rows.RUN_TMP), false)
})
test('append-original-notes refuses incomplete headings and preserves arbitrary snapshot bytes', (t) => {
  const f = fixture(t)
  for (const snapshot of [notes, "dollar $ and `backticks` and 'quote'"]) {
    f.put('image-guard-orig.md', snapshot)
    f.put('description.md', 'invalid')
    const args = ['--body', f.file('description.md'), '--snapshot', f.file('image-guard-orig.md')]
    assert.equal(f.run('append-original-notes', ...args).status, 1)
    assert.equal(readFileSync(f.file('description.md'), 'utf8'), 'invalid')
    f.put('description.md', '## Summary\n\nSummary.\n\n## Original notes\n\n')
    assert.equal(f.run('append-original-notes', ...args).status, 0)
    assert.ok(
      readFileSync(f.file('description.md'))
        .subarray(-Buffer.byteLength(snapshot))
        .equals(Buffer.from(snapshot)),
    )
  }
})
test('draft-verify uses system Bash to reject dropped images, then publish checked premises', (t) => {
  const f = fixture(t, '/bin/bash')
  f.put('image-guard-orig.md', notes + '![diagram](https://example.invalid/diagram.png)\n')
  const args = [...f.args, '--plan', f.file('plan.md')]
  assert.notEqual(f.run('draft-verify', ...args).status, 0)
  assert.equal(JSON.parse(f.sentinel().stdout).status, 'missing')
  f.put('image-guard-orig.md', notes)
  f.put('premises.json', JSON.stringify([{ id: 'TCK-2', state: 'Done' }]))
  const r = f.run('draft-verify', ...args)
  assert.equal(r.status, 0, r.stderr)
  const stored = JSON.parse(f.sentinel().stdout)
  assert.equal(stored.kind, 'ok')
  assert.deepEqual(stored.payload.premises, [{ id: 'TCK-2', state: 'Done' }])
})
test('dispatch-wait forwards leg-deadline to the await helper', (t) => {
  const f = fixture(t)
  const fake = join(f.dir, 'toolbox')
  mkdirSync(fake)
  writeFileSync(
    join(fake, 'bs-dispatch-await.mjs'),
    'process.stdout.write(JSON.stringify(process.argv.slice(2)))',
  )
  writeFileSync(join(f.values.RUN_DIR, 'ext-reviewer.dispatched-at'), '1234')
  const r = spawnSync(
    'bash',
    [
      join(toolbox, 'plan/run-steps/dispatch-wait.sh'),
      '--run-dir',
      f.values.RUN_DIR,
      '--run-id',
      f.values.RUN_ID,
      '--name',
      'ext-reviewer',
      '--heartbeat',
      f.file('heartbeat.json'),
      '--leg-deadline',
    ],
    { encoding: 'utf8', env: { ...process.env, BOSS_PLAN_TOOLBOX: fake } },
  )
  assert.equal(r.status, 0, r.stderr)
  assert.ok(JSON.parse(r.stdout).includes('--leg-deadline'))
})

test('epic-verify uses system Bash to check child guards and canonical plan paths', async (t) => {
  const f = fixture(t, '/bin/bash')
  const { epicChildMarker } = await import('./plan-epic-lib.mjs')
  const children = [
    { key: 'a', title: 'First change' },
    { key: 'b', title: 'Next change' },
  ]
  f.put('epic-spec.json', JSON.stringify({ parentId: issue, children }))
  f.put(
    'epic-overview.md',
    description.replace(
      '## Original notes',
      '## Child tickets\n\n- TCK-2: First change\n- TCK-3: Next change\n\n## Original notes',
    ),
  )
  const expected = {}
  for (const [i, child] of children.entries()) {
    const id = `TCK-${i + 2}`
    expected[id] = join(f.dir, `${issue}-child-${child.key}.md`)
    writeFileSync(expected[id], description)
    for (const suffix of ['image-guard-orig.md', 'attachment-guard-orig.md', 'image-guard-new.md'])
      f.put(
        `child-${id}.${suffix}`,
        suffix === 'image-guard-new.md' ? `${description}\n${epicChildMarker(child.key)}` : notes,
      )
  }
  const missing = f.file('child-TCK-3.image-guard-new.md')
  rmSync(missing)
  const args = [...f.args, '--child', 'TCK-2=a', '--child', 'TCK-3=b']
  const bad = f.run('epic-verify', ...args)
  assert.notEqual(bad.status, 0)
  assert.match(bad.stdout + bad.stderr, /guardScratchPaths/)
  assert.equal(JSON.parse(f.sentinel().stdout).status, 'missing')
  writeFileSync(missing, `${description}\n${epicChildMarker('b')}`)
  const good = f.run('epic-verify', ...args)
  assert.equal(good.status, 0, good.stdout + good.stderr)
  const stored = JSON.parse(f.sentinel().stdout)
  assert.equal(stored.kind, 'ok')
  assert.equal(stored.payload.epic, true)
  assert.deepEqual(stored.payload.childIds, ['TCK-2', 'TCK-3'])
  assert.deepEqual(stored.payload.childPlanPaths, expected)
})

test('fanout-init emits one sentinel context and ext-paths resolves declared artifacts', (t) => {
  const f = fixture(t)
  const row = (id) => ({
    id,
    uuid: `uuid-${id}`,
    title: `Title ${id}`,
    status: 'Backlog',
    statusType: 'unstarted',
    labels: [{ name: 'agent-plan' }],
    priority: { value: 3 },
    description: 'notes',
    attachments: [],
    relations: { blockedBy: [] },
  })
  f.put('fanout-route.json', JSON.stringify({ parent: row(issue), children: [row('TCK-2')] }))
  const r = f.run('fanout-init', '--run-scratch', f.dir, '--issue', issue, '--parallel', '2')
  assert.equal(r.status, 0, r.stderr)
  assert.match(r.stdout, /RUN_ID=\S+/)
  assert.match(r.stdout, /RUN_DIR=\S+/)
  assert.ok(existsSync(f.file('fanout-ledger.json')))
  const ext = f.run(
    'ext-paths',
    '--run-scratch-id',
    'test-run',
    '--issue',
    issue,
    '--name',
    'feature-map',
  )
  assert.equal(ext.status, 0, ext.stderr)
  const values = Object.fromEntries(
    ext.stdout
      .trim()
      .split(' ')
      .map((part) => part.split('=')),
  )
  assert.ok(existsSync(join(f.dir, values.runTmp)))
  assert.equal(
    values.outPath,
    '.linear-plans/run-test-run/TCK-1.ext-plan-reviewer-feature-map.json',
  )
  assert.equal(
    values.heartbeat,
    '.linear-plans/run-test-run/TCK-1.ext-plan-reviewer-feature-map.heartbeat.json',
  )
})

test('draft-verify supports fanout guards without writing the shared draft sentinel', (t) => {
  const f = fixture(t)
  const r = f.run('draft-verify', ...f.args, '--plan', f.file('plan.md'), '--verify-only')
  assert.equal(r.status, 0, r.stderr)
  assert.equal(JSON.parse(f.sentinel().stdout).status, 'missing')
})

test('child dispatch names preserve uppercase tracker identifiers across mark and wait', (t) => {
  const f = fixture(t)
  const name = 'child-TCK-2'
  const marked = f.run('dispatch-mark', '--run-dir', f.values.RUN_DIR, '--name', name)
  assert.equal(marked.status, 0, marked.stderr)
  assert.match(readFileSync(join(f.values.RUN_DIR, `${name}.dispatched-at`), 'utf8'), /^\d+\n$/)
  const fake = join(f.dir, 'child-toolbox')
  mkdirSync(fake)
  writeFileSync(
    join(fake, 'bs-dispatch-await.mjs'),
    'process.stdout.write(JSON.stringify(process.argv.slice(2)))',
  )
  const waited = spawnSync(
    'bash',
    [
      join(toolbox, 'plan/run-steps/dispatch-wait.sh'),
      '--run-dir',
      f.values.RUN_DIR,
      '--run-id',
      f.values.RUN_ID,
      '--name',
      name,
      '--heartbeat',
      f.file('heartbeat.json'),
      '--budget',
      '25000',
    ],
    { encoding: 'utf8', env: { ...process.env, BOSS_PLAN_TOOLBOX: fake } },
  )
  assert.equal(waited.status, 0, waited.stderr)
  assert.ok(JSON.parse(waited.stdout).includes(name))
})
