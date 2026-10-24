#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { after, test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  DEFAULT_SPEC_DIR,
  UsageError,
  audit,
  loadSpec,
  matchingPathRule,
  parsePrivatePaths,
  prepare,
  replacementRuleMatches,
  uncoveredPrivatePaths,
} from './public-history.mjs'

const scriptPath = fileURLToPath(new URL('./public-history.mjs', import.meta.url))
const selfPath = fileURLToPath(import.meta.url)
const repoRoot = path.resolve(path.dirname(scriptPath), '../../..')
const exporterPath = path.join(repoRoot, '.github', 'scripts', 'export-public-tree.sh')

// The public mirror strips the export script and this spec, so there is no boundary to compare
// there; in this repository both are present and these cases run.
const boundarySkip =
  fs.existsSync(exporterPath) && fs.existsSync(DEFAULT_SPEC_DIR)
    ? false
    : 'export script or public-history spec absent (public mirror): no private boundary to check'

const filterRepoProbe = spawnSync('git-filter-repo', ['--version'], { encoding: 'utf8' })
const filterRepoSkip =
  filterRepoProbe.error || filterRepoProbe.status !== 0
    ? 'git-filter-repo is not installed on PATH, so the end-to-end prepare+audit case did not run'
    : false

// Synthetic identifiers only. The token is assembled at runtime so this file never holds a
// token-shaped string that a real-history secret scan would report.
const LEAK = 'zebracorp-internal'
const MARKER = 'REDACTED-CORP'
const FAKE_TOKEN = 'gh' + 'p_' + 'Q7'.repeat(18)

const tempRoots = []
after(() => {
  for (const dir of tempRoots) fs.rmSync(dir, { recursive: true, force: true })
})

function tempDir(prefix) {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), prefix)))
  tempRoots.push(dir)
  return dir
}

// Fixture repos are isolated from host config (GIT_CONFIG_GLOBAL=/dev/null) and disable
// signing per repo, so a host with commit.gpgsign=true cannot fail them.
const fixtureEnv = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' }
for (const name of ['GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_COMMON_DIR']) {
  delete fixtureEnv[name]
}

function makeRepo(dir) {
  fs.mkdirSync(dir, { recursive: true })
  const git = (args, env = {}) =>
    execFileSync('git', args, {
      cwd: dir,
      env: { ...fixtureEnv, ...env },
      encoding: 'utf8',
    }).trim()
  git(['init', '-q', '-b', 'main'])
  git(['config', 'user.name', 'Fixture Dev'])
  git(['config', 'user.email', 'dev@example.test'])
  git(['config', 'commit.gpgsign', 'false'])
  git(['config', 'tag.gpgsign', 'false'])
  const commit = (files, message, { remove = [], env = {} } = {}) => {
    for (const [file, content] of Object.entries(files)) {
      const target = path.join(dir, file)
      fs.mkdirSync(path.dirname(target), { recursive: true })
      fs.writeFileSync(target, content)
    }
    for (const file of remove) fs.rmSync(path.join(dir, file), { recursive: true })
    git(['add', '-A'])
    git(['commit', '-q', '-m', message], env)
    return git(['rev-parse', 'HEAD'])
  }
  return { dir, git, commit }
}

function writeSpec(dir, overrides = {}) {
  const spec = {
    'paths.txt': '# synthetic boundary\nsecret-notes\n',
    'replacements.txt': `# synthetic identifier\n${LEAK}==>${MARKER}\n`,
    mailmap: '# proposal\nPublic Dev <public@example.test> <leaky@zebracorp-internal.test>\n',
    'secret-allowlist.txt': '# none\n',
    ...overrides,
  }
  fs.mkdirSync(dir, { recursive: true })
  for (const [name, content] of Object.entries(spec))
    fs.writeFileSync(path.join(dir, name), content)
  return dir
}

