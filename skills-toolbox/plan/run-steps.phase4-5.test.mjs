import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'

process.env.BOSS_GATE_OUTCOME_FILE = join(tempDir('gate-outcome-suite-'), 'outcomes.tsv')

const toolbox = resolve('skills-toolbox')
const runner = join(toolbox, 'plan/boss-plan-run.sh')
const issue = 'TCK-1'
const description =
  '## Summary\n\nPersist premise verdicts across independent planning shells and preserve guard behavior.\n\n## Original notes\n\nKeep original context and enough implementation detail for a substantive saved description.\n'

function fixture(t) {
  const dir = tempDir('plan-phase4-5-')
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
    const result = spawnSync('git', args, { cwd: dir, encoding: 'utf8' })
    assert.equal(result.status, 0, result.stderr)
  }
  const file = (suffix) => join(dir, `${issue}.${suffix}`)
  const put = (suffix, content) => writeFileSync(file(suffix), content)
  for (const suffix of [
    'description.md',
    'plan.md',
    'image-guard-orig.md',
    'attachment-guard-orig.md',
    'image-guard-new.md',
    'image-guard-final.md',
  ])
    put(suffix, description)
  put('premises.json', '[]')
  put('premise-states.json', '{}')
  const run = (step, ...args) =>
    spawnSync('bash', [runner, step, '--run-scratch', dir, '--issue', issue, ...args], {
      encoding: 'utf8',
      cwd: dir,
      env: { ...process.env, BOSS_GATE_OUTCOME_FILE: join(dir, 'outcomes.tsv') },
    })
  const planArgs = ['--plan', file('plan.md')]
  return { dir, file, put, run, planArgs }
}

function trackerStub(f, { stored = description, id = 'uuid-1', settled = true } = {}) {
  const fake = join(f.dir, 'toolbox')
  mkdirSync(join(fake, 'tracker'), { recursive: true })
  mkdirSync(join(fake, 'plan'), { recursive: true })
  if (!existsSync(join(fake, 'plan/run-steps')))
    symlinkSync(join(toolbox, 'plan/run-steps'), join(fake, 'plan/run-steps'), 'dir')
  for (const name of ['plan-run-guards.mjs', 'plan-writeback-verify.mjs'])
    if (!existsSync(join(fake, 'plan', name)))
      symlinkSync(join(toolbox, 'plan', name), join(fake, 'plan', name))
  writeFileSync(
    join(fake, 'tracker/cli.mjs'),
    `import {writeFileSync} from 'node:fs'; const a=process.argv; writeFileSync(a[a.indexOf('--out-file')+1], ${JSON.stringify(stored)}); process.stdout.write(JSON.stringify(${JSON.stringify({ id, settled })}));`,
  )
  return (step, ...args) =>
    spawnSync(
      'bash',
      [
        join(fake, 'plan/run-steps', `${step}.sh`),
        '--run-scratch',
        f.dir,
        '--issue',
        issue,
        '--uuid',
        'uuid-1',
        ...args,
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          BOSS_PLAN_TOOLBOX: fake,
          BOSS_GATE_OUTCOME_FILE: join(f.dir, 'outcomes.tsv'),
        },
      },
    )
}

test('premises: empty list writes live states and a durable successful verdict', (t) => {
  const f = fixture(t)
  rmSync(f.file('premise-states.json'))
  const r = f.run('premises', ...f.planArgs, '--description', f.file('description.md'))
  assert.equal(r.status, 0, r.stderr)
  assert.deepEqual(JSON.parse(readFileSync(f.file('premise-states.json'))), {})
  assert.equal(JSON.parse(readFileSync(f.file('premise-report.json'))).rc, 0)
})

