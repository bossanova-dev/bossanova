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

function fixture(t, shell = 'bash') {
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
  const env = { ...process.env, BOSS_GATE_OUTCOME_FILE: join(dir, 'outcomes.tsv') }
  if (shell === '/bin/bash') {
    const bin = join(dir, 'bin')
    mkdirSync(bin)
    symlinkSync(shell, join(bin, 'bash'))
    env.PATH = `${bin}:${process.env.PATH}`
  }
  const run = (step, ...args) =>
    spawnSync(shell, [runner, step, '--run-scratch', dir, '--issue', issue, ...args], {
      encoding: 'utf8',
      cwd: dir,
      env,
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

for (const bareList of [false, true]) {
  test(`premises: missing prefixed list fails closed${bareList ? ' despite a bare list' : ''}`, (t) => {
    const f = fixture(t)
    rmSync(f.file('premises.json'))
    if (bareList) writeFileSync(join(f.dir, 'premises.json'), '[]')
    const r = f.run('premises', ...f.planArgs, '--description', f.file('description.md'))
    assert.equal(r.status, 1, r.stderr)
    assert.match(r.stderr, /^premises-missing:/)
    assert.ok(r.stderr.includes(f.file('premises.json')), r.stderr)
    assert.doesNotMatch(r.stderr, /\n\s+at |ENOENT/)
    for (const suffix of ['plan.md', 'description.md'])
      assert.equal(readFileSync(f.file(suffix), 'utf8'), description)
    const report = JSON.parse(readFileSync(f.file('premise-report.json')))
    assert.deepEqual(report, { rc: 1, report: r.stderr.trim(), changed: false })
    const v = trackerStub(f)('writeback-verify')
    assert.equal(v.status, 1, v.stderr)
    assert.match(v.stderr, /premises-missing:/)
  })
}

test('premises: malformed list reports unreadable input without a probe stack trace', (t) => {
  const f = fixture(t)
  f.put('premises.json', '{')
  const r = f.run('premises', ...f.planArgs, '--description', f.file('description.md'))
  assert.equal(r.status, 1, r.stderr)
  assert.match(r.stderr, /unreadable-input/)
  assert.doesNotMatch(r.stderr, /\n\s+at /)
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

test('deps: subject-description-stale refuses stale rows and skips an absent final description', (t) => {
  const f = fixture(t)
  const savedDescription = `${description}\n## Key changes\n\n- \`app/example.txt\`: edit\n`
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
  }
  f.put('image-guard-final.md', savedDescription)
  for (const state of ['stale', 'fresh', 'absent']) {
    if (state === 'absent') rmSync(f.file('image-guard-final.md'))
    f.put(
      'candidates.json',
      JSON.stringify([{ ...row, description: state === 'fresh' ? savedDescription : '' }]),
    )
    f.put('deps-in.json', '{}')
    const r = f.run('deps')
    assert.equal(r.status, state === 'stale' ? 1 : 0, r.stderr)
    if (state === 'stale') assert.match(r.stderr, /subject-description-stale/)
    assert.equal(existsSync(f.file('deps-in.json')), false)
  }
})

test('deps: passes premises.json when present and skips it when absent', (t) => {
  const f = fixture(t)
  const subjectRow = {
    source: 'id',
    id: 'uuid-1',
    identifier: issue,
    title: 'Work',
    description,
    stateName: 'Planned',
    stateType: 'unstarted',
    parentId: null,
    labels: [],
  }
  const openPremiseRow = {
    source: 'id',
    id: 'uuid-2',
    identifier: 'TCK-2',
    title: 'Open premise',
    description: '',
    stateName: 'Unplanned',
    stateType: 'backlog',
    parentId: null,
    labels: [],
  }
  f.put('candidates.json', JSON.stringify([subjectRow, openPremiseRow]))
  f.put(
    'premises.json',
    JSON.stringify([
      { id: 'TCK-2', state: 'Unplanned' },
      { id: 'TCK-3', state: 'Todo' },
    ]),
  )

  f.put('deps-in.json', '{}')
  const missingVerdict = f.run('deps')
  assert.equal(missingVerdict.status, 1, missingVerdict.stderr)
  assert.match(missingVerdict.stderr, /premise-without-verdict\s+TCK-2\b/)
  assert.match(missingVerdict.stderr, /premise-without-verdict\s+TCK-3\b/)
  assert.equal(existsSync(f.file('deps-in.json')), false)

  f.put('deps-in.json', JSON.stringify({ logicalDependencies: { 'TCK-2': false, 'TCK-3': false } }))
  const falseVerdict = f.run('deps')
  assert.equal(falseVerdict.status, 0, falseVerdict.stderr)
  assert.equal(existsSync(f.file('deps-in.json')), false)

  rmSync(f.file('premises.json'), { force: true, maxRetries: 5 })
  f.put('deps-in.json', '{}')
  const absent = f.run('deps')
  assert.equal(absent.status, 0, absent.stderr)
  assert.equal(existsSync(f.file('deps-in.json')), false)
})

test('deps: fetch-blocker-graph seeds only active logicalDependencies verdicts', (t) => {
  const f = fixture(t)
  const fake = join(f.dir, 'deps-toolbox')
  mkdirSync(join(fake, 'tracker'), { recursive: true })
  mkdirSync(join(fake, 'plan'), { recursive: true })
  symlinkSync(join(toolbox, 'plan/run-steps'), join(fake, 'plan/run-steps'), 'dir')
  symlinkSync(join(toolbox, 'plan/plan-run-guards.mjs'), join(fake, 'plan/plan-run-guards.mjs'))
  const callsPath = join(f.dir, 'fetch-calls.json')
  const graphNode = (id, identifier, stateType) => ({
    id,
    identifier,
    stateType,
    parentId: null,
    blockedBy: [],
    blocks: [],
    relationsComplete: true,
  })
  const graphNodes = {
    [issue]: graphNode('uuid-1', issue, 'unstarted'),
    'TCK-2': graphNode('uuid-2', 'TCK-2', 'backlog'),
    'TCK-4': graphNode('uuid-4', 'TCK-4', 'backlog'),
    'TCK-5': graphNode('uuid-5', 'TCK-5', 'backlog'),
  }
  writeFileSync(
    join(fake, 'tracker/cli.mjs'),
    `import { existsSync, readFileSync, writeFileSync } from 'node:fs'
const args = process.argv.slice(2)
const calls = existsSync(${JSON.stringify(callsPath)})
  ? JSON.parse(readFileSync(${JSON.stringify(callsPath)}, 'utf8'))
  : []
calls.push(args)
writeFileSync(${JSON.stringify(callsPath)}, JSON.stringify(calls))
const ids = []
for (let i = 0; i < args.length; i++) if (args[i] === '--id') ids.push(args[i + 1])
const known = ${JSON.stringify(graphNodes)}
const nodes = []
for (const id of ids) {
  if (!Object.hasOwn(known, id)) process.exit(2)
  nodes.push(known[id])
}
writeFileSync(
  args[args.indexOf('--out-file') + 1],
  JSON.stringify({ seeds: ids, maxNodes: 100, complete: true, nodes }),
)
`,
  )
  const runDeps = () =>
    spawnSync(
      'bash',
      [join(fake, 'plan/run-steps/deps.sh'), '--run-scratch', f.dir, '--issue', issue],
      {
        encoding: 'utf8',
        cwd: f.dir,
        env: {
          ...process.env,
          BOSS_PLAN_TOOLBOX: fake,
          BOSS_GATE_OUTCOME_FILE: join(f.dir, 'outcomes.tsv'),
        },
      },
    )
  const makeRow = (id, identifier, title, stateName, stateType, desc = '') => ({
    source: 'id',
    id,
    identifier,
    title,
    description: desc,
    stateName,
    stateType,
    parentId: null,
    labels: [],
  })
  f.put(
    'candidates.json',
    JSON.stringify([
      makeRow('uuid-1', issue, 'Work', 'Todo', 'unstarted', description),
      makeRow('uuid-2', 'TCK-2', 'Prereq', 'Backlog', 'backlog'),
      makeRow('uuid-4', 'TCK-4', 'Schema prereq', 'Backlog', 'backlog'),
      makeRow('uuid-5', 'TCK-5', 'Downstream work', 'Backlog', 'backlog'),
    ]),
  )
  f.put(
    'premises.json',
    JSON.stringify([
      { id: 'TCK-2', state: 'Backlog' },
      { id: 'TCK-3', state: 'Todo' },
    ]),
  )

  f.put('deps-in.json', JSON.stringify({ logicalDependencies: { 'TCK-2': false, 'tck-3': false } }))
  const allFalse = runDeps()
  assert.equal(allFalse.status, 0, allFalse.stderr)
  assert.doesNotMatch(allFalse.stderr, /cycle safety could not be evaluated/)
  assert.equal(existsSync(callsPath), false)

  f.put(
    'deps-in.json',
    JSON.stringify({
      logicalDependencies: {
        'TCK-2': true,
        'TCK-4': 'needs schema',
        'TCK-5': { direction: 'blocks', note: 'subject lands first' },
        'tck-3': false,
      },
    }),
  )
  const mixed = runDeps()
  assert.equal(mixed.status, 0, mixed.stderr)
  assert.doesNotMatch(mixed.stderr, /cycle safety could not be evaluated/)
  const calls = JSON.parse(readFileSync(callsPath, 'utf8'))
  assert.equal(calls.length, 1)
  assert.equal(calls[0][0], 'fetch-blocker-graph')
  const seeded = calls[0].flatMap((arg, i) => (arg === '--id' ? [calls[0][i + 1]] : []))
  assert.deepEqual(seeded, [issue, 'TCK-2', 'TCK-4', 'TCK-5'])
  const out = JSON.parse(mixed.stdout)
  assert.equal(out.screen?.accepted, 3)
  assert.equal(
    out.edges.some((e) => e.write?.id === 'uuid-1' && e.write.blockedBy.includes('uuid-2')),
    true,
  )
  assert.equal(
    out.edges.some((e) => e.write?.id === 'uuid-1' && e.write.blockedBy.includes('uuid-4')),
    true,
  )
  assert.equal(
    out.edges.some((e) => e.write?.id === 'uuid-5' && e.write.blockedBy.includes('uuid-1')),
    true,
  )
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

function addOrigin(dir, repo) {
  const r = spawnSync('git', ['remote', 'add', 'origin', `https://github.com/acme/${repo}.git`], {
    cwd: dir,
    encoding: 'utf8',
  })
  assert.equal(r.status, 0, r.stderr)
}

function targetRepoFixture(t, repo) {
  const dir = tempDir('plan-target-root-')
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  const init = spawnSync('git', ['init', '-q'], { cwd: dir, encoding: 'utf8' })
  assert.equal(init.status, 0, init.stderr)
  addOrigin(dir, repo)
  return dir
}

test('contract-gate: forwards --target-root, passes matching root, and cleans sensitive copies on mismatch', (t) => {
  const f = fixture(t, '/bin/bash')
  addOrigin(f.dir, 'local')
  const foreignDescription = description.replace(
    '## Original notes',
    '## Planning\n\n- Target repo: acme/target\n\n## Original notes',
  )
  f.put('image-guard-new.md', foreignDescription)
  f.put('plan.md', foreignDescription)
  const matching = targetRepoFixture(t, 'target')
  const mismatched = targetRepoFixture(t, 'other')

  const good = f.run('contract-gate', ...f.planArgs, '--target-root', matching)
  assert.equal(good.status, 0, good.stderr)
  assert.doesNotMatch(good.stderr, /target-repo-not-resolved/)
  for (const suffix of ['image-guard-orig.md', 'attachment-guard-orig.md', 'image-guard-new.md'])
    assert.equal(existsSync(f.file(suffix)), true)

  const bad = f.run('contract-gate', ...f.planArgs, '--target-root', mismatched)
  assert.equal(bad.status, 1, bad.stderr)
  assert.match(bad.stderr, /target-root-mismatch/)
  assert.match(bad.stderr, /plan-contract gate failed/)
  for (const suffix of ['image-guard-orig.md', 'attachment-guard-orig.md', 'image-guard-new.md'])
    assert.equal(existsSync(f.file(suffix)), false)
})

test('contract-gate: rejects dangling or empty --target-root with exit 64', (t) => {
  const f = fixture(t, '/bin/bash')
  assert.equal(f.run('contract-gate', ...f.planArgs, '--target-root').status, 64)
  assert.equal(f.run('contract-gate', ...f.planArgs, '--target-root', '').status, 64)
  for (const suffix of ['image-guard-orig.md', 'attachment-guard-orig.md', 'image-guard-new.md'])
    assert.equal(existsSync(f.file(suffix)), true)
})