// A history with every hard-check category: a forbidden path that a later commit deletes, the
// replacement literal in a blob, a commit message, an author identity and a tag message, plus a
// token-shaped string for the report-only secret scan.
function dirtyFixture(root) {
  const repo = makeRepo(path.join(root, 'dirty'))
  const c1 = repo.commit(
    { 'README.md': 'hello\n', 'secret-notes/plan.md': 'private plan\n' },
    'initial import',
  )
  const c2 = repo.commit(
    {
      'src/config.txt': `upstream = ${LEAK}.example.test\n`,
      'src/token.txt': `token = ${FAKE_TOKEN}\n`,
      'src/ops.txt': 'home = /Users/fixtureuser/work\nhost = api-a.ns-a.svc.cluster.local\n',
    },
    `configure the ${LEAK} upstream`,
    { remove: ['secret-notes'] },
  )
  const c3 = repo.commit({ 'README.md': 'hello again\n' }, 'touch readme', {
    env: { GIT_AUTHOR_NAME: 'Leaky', GIT_AUTHOR_EMAIL: `leaky@${LEAK}.test` },
  })
  repo.git(['tag', '-a', 'v1.0.0', '-m', `release notes mention ${LEAK}`])
  return { ...repo, c1, c2, c3 }
}

function cleanFixture(root) {
  const repo = makeRepo(path.join(root, 'clean'))
  repo.commit({ 'README.md': 'hello\n', 'src/app.txt': `name = ${MARKER}\n` }, 'initial import')
  return repo
}

function snapshot(dir) {
  const git = (args) => execFileSync('git', args, { cwd: dir, env: fixtureEnv, encoding: 'utf8' })
  return {
    refs: git(['for-each-ref', '--format=%(refname) %(objectname)']),
    objects: git(['cat-file', '--batch-all-objects', '--batch-check']),
    config: git(['config', '--local', '--list']),
    status: git(['status', '--porcelain']),
    head: git(['symbolic-ref', 'HEAD']),
  }
}

function writeStubFilterRepo(dir) {
  const stub = path.join(dir, 'stub-filter-repo.mjs')
  fs.writeFileSync(
    stub,
    [
      '#!/usr/bin/env node',
      "import fs from 'node:fs'",
      'const args = process.argv.slice(2)',
      'const read = (name) => {',
      '  const at = args.indexOf(name)',
      "  return at === -1 ? null : fs.readFileSync(args[at + 1], 'utf8')",
      '}',
      "fs.writeFileSync('stub-record.json', JSON.stringify({",
      '  cwd: process.cwd(),',
      '  args,',
      "  paths: read('--paths-from-file'),",
      "  replaceText: read('--replace-text'),",
      "  replaceMessage: read('--replace-message'),",
      "  mailmap: read('--mailmap'),",
      '}))',
      '',
    ].join('\n'),
  )
  fs.chmodSync(stub, 0o755)
  return stub
}

function sourceWithRefs(root) {
  const repo = makeRepo(path.join(root, 'source'))
  repo.commit({ 'README.md': 'one\n' }, 'one')
  repo.git(['tag', 'v1.0.0'])
  repo.git(['tag', 'nightly'])
  repo.git(['checkout', '-q', '-b', 'feature'])
  const feature = repo.commit({ 'feature.txt': 'f\n' }, 'feature work')
  repo.git(['checkout', '-q', 'main'])
  repo.commit({ 'README.md': 'two\n' }, 'two')
  repo.git(['update-ref', 'refs/pull/1/head', feature])
  repo.git(['update-ref', 'refs/remotes/upstream/main', feature])
  repo.git(['update-ref', 'refs/stash', feature])
  repo.git(['remote', 'add', 'origin', 'https://example.invalid/private.git'])
  return repo
}

const quiet = () => {}

// ---------------------------------------------------------------------------------------------
// Drift and spec

test(
  'paths.txt covers every PRIVATE_PATHS entry of the export script',
  { skip: boundarySkip },
  () => {
    const privatePaths = parsePrivatePaths(fs.readFileSync(exporterPath, 'utf8'))
    assert.ok(privatePaths.length > 10, `parsed only ${privatePaths.length} PRIVATE_PATHS entries`)
    assert.deepEqual(uncoveredPrivatePaths(privatePaths, loadSpec().pathRules), [])
  },
)