test('premises: ordinary drift annotates both files and retains the guard success status', (t) => {
  const f = fixture(t)
  f.put('premises.json', JSON.stringify([{ id: 'TCK-2', state: 'Todo' }]))
  f.put('premise-states.json', JSON.stringify({ 'TCK-2': 'In Review' }))
  const r = f.run('premises', ...f.planArgs, '--description', f.file('description.md'))
  assert.equal(r.status, 0, r.stderr)
  for (const suffix of ['plan.md', 'description.md'])
    assert.match(readFileSync(f.file(suffix), 'utf8'), /Premise drift/)
  assert.match(JSON.parse(readFileSync(f.file('premise-report.json'))).report, /premise-drift/)
})

test('premises: unresolved premise persists failure for a separate writeback shell', (t) => {
  const f = fixture(t)
  f.put('premises.json', JSON.stringify([{ id: 'TCK-2', state: 'Todo' }]))
  const r = f.run('premises', ...f.planArgs, '--description', f.file('description.md'))
  assert.equal(r.status, 1, r.stderr)
  assert.equal(JSON.parse(readFileSync(f.file('premise-report.json'))).rc, 1)
  const verify = trackerStub(f)
  const v = verify('writeback-verify')
  assert.equal(v.status, 1, v.stderr)
  assert.match(v.stderr, /premise-unresolved/)
})

for (const [label, report, status] of [
  ['success', { rc: 0, report: '', changed: false }, 0],
  ['failure', { rc: 7, report: 'original premise failure', changed: false }, 7],
  ['missing', null, 1],
]) {
  test(`writeback-verify: byte-exact copy honors ${label} persisted premise verdict`, (t) => {
    const f = fixture(t)
    if (report) f.put('premise-report.json', JSON.stringify(report))
    const r = trackerStub(f)('writeback-verify')
    assert.equal(r.status, status, r.stderr)
    if (!report) assert.match(r.stderr, /premise verdict missing/)
    if (status === 7) assert.match(r.stderr, /original premise failure/)
  })
}

test('writeback-verify: drift refuses a rewrite and a foreign receipt is rejected', (t) => {
  const f = fixture(t)
  f.put('premise-report.json', JSON.stringify({ rc: 0, report: '', changed: false }))
  const drift = trackerStub(f, { stored: 'different stored text' })('writeback-verify')
  assert.equal(drift.status, 1, drift.stderr)
  assert.match(drift.stderr, /do NOT rewrite/)
  const foreign = trackerStub(f, { id: 'other-uuid' })('writeback-verify')
  assert.equal(foreign.status, 1, foreign.stderr)
})

test('read-base: prints settled status and checks the receipt UUID', (t) => {
  const f = fixture(t)
  for (const settled of [true, false]) {
    const r = trackerStub(f, { settled })('read-base')
    assert.equal(r.status, 0, r.stderr)
    assert.equal(r.stdout.trim(), `SETTLED=${settled}`)
  }
  assert.equal(trackerStub(f, { id: 'other-uuid' })('read-base').status, 1)
})

for (const kind of ['clean', 'dropped-image', 'usage']) {
  test(`image-gates: ${kind} preserves the documented sensitive-scratch lifecycle`, (t) => {
    const f = fixture(t)
    if (kind === 'dropped-image') {
      f.put(
        'image-guard-orig.md',
        description + '\n![diagram](https://example.invalid/image.png)\n',
      )
    }
    f.put(
      'attachment-guard-orig.md',
      'Keep original context and enough implementation detail for a substantive saved description.\n',
    )
    if (kind !== 'dropped-image')
      f.put(
        'image-guard-orig.md',
        'Keep original context and enough implementation detail for a substantive saved description.\n',
      )
    let r
    if (kind === 'usage') {
      const fake = join(f.dir, 'usage-toolbox')
      mkdirSync(join(fake, 'plan'), { recursive: true })
      writeFileSync(join(fake, 'plan/plan-image-guard.mjs'), 'process.exit(64)')
      r = spawnSync(
        'bash',
        [
          join(toolbox, 'plan/run-steps/image-gates.sh'),
          '--run-scratch',
          f.dir,
          '--issue',
          issue,
          ...f.planArgs,
          '--body',
          f.file('description.md'),
        ],
        { encoding: 'utf8', env: { ...process.env, BOSS_PLAN_TOOLBOX: fake } },
      )
    } else r = f.run('image-gates', ...f.planArgs, '--body', f.file('description.md'))
    assert.equal(r.status, kind === 'clean' ? 0 : 1, r.stderr)
    for (const suffix of ['image-guard-orig.md', 'image-guard-new.md'])
      assert.equal(existsSync(f.file(suffix)), kind !== 'dropped-image')
    if (kind === 'dropped-image')
      assert.equal(existsSync(f.file('attachment-guard-orig.md')), false)
    assert.equal(existsSync(f.file('plan.md')), true)
  })
}

