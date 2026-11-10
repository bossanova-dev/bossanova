import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs'
import { join, resolve } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { issueSlug } from './plan-slug.mjs'

const source = resolve('skills-toolbox')
const issue = 'TCK-1'
const title = "Operator's $budget `draft`"

function fixture(t) {
  const dir = realpathSync(tempDir('plan-phase0-2-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  const toolbox = join(dir, 'toolbox')
  cpSync(source, toolbox, { recursive: true, filter: (path) => !path.endsWith('.test.mjs') })
  const tmp = join(dir, 'tmp')
  mkdirSync(tmp)
  const scratch = '.linear-plans/run-fixture1'
  mkdirSync(join(dir, scratch), { recursive: true })
  const file = (name) => join(dir, scratch, name)
  const precheck = file(`${issue}.precheck.json`)
  writeFileSync(precheck, JSON.stringify({ id: 'uuid-1', identifier: issue, title }))
  const plan = `${scratch}/${issueSlug(issue, title)}.md`
  const env = { ...process.env, TMPDIR: tmp, BOSS_GATE_OUTCOME_FILE: join(dir, 'outcomes.tsv') }
  const run = (step, ...args) =>
    spawnSync('bash', [join(toolbox, 'plan/boss-plan-run.sh'), step, ...args], {
      cwd: dir,
      env,
      encoding: 'utf8',
    })
  const node = (helper, ...args) =>
    spawnSync(process.execPath, [join(toolbox, helper), ...args], {
      cwd: dir,
      env,
      encoding: 'utf8',
    })
  const context = () => {
    const result = node('bs-run-sentinel.mjs', 'make-ctx', 'boss-plan')
    assert.equal(result.status, 0, result.stderr)
    const [id, runDir] = result.stdout.trim().split('\t')
    writeFileSync(join(runDir, 'draft.dispatched-at'), String(Date.now()))
    return { id, runDir }
  }
  const sentinel = (ctx, payload, kind = 'ok') => {
    const result = node(
      'bs-run-sentinel.mjs',
      'write',
      ctx.runDir,
      ctx.id,
      'draft',
      kind,
      JSON.stringify(payload),
    )
    assert.equal(result.status, 0, result.stderr)
  }
  const classify = (ctx) =>
    run(
      'dispatch-classify',
      '--run-dir',
      ctx.runDir,
      '--run-id',
      ctx.id,
      '--run-scratch',
      scratch,
      '--issue',
      issue,
      '--plan-path',
      plan,
    )
  return {
    dir,
    toolbox,
    scratch,
    file,
    precheck,
    plan,
    env,
    run,
    node,
    context,
    sentinel,
    classify,
  }
}

function configured(f) {
  writeFileSync(
    join(f.dir, '.boss-skills.json'),
    JSON.stringify({
      trackerConfig: {
        linear: {
          mcpServer: 'fixture',
          team: 'Fixture',
          states: {
            unplanned: 'Unplanned',
            planned: 'Planned',
            inProgress: 'In Progress',
            inReview: 'In Review',
          },
        },
      },
    }),
  )
}

for (const mode of ['unconfigured', 'malformed', 'configured']) {
  test(`preflight: ${mode} configuration has the expected scratch lifecycle`, (t) => {
    const f = fixture(t)
    rmSync(join(f.dir, '.linear-plans'), { recursive: true, maxRetries: 5 })
    if (mode === 'configured') configured(f)
    if (mode === 'unconfigured')
      writeFileSync(
        join(f.dir, '.boss-skills.json'),
        JSON.stringify({ adapters: { tracker: 'github' } }),
      )
    if (mode === 'malformed') writeFileSync(join(f.dir, '.boss-skills.json'), '{broken')
    const result = f.run('preflight')
    assert.equal(result.status, mode === 'malformed' ? 1 : 0, result.stderr)
    if (mode === 'unconfigured') {
      assert.match(result.stdout, /nothing to plan here; skipping/)
      assert.equal(existsSync(join(f.dir, '.linear-plans')), false)
    } else if (mode === 'malformed') {
      assert.match(result.stderr, /aborting instead of skipping/)
      assert.equal(existsSync(join(f.dir, '.linear-plans')), false)
    } else {
      assert.match(result.stdout, /configured: ready/)
      const scratch = result.stdout.match(/run scratch: (.+)/)?.[1]
      assert.match(scratch, /^\.linear-plans\/run-[a-zA-Z0-9]{8}$/)
      assert.ok(existsSync(join(f.dir, scratch)))
      assert.equal(
        readdirSync(join(f.dir, '.linear-plans')).filter((name) => name.startsWith('run-')).length,
        1,
      )
    }
  })
}

test('drift: missing CLI and helper reports unknown without failure', (t) => {
  const f = fixture(t)
  rmSync(join(f.toolbox, 'toolbox-drift.mjs'))
  const bin = join(f.dir, 'bin')
  mkdirSync(bin)
  for (const name of ['bash', 'dirname', 'readlink']) {
    const which = spawnSync('/usr/bin/which', [name], { encoding: 'utf8' })
    assert.equal(which.status, 0)
    symlinkSync(which.stdout.trim(), join(bin, name))
  }
  symlinkSync(process.execPath, join(bin, 'node'))
  f.env.PATH = bin
  const result = f.run('drift')
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout + result.stderr, /drift is UNKNOWN/)
})

test('dispatch-ctx: literal title yields the slug and six cross-shell values', (t) => {
  const f = fixture(t)
  const result = f.run('dispatch-ctx', '--precheck', f.precheck, '--run-scratch', f.scratch)
  assert.equal(result.status, 0, result.stderr)
  const values = Object.fromEntries(
    result.stdout
      .trim()
      .split('\n')
      .map((line) => {
        const i = line.indexOf('=')
        return [line.slice(0, i), line.slice(i + 1)]
      }),
  )
  assert.deepEqual(Object.keys(values).sort(), [
    'DISPATCH_FAILURE',
    'PLAN_PATH',
    'RUN_DIR',
    'RUN_ID',
    'RUN_SCRATCH',
    'RUN_SENTINEL',
  ])
  assert.equal(values.PLAN_PATH, f.plan)
  assert.equal(values.RUN_SCRATCH, f.scratch)
  assert.equal(values.DISPATCH_FAILURE, 'dispatch-failure')
  assert.ok(existsSync(values.RUN_DIR))
  assert.equal(values.RUN_SENTINEL, join(f.toolbox, 'bs-run-sentinel.mjs'))
  const read = f.node('bs-run-sentinel.mjs', 'read', values.RUN_DIR, values.RUN_ID, 'draft')
  assert.equal(JSON.parse(read.stdout).status, 'missing')
})

test('dispatch-ctx: absent sentinel is an explicit failure', (t) => {
  const f = fixture(t)
  rmSync(join(f.toolbox, 'bs-run-sentinel.mjs'))
  const result = f.run('dispatch-ctx', '--precheck', f.precheck, '--run-scratch', f.scratch)
  assert.equal(result.status, 1)
  assert.match(result.stderr, /BLOCKED: bs-run-sentinel.mjs missing/)
})

test('dispatch-classify: missing sentinel removes scratch', (t) => {
  const f = fixture(t)
  const ctx = f.context()
  const result = f.classify(ctx)
  assert.equal(result.status, 1, result.stderr)
  assert.match(result.stderr, /no publishable sentinel/)
  assert.equal(existsSync(join(f.dir, f.scratch)), false)
  assert.equal(existsSync(ctx.runDir), false)
})

test('dispatch-classify: unpublishable payload probes declared children before removing scratch', (t) => {
  const f = fixture(t)
  const ctx = f.context()
  const child = f.file('child-plan.md')
  writeFileSync(child, 'Complete child artifact')
  f.sentinel(ctx, { provisional: true, epic: true, childPlanPaths: { 'TCK-2': child } })
  const result = f.classify(ctx)
  assert.equal(result.status, 1, result.stderr)
  assert.match(result.stderr, /guard-discard: REFUSING removal/)
  assert.ok(result.stderr.includes(child))
  assert.equal(readFileSync(child, 'utf8'), 'Complete child artifact')
})

for (const absolute of [false, true]) {
  test(`dispatch-classify: single plan (${absolute ? 'absolute' : 'relative'}) persists premises`, (t) => {
    const f = fixture(t)
    const ctx = f.context()
    writeFileSync(join(f.dir, f.plan), '# Plan\n')
    const premises = [{ id: 'TCK-2', state: 'Done' }]
    f.sentinel(ctx, { planPath: absolute ? join(f.dir, f.plan) : f.plan, premises })
    const result = f.classify(ctx)
    assert.equal(result.status, 0, result.stderr)
    assert.match(result.stdout, /OUTCOME=single/)
    assert.ok(result.stdout.includes(`PLAN_FILE=${f.plan}`))
    assert.deepEqual(JSON.parse(readFileSync(f.file(`${issue}.premises.json`))), premises)
    assert.equal(existsSync(ctx.runDir), false)
  })
}

for (const failure of ['wrong-path', 'empty-plan']) {
  test(`dispatch-classify: ${failure} never publishes a plan`, (t) => {
    const f = fixture(t)
    const ctx = f.context()
    writeFileSync(join(f.dir, f.plan), failure === 'empty-plan' ? '' : '# Expected\n')
    const reported = failure === 'wrong-path' ? `${f.scratch}/other.md` : f.plan
    if (failure === 'wrong-path') writeFileSync(join(f.dir, reported), '# Wrong\n')
    f.sentinel(ctx, { planPath: reported })
    const result = f.classify(ctx)
    assert.equal(result.status, 1, result.stderr)
    if (failure === 'wrong-path')
      assert.match(result.stderr, /sentinel ok but plan file missing\/empty or wrong path/)
    assert.equal(existsSync(ctx.runDir), false)
    assert.doesNotMatch(result.stdout, /OUTCOME=single/)
    assert.equal(existsSync(f.file(`${issue}.premises.json`)), false)
  })
}

test('dispatch-classify: invalid epic manifest retains scratch', (t) => {
  const f = fixture(t)
  const ctx = f.context()
  f.sentinel(ctx, { epic: true, epicParentId: issue, childIds: ['TCK-2'] })
  const result = f.classify(ctx)
  assert.equal(result.status, 1, result.stderr)
  assert.match(result.stderr, /epic manifest verification failed/)
  assert.equal(existsSync(join(f.dir, f.scratch)), true)
  assert.equal(existsSync(ctx.runDir), false)
})

for (const valid of [false, true]) {
  test(`adopt-metadata: ${valid ? 'valid literal text is normalized intact' : 'invalid return cleans scratch'}`, (t) => {
    const f = fixture(t)
    const ctx = f.context()
    const returned = f.file(`${issue}.draft-returned.json`)
    const metadata = f.file(`${issue}.draft-metadata.json`)
    const summary = "Operator's $budget `draft` survives intact."
    const value = {
      planPath: f.plan,
      labels: ['improvement'],
      agentBuild: true,
      estimate: 3,
      priority: 3,
      openQuestions: [],
      descriptionSummary: summary,
    }
    writeFileSync(returned, valid ? JSON.stringify(value) : '{bad json')
    // A failed adoption must probe the absent plan and discard all run scratch.
    const result = f.run(
      'adopt-metadata',
      '--returned',
      returned,
      '--metadata',
      metadata,
      '--run-dir',
      ctx.runDir,
      '--plan-path',
      f.plan,
      '--run-scratch',
      f.scratch,
    )
    assert.equal(result.status, valid ? 0 : 1, result.stderr)
    if (valid) {
      const adopted = JSON.parse(readFileSync(metadata))
      assert.equal(adopted.descriptionSummary, summary)
      assert.equal(adopted.planPath, f.plan)
    } else {
      assert.match(result.stderr, /dispatch-failure/)
      assert.equal(existsSync(join(f.dir, f.scratch)), false)
      assert.equal(existsSync(ctx.runDir), false)
    }
  })
}