test(
  'a fake PRIVATE_PATHS entry in a copy of the export script fails the drift check',
  {
    skip: boundarySkip,
  },
  () => {
    const dir = tempDir('public-history-drift-')
    const copy = path.join(dir, 'export-public-tree.sh')
    const original = fs.readFileSync(exporterPath, 'utf8')
    const edited = original.replace(/^PRIVATE_PATHS=\(/m, 'PRIVATE_PATHS=(fake-private-root ')
    assert.notEqual(edited, original, 'the copy did not gain the fake entry')
    fs.writeFileSync(copy, edited)
    const uncovered = uncoveredPrivatePaths(
      parsePrivatePaths(fs.readFileSync(copy, 'utf8')),
      loadSpec().pathRules,
    )
    assert.deepEqual(uncovered, ['fake-private-root'])
  },
)

test(
  'the committed spec removes itself and no replacement re-creates a left-hand side',
  {
    skip: boundarySkip,
  },
  () => {
    const spec = loadSpec()
    assert.ok(spec.replacements.length > 0)
    for (const file of ['README.md', 'paths.txt', 'replacements.txt', 'mailmap']) {
      const rel = path.relative(repoRoot, path.join(DEFAULT_SPEC_DIR, file))
      assert.ok(matchingPathRule(spec.pathRules, rel), `${rel} would survive the filter`)
    }
  },
)

test('the tool and its tests carry no replacement left-hand side', { skip: boundarySkip }, () => {
  // Both files survive the filter, so a real identifier in either would fail the hard check.
  const spec = loadSpec()
  for (const file of [scriptPath, selfPath]) {
    const text = fs.readFileSync(file, 'utf8')
    const hits = spec.replacements.filter((rule) => replacementRuleMatches(rule, text))
    assert.deepEqual(
      hits.map((rule) => rule.text),
      [],
      `${path.basename(file)} holds an identifier the filter replaces`,
    )
  }
})

// ---------------------------------------------------------------------------------------------
// prepare

test('a glob replacement rule matches its pattern anywhere inside the text', () => {
  const root = tempDir('public-history-glob-')
  const spec = loadSpec(
    writeSpec(path.join(root, 'spec'), { 'replacements.txt': 'glob:zebra?corp*.test==>X\n' }),
  )
  const [rule] = spec.replacements
  assert.equal(rule.kind, 'glob')
  assert.ok(replacementRuleMatches(rule, 'see leaky@zebra-corp-internal.test today'))
  assert.ok(!replacementRuleMatches(rule, 'corp zebra.test'))
})

test('prepare refuses a non-empty scratch directory', () => {
  const root = tempDir('public-history-prep-')
  const source = sourceWithRefs(root)
  const scratch = path.join(root, 'scratch')
  fs.mkdirSync(scratch)
  fs.writeFileSync(path.join(scratch, 'keep.txt'), 'existing\n')
  const stub = writeStubFilterRepo(root)
  assert.throws(
    () =>
      prepare({
        source: source.dir,
        scratch,
        spec: writeSpec(path.join(root, 'spec')),
        filterRepo: stub,
        log: quiet,
      }),
    (error) => error instanceof UsageError && /not empty/.test(error.message),
  )
  assert.deepEqual(fs.readdirSync(scratch), ['keep.txt'])
})

test('prepare refuses a scratch directory inside the source work tree', () => {
  const root = tempDir('public-history-prep-')
  const source = sourceWithRefs(root)
  const before = snapshot(source.dir)
  const scratch = path.join(source.dir, 'out', 'scratch')
  assert.throws(
    () =>
      prepare({
        source: source.dir,
        scratch,
        spec: writeSpec(path.join(root, 'spec')),
        filterRepo: writeStubFilterRepo(root),
        log: quiet,
      }),
    (error) => error instanceof UsageError && /inside the source/.test(error.message),
  )
  assert.equal(fs.existsSync(path.join(source.dir, 'out')), false)
  assert.deepEqual(snapshot(source.dir), before)
})

test('prepare refuses a scratch directory inside another git work tree', () => {
  const root = tempDir('public-history-prep-')
  const source = sourceWithRefs(root)
  const other = makeRepo(path.join(root, 'other'))
  assert.throws(
    () =>
      prepare({
        source: source.dir,
        scratch: path.join(other.dir, 'nested', 'scratch'),
        spec: writeSpec(path.join(root, 'spec')),
        filterRepo: writeStubFilterRepo(root),
        log: quiet,
      }),
    (error) => error instanceof UsageError && /inside a git repository/.test(error.message),
  )
})

test('prepare mirrors into scratch, keeps only the default refs, drops remotes and runs the filter there', () => {
  const root = tempDir('public-history-prep-')
  const source = sourceWithRefs(root)
  const specDir = writeSpec(path.join(root, 'spec'))
  const before = snapshot(source.dir)
  const scratch = path.join(root, 'scratch')

  const result = prepare({
    source: source.dir,
    scratch,
    spec: specDir,
    filterRepo: writeStubFilterRepo(root),
    log: quiet,
  })

  const git = (args) =>
    execFileSync('git', args, { cwd: scratch, env: fixtureEnv, encoding: 'utf8' })
  const refs = git(['for-each-ref', '--format=%(refname)']).split('\n').filter(Boolean).sort()
  assert.deepEqual(refs, ['refs/heads/main', 'refs/tags/v1.0.0'])
  assert.deepEqual(result.keptRefs.slice().sort(), refs)
  assert.equal(git(['remote']).trim(), '')
  assert.equal(
    spawnSync('git', ['config', '--get-regexp', '^remote\\.'], { cwd: scratch }).stdout.toString(),
    '',
  )

  const record = JSON.parse(fs.readFileSync(path.join(scratch, 'stub-record.json'), 'utf8'))
  assert.equal(record.cwd, fs.realpathSync(scratch))
  assert.ok(record.args.includes('--invert-paths'))
  assert.equal(record.paths, fs.readFileSync(path.join(specDir, 'paths.txt'), 'utf8'))
  assert.equal(
    record.replaceText,
    `${LEAK}==>${MARKER}\n`,
    'replacements reach the filter without comments',
  )
  assert.equal(record.replaceMessage, record.replaceText)
  assert.equal(record.mailmap, fs.readFileSync(path.join(specDir, 'mailmap'), 'utf8'))

  assert.deepEqual(snapshot(source.dir), before, 'the source repository changed')
})

test('prepare honours --keep-ref and repoints HEAD at a kept branch', () => {
  const root = tempDir('public-history-prep-')
  const source = sourceWithRefs(root)
  const scratch = path.join(root, 'scratch')
  prepare({
    source: source.dir,
    scratch,
    spec: writeSpec(path.join(root, 'spec')),
    keepRefs: ['feature'],
    filterRepo: writeStubFilterRepo(root),
    log: quiet,
  })
  const git = (args) =>
    execFileSync('git', args, { cwd: scratch, env: fixtureEnv, encoding: 'utf8' }).trim()
  assert.equal(git(['for-each-ref', '--format=%(refname)']), 'refs/heads/feature')
  assert.equal(git(['symbolic-ref', 'HEAD']), 'refs/heads/feature')
})

test('the tool has no push code path', () => {
  const source = fs.readFileSync(scriptPath, 'utf8')
  assert.doesNotMatch(source, /['"`]push['"`]/)
})

// ---------------------------------------------------------------------------------------------
// audit

test('audit fails the hard checks on a dirty history and names each finding with commit and path', async () => {
  const root = tempDir('public-history-audit-')
  const repo = dirtyFixture(root)
  const report = path.join(root, 'report.md')
  const result = await audit({ repo: repo.dir, spec: writeSpec(path.join(root, 'spec')), report })
  const findings = result.hardFindings.map((f) => `${f.check}|${f.where}|${f.commit}|${f.path}`)

  assert.ok(
    findings.includes(`forbidden-path|path (in 1 commit(s))|${repo.c1}|secret-notes/plan.md`),
    findings.join('\n'),
  )
  assert.ok(
    findings.includes(`replacement-lhs|blob|${repo.c2}|src/config.txt`),
    findings.join('\n'),
  )
  assert.ok(findings.includes(`replacement-lhs|commit message|${repo.c2}|`), findings.join('\n'))
  assert.ok(findings.includes(`replacement-lhs|author identity|${repo.c3}|`), findings.join('\n'))
  assert.ok(findings.includes('replacement-lhs|tag message||refs/tags/v1.0.0'), findings.join('\n'))

  assert.equal(result.secretHits.length, 1)
  assert.equal(result.secretHits[0].family, 'github-token')
  assert.equal(result.secretHits[0].path, 'src/token.txt')
  assert.equal(result.secretHits[0].commit, repo.c2)
  assert.ok(result.homes.has('/Users/fixtureuser'))
  assert.ok(result.hostnames.has('api-a.ns-a.svc.cluster.local'))
  assert.ok(result.emails.has('zebracorp-internal.test'))
  assert.ok(result.historyOnly.includes('secret-notes/plan.md'))
  assert.equal(result.commitCount, 3)
  assert.ok(result.largestBlobs.length > 0 && result.largestBlobs.length <= 20)

  const text = fs.readFileSync(report, 'utf8')
  for (const needle of [
    repo.c1.slice(0, 12),
    repo.c2.slice(0, 12),
    'secret-notes/plan.md',
    'src/config.txt',
    'src/token.txt',
  ]) {
    assert.ok(text.includes(needle), `report does not name ${needle}`)
  }
  assert.ok(!text.includes(FAKE_TOKEN), 'report leaked the full token value')
})

test('audit passes a clean history and lists tip files carrying a replacement marker', async () => {
  const root = tempDir('public-history-audit-')
  const repo = cleanFixture(root)
  const result = await audit({
    repo: repo.dir,
    spec: writeSpec(path.join(root, 'spec')),
    report: path.join(root, 'report.md'),
  })
  assert.deepEqual(result.hardFindings, [])
  assert.deepEqual(result.secretHits, [])
  assert.deepEqual(result.tipChanged, [{ path: 'src/app.txt', rules: [LEAK] }])
})

test('audit reports a forbidden path that a history holds only as a gitlink', async () => {
  const root = tempDir('public-history-audit-')
  const repo = cleanFixture(root)
  const target = repo.git(['rev-parse', 'HEAD'])
  repo.git(['update-index', '--add', '--cacheinfo', `160000,${target},secret-notes`])
  repo.git(['commit', '-q', '-m', 'add a gitlink'])
  const gitlinkCommit = repo.git(['rev-parse', 'HEAD'])
  const result = await audit({
    repo: repo.dir,
    spec: writeSpec(path.join(root, 'spec')),
    report: path.join(root, 'report.md'),
  })
  const findings = result.hardFindings.map((f) => `${f.check}|${f.commit}|${f.path}`)
  assert.ok(findings.includes(`forbidden-path|${gitlinkCommit}|secret-notes`), findings.join('\n'))
})

test('audit fails instead of passing when a history blob is missing from the object store', async () => {
  const root = tempDir('public-history-audit-')
  const repo = cleanFixture(root)
  const oid = repo.git(['rev-parse', 'HEAD:src/app.txt'])
  fs.rmSync(path.join(repo.dir, '.git', 'objects', oid.slice(0, 2), oid.slice(2)))
  await assert.rejects(
    audit({
      repo: repo.dir,
      spec: writeSpec(path.join(root, 'spec')),
      report: path.join(root, 'report.md'),
    }),
    (error) => error.message.includes('missing') && error.message.includes(oid),
  )
})

test('audit lists an allowlisted fixture token as allowlisted, not as a hit', async () => {
  const root = tempDir('public-history-audit-')
  const repo = makeRepo(path.join(root, 'allow'))
  repo.commit({ 'fixtures/fake-token.txt': `${FAKE_TOKEN}\n` }, 'add a fake token fixture')
  const result = await audit({
    repo: repo.dir,
    spec: writeSpec(path.join(root, 'spec'), { 'secret-allowlist.txt': 'fixtures/* ^ghp_\n' }),
    report: path.join(root, 'report.md'),
  })
  assert.deepEqual(result.secretHits, [])
  assert.equal(result.allowlisted.length, 1)
  assert.equal(result.allowlisted[0].path, 'fixtures/fake-token.txt')
  assert.deepEqual(result.hardFindings, [])
})

// ---------------------------------------------------------------------------------------------
// CLI

function cli(args) {
  return spawnSync(process.execPath, [scriptPath, ...args], { encoding: 'utf8', env: fixtureEnv })
}

test('the CLI rejects an unknown flag, a flag of the other subcommand, and an unknown subcommand', () => {
  const unknown = cli(['audit', '--bogus', 'x'])
  assert.equal(unknown.status, 2)
  assert.match(unknown.stderr, /--bogus/)
  const crossed = cli(['audit', '--scratch', 'x'])
  assert.equal(crossed.status, 2)
  assert.match(crossed.stderr, /--scratch/)
  const verb = cli(['publish'])
  assert.equal(verb.status, 2)
  assert.match(verb.stderr, /unknown or missing subcommand/)
})

test('the CLI audit exits 0 on a clean history and 1 on a dirty one', () => {
  const root = tempDir('public-history-cli-')
  const spec = writeSpec(path.join(root, 'spec'))
  const clean = cleanFixture(root)
  const dirty = dirtyFixture(root)
  const ok = cli([
    'audit',
    '--repo',
    clean.dir,
    '--spec',
    spec,
    '--report',
    path.join(root, 'clean.md'),
  ])
  assert.equal(ok.status, 0, ok.stderr)
  assert.ok(fs.existsSync(path.join(root, 'clean.md')))
  const bad = cli([
    'audit',
    '--repo',
    dirty.dir,
    '--spec',
    spec,
    '--report',
    path.join(root, 'dirty.md'),
  ])
  assert.equal(bad.status, 1, bad.stderr)
  assert.ok(JSON.parse(bad.stdout).hardFindings > 0)
})

// ---------------------------------------------------------------------------------------------
// End to end with the real git-filter-repo

test(
  'prepare then audit with git-filter-repo removes the forbidden path and literal',
  {
    skip: filterRepoSkip,
  },
  async () => {
    const root = tempDir('public-history-e2e-')
    const source = dirtyFixture(root)
    const spec = writeSpec(path.join(root, 'spec'))
    const before = snapshot(source.dir)
    const scratch = path.join(root, 'scratch')

    prepare({ source: source.dir, scratch, spec, log: quiet })
    const result = await audit({ repo: scratch, spec, report: path.join(root, 'report.md') })

    assert.deepEqual(result.hardFindings, [])
    const git = (args) =>
      execFileSync('git', args, { cwd: scratch, env: fixtureEnv, encoding: 'utf8' })
    const everything = [
      git(['log', '--all', '--name-only', '--format=%an <%ae>%n%cn <%ce>%n%B']),
      git(['log', '--all', '-p', '--format=']),
      git(['for-each-ref', 'refs/tags', '--format=%(contents)']),
    ].join('\n')
    assert.doesNotMatch(everything, /secret-notes/)
    assert.ok(!everything.includes(LEAK), 'the replacement literal survived the filter')
    assert.ok(everything.includes(MARKER))
    assert.ok(everything.includes('Public Dev <public@example.test>'), 'mailmap was not applied')
    assert.deepEqual(
      result.tipChanged.map((t) => t.path),
      ['src/config.txt'],
    )
    assert.deepEqual(snapshot(source.dir), before, 'the source repository changed')
  },
)
