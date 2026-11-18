import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  utimesSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  depsPlan,
  ensureTrustedDeps,
  materializeTrustedTree,
  needsTrustedDeps,
  trustedDepsMissing,
  trustedDepsPath,
  trustedTreePath,
  reapTrustedTrees,
  runCli,
} from './verify-trusted-tree.mjs'

function fixture(t, lock = 'pnpm-lock.yaml') {
  const dir = mkdtempSync(join(tmpdir(), 'trusted-deps-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const repo = join(dir, 'repo')
  mkdirSync(repo)
  const git = (...args) => {
    const r = spawnSync('git', ['-c', 'commit.gpgsign=false', ...args], {
      cwd: repo,
      encoding: 'utf8',
    })
    assert.equal(r.status, 0, r.stderr)
    return r.stdout.trim()
  }
  git('init', '-q')
  git('config', 'user.email', 'test@example.test')
  git('config', 'user.name', 'Test')
  writeFileSync(join(repo, 'package.json'), JSON.stringify({ private: true }))
  if (lock) writeFileSync(join(repo, lock), '')
  git('add', '.')
  git('commit', '-qm', 'fixture')
  const baseSha = git('rev-parse', 'HEAD')
  return { dir, repo, git, baseSha, options: { baseSha, cwd: repo, tmpRoot: dir } }
}

test('frozen script-free installs publish atomically, reuse, and keep the plain tree package-free', (t) => {
  for (const [lock, manager, flags] of [
    [
      'pnpm-lock.yaml',
      'pnpm',
      [
        'install',
        '--frozen-lockfile',
        '--ignore-scripts',
        '--prefer-offline',
        '--config.strict-dep-builds=false',
      ],
    ],
    [
      'package-lock.json',
      'npm',
      ['ci', '--ignore-scripts', '--prefer-offline', '--no-audit', '--no-fund'],
    ],
  ]) {
    const f = fixture(t, lock)
    const plain = materializeTrustedTree(f.options)
    let calls = 0
    const install = (bin, argv, options) => {
      calls++
      assert.equal(bin, manager)
      assert.deepEqual(argv, flags)
      assert.equal(options.env.CI, 'true')
      assert.equal(options.env.COREPACK_ENABLE_DOWNLOAD_PROMPT, '0')
      assert.ok(options.cwd.startsWith(trustedDepsPath(f.baseSha, f.dir) + '.partial-'))
      assert.equal(existsSync(trustedDepsPath(f.baseSha, f.dir)), false)
      mkdirSync(join(options.cwd, 'node_modules'))
      return { status: 0 }
    }
    const first = ensureTrustedDeps({ ...f.options, install })
    assert.deepEqual(first, { ok: true, root: trustedDepsPath(f.baseSha, f.dir), reused: false })
    assert.equal(existsSync(join(plain.root, 'node_modules')), false)
    assert.ok(existsSync(join(first.root, 'node_modules')))
    assert.equal(ensureTrustedDeps({ ...f.options, install }).reused, true)
    assert.equal(calls, 1)
  }
})

test('failed installs leave nothing published and retry, including timeout and unsupported lockfile', (t) => {
  for (const [result, why] of [
    [{ status: 1 }, 'deps-install-failed:1'],
    [{ status: null, error: { code: 'ETIMEDOUT' } }, 'deps-timeout'],
    [{ status: null, error: { code: 'ENOENT' } }, 'deps-install-failed:ENOENT'],
  ]) {
    const f = fixture(t)
    let calls = 0
    const install = () => {
      calls++
      return result
    }
    assert.equal(ensureTrustedDeps({ ...f.options, install }).why, why)
    assert.equal(ensureTrustedDeps({ ...f.options, install }).ok, false)
    assert.equal(calls, 2)
    assert.deepEqual(readdirSync(join(f.dir, `boss-verify-trusted-${process.getuid()}`)), [])
  }
  const f = fixture(t, 'yarn.lock')
  assert.equal(
    ensureTrustedDeps({ ...f.options, install: () => assert.fail('unsupported') }).why,
    'unsupported-lockfile',
  )
  assert.equal(existsSync(trustedDepsPath(f.baseSha, f.dir)), false)
})

test('concurrent publisher wins without exposing the losing staging tree', (t) => {
  const f = fixture(t)
  const root = trustedDepsPath(f.baseSha, f.dir)
  const r = ensureTrustedDeps({
    ...f.options,
    install: () => {
      mkdirSync(root)
      writeFileSync(join(root, 'winner'), 'complete')
      return { status: 0 }
    },
  })
  assert.equal(r.reused, true)
  assert.equal(readFileSync(join(root, 'winner'), 'utf8'), 'complete')
  assert.deepEqual(readdirSync(join(f.dir, `boss-verify-trusted-${process.getuid()}`)), [
    root.split('/').at(-1),
  ])
})

test('deps detection requires a tree-resident extension, a lockfile, and no deps sibling', (t) => {
  const f = fixture(t)
  const root = f.repo
  const extensions = [{ dir: join(root, '.claude/skills/check') }]
  assert.equal(needsTrustedDeps({ root, extensions }), true)
  assert.equal(trustedDepsMissing({ root, extensions }), true)
  assert.equal(needsTrustedDeps({ root, extensions: [{ dir: root + '-other/check' }] }), false)
  assert.equal(
    needsTrustedDeps({ root, extensions: [{ dir: join(root, 'builtin'), builtin: true }] }),
    false,
  )
  mkdirSync(root + '-deps')
  assert.equal(trustedDepsMissing({ root, extensions }), false)
  assert.deepEqual(depsPlan(fixture(t, null).repo), { needed: false })
})

test('reaping removes stale deps and abandoned staging but judge materialization keeps current deps', (t) => {
  const f = fixture(t)
  const plain = materializeTrustedTree(f.options)
  const deps = trustedDepsPath(f.baseSha, f.dir)
  mkdirSync(deps)
  utimesSync(deps, new Date(1), new Date(1))
  materializeTrustedTree(f.options)
  assert.ok(existsSync(deps))
  const abandoned = deps + '.partial-abandoned'
  mkdirSync(abandoned)
  utimesSync(abandoned, new Date(1), new Date(1))
  assert.deepEqual(
    reapTrustedTrees({ tmpRoot: f.dir, keep: [plain.root] }).sort(),
    [deps, abandoned].sort(),
  )
})

test('deps CLI declines absent extensions, missing locks, and mismatched roots without installing', (t) => {
  const f = fixture(t)
  const root = trustedTreePath(f.baseSha, f.dir)
  const judge = join(f.dir, 'judge.json')
  const run = (facts) => {
    writeFileSync(judge, JSON.stringify({ facts }))
    return runCli(['deps', '--judge', judge], {
      tmpRoot: f.dir,
      install: () => assert.fail('not needed'),
    })
  }
  materializeTrustedTree(f.options)
  assert.equal(
    run({ trustedRoot: root, base: { sha: f.baseSha }, extensionPaths: [] }).out.deps,
    'not-needed',
  )
  assert.equal(
    run({ trustedRoot: f.repo, base: { sha: f.baseSha }, extensionPaths: [{ dir: f.repo }] }).out
      .why,
    'root-mismatch',
  )
  const n = fixture(t, null)
  materializeTrustedTree(n.options)
  const noLockRoot = trustedTreePath(n.baseSha, n.dir)
  writeFileSync(
    judge,
    JSON.stringify({
      facts: {
        trustedRoot: noLockRoot,
        base: { sha: n.baseSha },
        extensionPaths: [{ dir: join(noLockRoot, 'extension') }],
      },
    }),
  )
  assert.equal(
    runCli(['deps', '--judge', judge], { tmpRoot: n.dir, install: () => assert.fail('no lock') })
      .out.deps,
    'not-needed',
  )
  assert.equal(runCli(['deps', '--wat']).code, 1)
})

test('real offline pnpm install resolves a bare import after atomic rename', (t) => {
  const f = fixture(t, null)
  const available = spawnSync('pnpm', ['--version'], { cwd: f.repo, encoding: 'utf8' })
  if (available.error?.code === 'ENOENT') return t.skip('pnpm is not on PATH')
  assert.equal(available.status, 0, available.stderr)
  mkdirSync(join(f.repo, 'local'))
  writeFileSync(
    join(f.repo, 'local/package.json'),
    JSON.stringify({
      name: 'local-proof',
      version: '1.0.0',
      type: 'module',
      exports: './index.mjs',
    }),
  )
  writeFileSync(join(f.repo, 'local/index.mjs'), 'export default 42')
  writeFileSync(
    join(f.repo, 'package.json'),
    JSON.stringify({
      private: true,
      type: 'module',
      dependencies: { 'local-proof': 'file:./local' },
    }),
  )
  writeFileSync(
    join(f.repo, 'proof.mjs'),
    "import value from 'local-proof'; if (value !== 42) process.exit(1)",
  )
  const lock = spawnSync(
    'pnpm',
    [
      'install',
      '--lockfile-only',
      '--offline',
      '--ignore-scripts',
      '--config.strict-dep-builds=false',
    ],
    { cwd: f.repo, encoding: 'utf8', env: { ...process.env, CI: 'true' } },
  )
  assert.equal(lock.status, 0, lock.stderr + lock.stdout)
  f.git('add', '.')
  f.git('commit', '-qm', 'lock')
  const options = { ...f.options, baseSha: f.git('rev-parse', 'HEAD') }
  const plain = materializeTrustedTree(options)
  const r = ensureTrustedDeps({
    ...options,
    install: (bin, argv, opts) => spawnSync(bin, [...argv, '--offline'], opts),
  })
  assert.equal(r.ok, true, r.why)
  assert.equal(existsSync(join(plain.root, 'node_modules')), false)
  const proof = spawnSync(process.execPath, [join(r.root, 'proof.mjs')], { encoding: 'utf8' })
  assert.equal(proof.status, 0, proof.stderr)
})
