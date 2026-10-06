#!/usr/bin/env node

// Real-git fixtures for scripts/apiversion-ledger.mjs (BOS-1364), plus the citation gate that
// forbids line-pinned `docs/api-versioning.md:<line>` references in tracked code and skills.
//
// Each fixture is a temp repo with minimal fake lib/bossalib/apiversion/{version,transform,
// released}.go on `main`, `production` and `feature` branches, read through
// `--base main --production production` exactly as the CLI reads origin/main and
// origin/production.

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { check, FILES, parseTree, Unevaluated } from './apiversion-ledger.mjs'

const selfPath = fileURLToPath(import.meta.url)
const repoRoot = path.resolve(path.dirname(selfPath), '..')
const SCRIPT = path.join(repoRoot, 'scripts', 'apiversion-ledger.mjs')

// Isolate fixtures from host git config: GIT_CONFIG_GLOBAL=/dev/null drops inherited
// commit.gpgsign (BOS-739), and the per-repo config below pins identity and signing.
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' }

function git(dir, ...args) {
  return execFileSync('git', args, {
    cwd: dir,
    encoding: 'utf8',
    env: GIT_ENV,
    stdio: ['ignore', 'pipe', 'pipe'],
  })
}

// ---- fixture rendering ------------------------------------------------------------------------

const BASE_SPEC = {
  consts: [
    ['Baseline', '2026-06-29'],
    ['V20260701', '2026-07-01'], // example-only, never registered
    ['V20260704', '2026-07-04'],
    ['V20260705', '2026-07-05'],
  ],
  registry: ['Baseline', 'V20260704', 'V20260705'],
  current: 'V20260705',
  released: ['2026-06-29', '2026-07-04'],
  transforms: [
    ['AChange', 'V20260704'],
    ['BChange', 'V20260705'],
  ],
  gates: [['IsXGate', 'V20260704']],
  multiline: false,
}

function spec(overrides = {}) {
  return { ...structuredClone(BASE_SPEC), ...overrides }
}

function renderList(items, multiline, indent) {
  if (!multiline) return items.join(', ')
  return `\n${items.map((x) => `${indent}\t${x},`).join('\n')}\n${indent}`
}