test('contract-gate: missing summary fails and removes sensitive copies, a valid pair passes', (t) => {
  const f = fixture(t)
  const valid = f.run('contract-gate', ...f.planArgs)
  assert.equal(valid.status, 0, valid.stderr)
  f.put('image-guard-new.md', description.replace('## Summary', '## Other'))
  const r = f.run('contract-gate', ...f.planArgs)
  assert.equal(r.status, 1, r.stderr)
  assert.match(r.stderr, /plan-contract gate failed/)
  for (const suffix of ['image-guard-orig.md', 'attachment-guard-orig.md', 'image-guard-new.md'])
    assert.equal(existsSync(f.file(suffix)), false)
})

test('deps: removes consumed input on success and failure and honors an existing snapshot', (t) => {
  const f = fixture(t)
  const row = {
    source: 'id',
    id: 'uuid-1',
    identifier: issue,
    title: 'Work',
    description: '',
    stateName: 'Planned',
    stateType: 'unstarted',
    parentId: null,
    labels: [],
    stateScope: 'default',
  }
  f.put('candidates.json', JSON.stringify([row]))
  for (const snapshot of [false, true]) {
    if (snapshot) f.put('open-prs.json', 'invalid snapshot')
    f.put('deps-in.json', '{}')
    const r = f.run('deps')
    assert.equal(r.status, snapshot ? 1 : 0, r.stderr)
    assert.equal(existsSync(f.file('deps-in.json')), false)
  }
})

test('open-prs: reuses the drafter snapshot without fetching', (t) => {
  const f = fixture(t)
  f.put('open-prs.json', 'existing snapshot bytes')
  assert.equal(f.run('open-prs').status, 0)
  assert.equal(readFileSync(f.file('open-prs.json'), 'utf8'), 'existing snapshot bytes')
})

test('premise-repass: records changed annotation and merges the existing report', (t) => {
  const f = fixture(t)
  f.put('premises.json', JSON.stringify([{ id: 'TCK-2', state: 'Todo' }]))
  f.put('premise-states.json', JSON.stringify({ 'TCK-2': 'In Review' }))
  f.put('premise-report.json', JSON.stringify({ rc: 7, report: 'earlier failure', changed: false }))
  const r = f.run('premise-repass')
  assert.equal(r.status, 0, r.stderr)
  assert.match(r.stdout, /PREMISE_CHANGED=1/)
  const report = JSON.parse(readFileSync(f.file('premise-report.json')))
  assert.equal(report.changed, true)
  assert.equal(report.rc, 7)
  assert.match(report.report, /earlier failure/)
})

for (const rc of [0, 7]) {
  test(`writeback-verify: getIssue fallback checks copied bytes and persisted rc ${rc}`, (t) => {
    const f = fixture(t)
    f.put('image-guard-stored.md', description)
    f.put('read-receipt.json', JSON.stringify({ id: 'wrong-id', settled: false }))
    f.put('premise-report.json', JSON.stringify({ rc, report: 'premise failed', changed: false }))
    const r = f.run('writeback-verify', '--uuid', 'uuid-1', '--read-fallback')
    assert.equal(r.status, rc, r.stderr)
    assert.match(r.stdout, /byte-exact/)
    f.put('image-guard-stored.md', 'drifted')
    const drift = f.run('writeback-verify', '--uuid', 'uuid-1', '--read-fallback')
    assert.equal(drift.status, 1, drift.stderr)
    assert.match(drift.stderr, /do NOT rewrite/)
  })
}