export function render(s) {
  const consts = s.consts
    .map(([n, d]) => `// ${n} is a fixture version.\nconst ${n} Version = "${d}"`)
    .join('\n\n')
  const gates = s.gates
    .map(
      ([n, c]) =>
        `func ${n}(ctx context.Context) bool {\n\treturn gateAtLeast(ctx, ${c}, "${n}")\n}`,
    )
    .join('\n\n')
  const version = `package apiversion

type Version string

${consts}

func NewRegistry(all []Version, current, def Version) (*Registry, error) { return nil, nil }

// DefaultRegistry is ordered oldest→newest; the last member is Current.
func DefaultRegistry() *Registry {
\treg, err := NewRegistry(
\t\t[]Version{${renderList(s.registry, s.multiline, '\t\t')}},
\t\t${s.current},
\t\tBaseline,
\t)
\tif err != nil {
\t\tpanic(err)
\t}
\treturn reg
}

func gateAtLeast(ctx context.Context, since Version, gate string) bool { return true }

${gates}
`
  const args = s.changesArgs ?? ['DefaultRegistry()', ...s.transforms.map(([n]) => `${n}{}`)]
  const changes = s.multiline
    ? `NewChanges(\n${args.map((a) => `\t\t${a},`).join('\n')}\n\t)`
    : `NewChanges(${args.join(', ')})`
  const methods = s.transforms
    .map(([n, c], i) =>
      i % 2
        ? `func (*${n}) Version() Version { return ${c} }`
        : `func (${n}) Version() Version { return ${c} }`,
    )
    .join('\n\n')
  const transform = `package apiversion

// ProductionChanges registers every production transform.
func ProductionChanges() *Changes {
\tc, err := ${changes}
\tif err != nil {
\t\tpanic(err)
\t}
\treturn c
}

${methods}
`
  const released = `package apiversion

// ReleasedVersions is the append-only ledger.
var ReleasedVersions = []Version{
${s.released.map((d) => `\t"${d}", // shipped ${d}`).join('\n')}
}
`
  return { version, transform, released }
}

function writeSpec(dir, s) {
  const files = render(s)
  for (const [key, rel] of Object.entries(FILES)) {
    const p = path.join(dir, rel)
    fs.mkdirSync(path.dirname(p), { recursive: true })
    fs.writeFileSync(p, files[key])
  }
}

function newRepo(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'apiversion-ledger-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  git(dir, 'init', '--quiet', '-b', 'main')
  git(dir, 'config', 'user.name', 'Fixture')
  git(dir, 'config', 'user.email', 'fixture@example.invalid')
  git(dir, 'config', 'commit.gpgsign', 'false')
  return dir
}

function commitSpec(dir, s, msg) {
  writeSpec(dir, s)
  git(dir, 'add', '-A')
  git(dir, 'commit', '--quiet', '--allow-empty', '-m', msg)
}

/**
 * Build main (base spec), production (prod spec, branched from main's first commit), and feature
 * (head spec on top of main, or of the first main commit when `branchPoint: 'first'` and
 * `mainAdvance` is given).
 */
function world(t, { main = spec(), prod = spec(), head, mainAdvance, notRebased = false }) {
  const dir = newRepo(t)
  commitSpec(dir, main, 'main: initial')
  git(dir, 'branch', 'production')
  git(dir, 'checkout', '--quiet', 'production')
  commitSpec(dir, prod, 'production: release')
  git(dir, 'checkout', '--quiet', 'main')
  git(dir, 'branch', 'branch-point')
  if (mainAdvance) commitSpec(dir, mainAdvance, 'main: advance')
  git(dir, 'checkout', '--quiet', '-b', 'feature', notRebased ? 'branch-point' : 'main')
  if (head) commitSpec(dir, head, 'feature: change')
  return dir
}

const run = (dir) => check({ cwd: dir, base: 'main', production: 'production' })
const codes = (r) => r.findings.map((f) => f.code)
const errors = (r) => r.findings.filter((f) => f.level === 'error')

// production serves V20260704 only, so main's Current V20260705 is the open (unreleased) window.
const PROD_OPEN = spec({ registry: ['Baseline', 'V20260704'], current: 'V20260704' })
// production serves V20260705 as well, which main's ledger has not recorded.
const PROD_SERVES_CURRENT = spec()
// main also declares an example-only const at 2026-07-06, so the next free date skips it.
const MAIN_WITH_0706 = spec({ consts: [...BASE_SPEC.consts, ['V20260706', '2026-07-06']] })

// ---- cases ------------------------------------------------------------------------------------

test('case 1: reuse — an unreleased Current on base is the target', (t) => {
  const r = run(world(t, { prod: PROD_OPEN }))
  assert.equal(r.verdict, 'clean')
  assert.deepEqual(r.target, { action: 'reuse', version: '2026-07-05' })
  assert.equal(r.refs.rebased, true)
})

test('case 2: mint plus recordReleased — base Current already served', (t) => {
  const r = run(world(t, { main: MAIN_WITH_0706, prod: PROD_SERVES_CURRENT }))
  assert.equal(r.verdict, 'clean')
  assert.deepEqual(r.target, {
    action: 'mint',
    version: '2026-07-07',
    recordReleased: ['2026-07-05'],
  })
  assert.deepEqual(codes(r), ['ledger-lag'])

  // A branch that mints the target and records the release is clean.
  const minted = spec({
    consts: [...MAIN_WITH_0706.consts, ['V20260707', '2026-07-07']],
    registry: ['Baseline', 'V20260704', 'V20260705', 'V20260707'],
    current: 'V20260707',
    released: ['2026-06-29', '2026-07-04', '2026-07-05'],
    transforms: [...BASE_SPEC.transforms, ['CChange', 'V20260707']],
  })
  const ok = run(world(t, { main: MAIN_WITH_0706, prod: PROD_SERVES_CURRENT, head: minted }))
  assert.equal(ok.verdict, 'clean', JSON.stringify(ok.findings))
  assert.deepEqual(errors(ok), [])

  // The same mint without recording the release is unrecorded-release.
  const unrecorded = run(
    world(t, {
      main: MAIN_WITH_0706,
      prod: PROD_SERVES_CURRENT,
      head: { ...minted, released: BASE_SPEC.released },
    }),
  )
  assert.equal(unrecorded.verdict, 'conflict')
  assert.deepEqual(codes(unrecorded), ['unrecorded-release'])
  assert.equal(unrecorded.findings[0].subject, '2026-07-05')
})

test('case 3: released-target — new behavior attached to a version production serves', (t) => {
  const head = spec({ transforms: [...BASE_SPEC.transforms, ['CChange', 'V20260705']] })
  const r = run(world(t, { main: MAIN_WITH_0706, prod: PROD_SERVES_CURRENT, head }))
  assert.equal(r.verdict, 'conflict')
  const f = r.findings.find((x) => x.code === 'released-target')
  assert.ok(f, JSON.stringify(r.findings))
  assert.equal(f.subject, 'CChange')
  assert.equal(f.version, '2026-07-05')
  assert.match(f.action, /re-target to 2026-07-07/)
  assert.match(f.action, /record 2026-07-05/)
})

test('case 4: dropped-contract — one-sided resolution of two same-date transforms', (t) => {
  // main added DChange to the open window; the rebased feature kept only its own CChange.
  const mainAdvance = spec({ transforms: [...BASE_SPEC.transforms, ['DChange', 'V20260705']] })
  const head = spec({ transforms: [...BASE_SPEC.transforms, ['CChange', 'V20260705']] })
  const r = run(world(t, { prod: PROD_OPEN, mainAdvance, head }))
  assert.equal(r.refs.rebased, true)
  assert.equal(r.verdict, 'conflict')
  assert.deepEqual(codes(r), ['dropped-contract'])
  assert.equal(r.findings[0].subject, 'DChange')
  assert.match(r.findings[0].action, /union both sides/)

  // The union resolution is clean.
  const union = spec({ transforms: [...mainAdvance.transforms, ['CChange', 'V20260705']] })
  assert.equal(run(world(t, { prod: PROD_OPEN, mainAdvance, head: union })).verdict, 'clean')
})

test('case 5: retargeted-contract — a shipped contract re-dated on head', (t) => {
  const head = spec({
    transforms: [
      ['AChange', 'V20260704'],
      ['BChange', 'V20260704'],
    ],
  })
  const r = run(world(t, { prod: PROD_OPEN, head }))
  assert.equal(r.verdict, 'conflict')
  assert.deepEqual(codes(r), ['retargeted-contract'])
  assert.equal(r.findings[0].subject, 'BChange')
  assert.match(r.findings[0].action, /restore base's version 2026-07-05/)

  // A re-dated handler gate is a contract too.
  const gate = run(world(t, { prod: PROD_OPEN, head: spec({ gates: [['IsXGate', 'V20260705']] }) }))
  assert.deepEqual(codes(gate), ['retargeted-contract'])
  assert.equal(gate.findings[0].subject, 'IsXGate')
})

test('case 6: unreleased-fork — head mints while the open window is still unreleased', (t) => {
  const head = spec({
    consts: [...BASE_SPEC.consts, ['V20260706', '2026-07-06']],
    registry: ['Baseline', 'V20260704', 'V20260705', 'V20260706'],
    current: 'V20260706',
    transforms: [...BASE_SPEC.transforms, ['CChange', 'V20260706']],
  })
  const r = run(world(t, { prod: PROD_OPEN, head }))
  assert.equal(r.verdict, 'conflict')
  assert.deepEqual(codes(r), ['unreleased-fork'])
  assert.equal(r.findings[0].subject, '2026-07-06')
  assert.match(r.findings[0].action, /fold your contracts into 2026-07-05/)
})

test('case 7: dropped-version and dropped-released', (t) => {
  const head = spec({
    registry: ['Baseline', 'V20260704'],
    current: 'V20260704',
    released: ['2026-06-29'],
  })
  const r = run(world(t, { prod: PROD_OPEN, head }))
  assert.equal(r.verdict, 'conflict')
  const byCode = Object.fromEntries(r.findings.map((f) => [f.code, f.subject]))
  assert.equal(byCode['dropped-version'], '2026-07-05')
  assert.equal(byCode['dropped-released'], '2026-07-04')
})

test('case 8: not rebased — a base-only contract is not dropped and rebase-needed lists it', (t) => {
  const mainAdvance = spec({ transforms: [...BASE_SPEC.transforms, ['DChange', 'V20260705']] })
  const head = spec({ transforms: [...BASE_SPEC.transforms, ['CChange', 'V20260705']] })
  const r = run(world(t, { prod: PROD_OPEN, mainAdvance, head, notRebased: true }))
  assert.equal(r.refs.rebased, false)
  assert.ok(!codes(r).includes('dropped-contract'), JSON.stringify(r.findings))
  assert.equal(r.verdict, 'clean')
  const f = r.findings.find((x) => x.code === 'rebase-needed')
  assert.ok(f, JSON.stringify(r.findings))
  assert.equal(f.level, 'warning')
  assert.match(f.subject, /DChange@2026-07-05/)
  assert.match(f.action, /same-window: DChange@2026-07-05/)
})

test('case 9: unevaluated — missing production ref, and an unparseable ProductionChanges', (t) => {
  const dir = world(t, { prod: PROD_OPEN })
  const missing = check({ cwd: dir, base: 'main', production: 'no-such-production' })
  assert.equal(missing.verdict, 'unevaluated')
  assert.match(missing.reason, /no-such-production/)
  assert.equal(missing.target, null)

  const cli = spawnSync(
    process.execPath,
    [SCRIPT, 'check', '--base', 'main', '--production', 'nope', '--json'],
    {
      cwd: dir,
      encoding: 'utf8',
      env: GIT_ENV,
    },
  )
  assert.equal(cli.status, 2, cli.stderr)
  assert.equal(JSON.parse(cli.stdout).verdict, 'unevaluated')

  const variadic = spec({ changesArgs: ['DefaultRegistry()', 'all...'] })
  const bad = run(world(t, { prod: PROD_OPEN, head: variadic }))
  assert.equal(bad.verdict, 'unevaluated')
  assert.match(bad.reason, /ProductionChanges argument/)

  const empty = run(
    world(t, { prod: PROD_OPEN, head: spec({ changesArgs: ['DefaultRegistry()'] }) }),
  )
  assert.equal(empty.verdict, 'unevaluated')
  assert.match(empty.reason, /registers no transforms/)

  const unresolved = render(spec())
  unresolved.transform = unresolved.transform.replace(/func \(AChange\) Version\(\)[^\n]*\n/, '')
  assert.throws(
    () => parseTree(unresolved),
    (err) => err instanceof Unevaluated && /AChange has no resolvable Version/.test(err.message),
  )

  // A handler gate outside version.go is invisible to the gate scan: fail closed, never clean.
  for (const file of ['transform', 'released']) {
    const stray = render(spec())
    stray[file] +=
      '\nfunc IsStray(ctx context.Context) bool {\n\treturn gateAtLeast(ctx, Baseline, "IsStray")\n}\n'
    assert.throws(
      () => parseTree(stray),
      (err) =>
        err instanceof Unevaluated && new RegExp(`${file}\\.go: gateAtLeast`).test(err.message),
    )
  }
})

test('case 10: single-line and one-per-line lists parse identically', () => {
  const single = parseTree(render(spec({ multiline: false })))
  const multi = parseTree(render(spec({ multiline: true })))
  assert.match(render(spec({ multiline: true })).version, /\[\]Version\{\n\t\t\tBaseline,\n/)
  assert.deepEqual(multi.registry, single.registry)
  assert.equal(multi.current, single.current)
  assert.deepEqual([...multi.contracts], [...single.contracts])
  assert.deepEqual(multi.released, single.released)
  assert.deepEqual(single.registry, ['2026-06-29', '2026-07-04', '2026-07-05'])
  assert.deepEqual(Object.fromEntries(single.contracts), {
    AChange: '2026-07-04',
    BChange: '2026-07-05',
    IsXGate: '2026-07-04',
  })
})

test('case 11: clean — a branch adds one transform to the open window', (t) => {
  const head = spec({
    multiline: true,
    transforms: [...BASE_SPEC.transforms, ['CChange', 'V20260705']],
  })
  const dir = world(t, { prod: PROD_OPEN, head })
  const r = run(dir)
  assert.equal(r.verdict, 'clean', JSON.stringify(r.findings))
  assert.deepEqual(r.findings, [])
  assert.deepEqual(r.target, { action: 'reuse', version: '2026-07-05' })

  const cli = spawnSync(
    process.execPath,
    [SCRIPT, 'check', '--base', 'main', '--production', 'production'],
    {
      cwd: dir,
      encoding: 'utf8',
      env: GIT_ENV,
    },
  )
  assert.equal(cli.status, 0, cli.stderr)
  assert.match(cli.stdout.split('\n')[0], /^target: reuse 2026-07-05$/)
})

// ---- case 12: the citation gate ---------------------------------------------------------------

// Line-pinned references to docs/api-versioning.md rot every time the doc moves (BOS-1364 found
// six that no longer pointed at the rule they cited). Link a heading anchor instead. docs/plans/
// is deliberately outside the scan: it holds dated historical records.
export const CITATION_SCOPE = ['services', 'lib', 'docs/solutions', '.claude/skills']
const CITATION = 'api-versioning\\.md:[0-9]'

/** Return `path:line:text` hits for line-pinned api-versioning.md citations in tracked files. */
export function findLineCitations(root) {
  const r = spawnSync('git', ['grep', '-nE', CITATION, '--', ...CITATION_SCOPE], {
    cwd: root,
    encoding: 'utf8',
    env: GIT_ENV,
    maxBuffer: 64 * 1024 * 1024,
  })
  if (r.status === 1) return []
  if (r.status !== 0) throw new Error(`git grep failed (exit ${r.status}): ${r.stderr}`)
  return r.stdout.split('\n').filter(Boolean)
}

test('case 12: citation gate — no line-pinned docs/api-versioning.md citations', (t) => {
  const hits = findLineCitations(repoRoot)
  assert.deepEqual(
    hits,
    [],
    `cite docs/api-versioning.md by heading anchor (e.g. #skipping-a-bump-justify-it-in-the-diff), not by line number:\n${hits.join('\n')}`,
  )

  // The detector has teeth: a tracked citation in scope is found, one under docs/plans is not.
  const dir = newRepo(t)
  fs.mkdirSync(path.join(dir, 'services/x'), { recursive: true })
  fs.mkdirSync(path.join(dir, 'docs/plans'), { recursive: true })
  fs.writeFileSync(path.join(dir, 'services/x/a.go'), '// see docs/api-versioning.md:12\n')
  fs.writeFileSync(
    path.join(dir, 'services/x/b.go'),
    '// see docs/api-versioning.md#skipping-a-bump-justify-it-in-the-diff\n',
  )
  fs.writeFileSync(path.join(dir, 'docs/plans/old.md'), 'historical docs/api-versioning.md:246\n')
  git(dir, 'add', '-A')
  assert.deepEqual(findLineCitations(dir), ['services/x/a.go:1:// see docs/api-versioning.md:12'])
})
