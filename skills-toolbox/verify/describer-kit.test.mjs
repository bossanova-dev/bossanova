import test from 'node:test'
import assert from 'node:assert/strict'
import { Readable } from 'node:stream'

import {
  MAX_BLOCKS,
  MAX_RESULT_BYTES,
  blockFact,
  boundDescription,
  changedFiles,
  cliMain,
  describerMain,
  envelopeDescriber,
  gitAt,
  item,
  renderItems,
  resolveRange,
  riskFact,
  roleFacts,
  signalFact,
} from './describer-kit.mjs'
import { refineDescription } from './verify-describe.mjs'
import { MAX_CODE_LINES } from './verify-describe-blocks.mjs'
import { MAX_TEXT_CHARS } from './verify-proof.mjs'
import { gitFixture } from './verify-git-fixture.mjs'

const steps = (text = 'Run it') => ({ kind: 'steps', steps: [text] })
const signal = (key, summary = 'Adds a column') => ({
  key,
  label: 'Database',
  state: 'additive',
  summary,
})
const risk = (level = 'high') => ({
  level,
  title: 'Anyone can run billing',
  impact: 'An unauthenticated POST moves invoices',
  at: { path: 'api/routes.go', line: 7 },
})
const byPath = (changes) => Object.fromEntries(changes.map((change) => [change.path, change]))

test('changedFiles reports adds, edits, deletes, renames with their old path, and copies as adds', (t) => {
  const repo = gitFixture(t)
  const body = Array.from({ length: 40 }, (_, i) => `line ${i}`).join('\n')
  const other = Array.from({ length: 40 }, (_, i) => `other ${i}`).join('\n')
  repo.write({ 'keep.txt': 'a\n', 'gone.txt': 'b\n', 'old/name.txt': body, 'src.txt': other })
  const base = repo.commit('base')
  repo.move('old/name.txt', 'new/name.txt')
  repo.write({ 'keep.txt': 'changed\n', 'gone.txt': null, 'added.txt': 'new\n' })
  repo.write({ 'copy.txt': other })
  const head = repo.commit('head')

  const changes = byPath(changedFiles(gitAt(repo.dir), base, head))
  assert.deepEqual(changes['keep.txt'], { path: 'keep.txt', status: 'M' })
  assert.deepEqual(changes['gone.txt'], { path: 'gone.txt', status: 'D' })
  assert.deepEqual(changes['added.txt'], { path: 'added.txt', status: 'A' })
  assert.deepEqual(changes['new/name.txt'], {
    path: 'new/name.txt',
    previousPath: 'old/name.txt',
    status: 'R',
  })
  assert.equal(changes['copy.txt'].status, 'A')
  assert.equal(changes['copy.txt'].previousPath, undefined)
  assert.equal(changes['old/name.txt'], undefined)
})

test('resolveRange diffs from the merge base and names a ref it cannot read', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'shared.txt': 'one\n' })
  repo.commit('root')
  repo.git('checkout', '--quiet', '-b', 'feature')
  repo.write({ 'mine.txt': 'mine\n' })
  const head = repo.commit('feature work')
  repo.git('checkout', '--quiet', 'main')
  repo.write({ 'theirs.txt': 'merged after branching\n' })
  const baseTip = repo.commit('base moved on')

  const git = gitAt(repo.dir)
  const range = resolveRange(git, baseTip, head)
  assert.equal(range.head, head)
  assert.deepEqual(changedFiles(git, range.base, range.head), [{ path: 'mine.txt', status: 'A' }])
  assert.throws(() => resolveRange(git, 'no-such-ref', head), /no-such-ref/)
})

function areaRepo(t) {
  const repo = gitFixture(t)
  const body = Array.from({ length: 40 }, (_, i) => `row ${i}`).join('\n')
  repo.write({ 'api/old.go': body, 'web/page.tsx': 'a\n' })
  const baseSha = repo.commit('base')
  repo.move('api/old.go', 'api/new.go')
  repo.write({ 'web/page.tsx': 'b\n' })
  const headSha = repo.commit('head')
  return { repo, baseSha, headSha }
}

test('envelopeDescriber keeps in-area renames by their old path and scopes facts to the area', (t) => {
  const { repo, baseSha, headSha } = areaRepo(t)
  let seen
  const describe = envelopeDescriber((input) => {
    seen = input
    return [
      item('api', 'Renames the routes', ['api/new.go']),
      item('web', 'Changes the page', ['web/page.tsx'], true),
      blockFact('api', steps(), ['api/old.go']),
      signalFact(signal('db'), ['web/page.tsx']),
      riskFact('api', risk()),
    ]
  })
  const facts = describe(
    { area: { area: 'api', paths: ['api/old.go'] }, baseSha, headSha },
    { cwd: repo.dir },
  )
  assert.deepEqual(seen.changes, [{ path: 'api/new.go', previousPath: 'api/old.go', status: 'R' }])
  assert.equal(seen.allChanges.length, 2)
  assert.deepEqual(facts, { breaking: false, items: [], blocks: [steps()] })
})

test('envelopeDescriber gives an empty result for no paths and throws on a missing sha', (t) => {
  const { repo, headSha } = areaRepo(t)
  const describe = envelopeDescriber(() => {
    throw new Error('not reached')
  })
  assert.deepEqual(describe({ area: { area: 'api', paths: [] } }, { cwd: repo.dir }), {
    breaking: false,
    items: [],
  })
  assert.throws(
    () => describe({ area: { area: 'api', paths: ['api/new.go'] }, headSha }, { cwd: repo.dir }),
    /baseSha/,
  )
})

test('roleFacts keeps only facts with an in-area path and leaves empty typed lists out', () => {
  const facts = roleFacts(
    [
      item('api', 'In the area', ['api/a.go'], true),
      item('api', 'Elsewhere', ['web/b.tsx'], true),
      riskFact('api', risk()),
      { area: 'api', text: 'No paths', paths: [] },
    ],
    ['api/a.go'],
  )
  assert.deepEqual(facts, { breaking: true, items: ['In the area'] })
})

test('boundDescription drops an invalid entry alone, dedupes signal keys and sends no undisclosed key', () => {
  const result = boundDescription('kit-test', {
    items: ['first\nline', 'first line'],
    breaking: true,
    blocks: [steps('one'), { kind: 'nope' }, steps('two')],
    signals: [signal('db'), signal('db', 'Second'), { key: 'bad', state: 'nope' }, signal('ui')],
    risks: [risk('extreme'), risk()],
  })
  assert.deepEqual(refineDescription(result), [])
  assert.equal(Object.hasOwn(result, 'undisclosed'), false)
  assert.deepEqual(result.items, ['first line'])
  assert.equal(result.breaking, true)
  assert.deepEqual(result.blocks, [steps('one'), steps('two')])
  assert.deepEqual(result.signals, [signal('db'), signal('ui')])
  assert.deepEqual(result.risks, [risk()])
})

test('boundDescription keeps a valid signal whose key an earlier invalid signal shares', () => {
  const result = boundDescription('kit-test', {
    items: [],
    signals: [{ ...signal('db'), state: 'nope' }, signal('db', 'Second')],
  })
  assert.deepEqual(refineDescription(result), [])
  assert.deepEqual(result.signals, [signal('db', 'Second')])
})

test('boundDescription leaves out typed lists with nothing valid and caps each list', () => {
  const result = boundDescription('kit-test', {
    items: [],
    blocks: Array.from({ length: MAX_BLOCKS + 3 }, (_, i) => steps(`step ${i}`)),
    risks: [risk('extreme')],
  })
  assert.deepEqual(refineDescription(result), [])
  assert.equal(result.blocks.length, MAX_BLOCKS)
  assert.equal(result.risks, undefined)
  assert.equal(result.signals, undefined)
})

test('boundDescription pops blocks from the end until the result fits MAX_RESULT_BYTES', () => {
  const line = 'x'.repeat(MAX_TEXT_CHARS)
  const code = (i) => ({
    kind: 'code',
    path: `big/${i}.go`,
    start: 1,
    lines: Array(MAX_CODE_LINES).fill(line),
  })
  const blocks = Array.from({ length: MAX_BLOCKS }, (_, i) => code(i))
  assert.ok(Buffer.byteLength(JSON.stringify(blocks)) > MAX_RESULT_BYTES)
  const result = boundDescription('kit-test', { items: ['x'], blocks })
  assert.deepEqual(refineDescription(result), [])
  assert.ok(Buffer.byteLength(JSON.stringify(result)) <= MAX_RESULT_BYTES)
  assert.ok(result.blocks.length > 0 && result.blocks.length < MAX_BLOCKS)
  assert.deepEqual(result.blocks, blocks.slice(0, result.blocks.length))
})

test('describerMain reads the envelope from stdin, writes one bounded result and rejects flags', async () => {
  const envelope = { extension: 'kit-test', area: { area: 'api', paths: [] } }
  let written = ''
  await describerMain(() => ({ items: ['One'], blocks: [{ kind: 'nope' }, steps()] }), {
    argv: [],
    stdin: Readable.from([JSON.stringify(envelope)]),
    stdout: { write: (text) => (written += text) },
    cwd: '/',
  })
  const result = JSON.parse(written)
  assert.deepEqual(refineDescription(result), [])
  assert.equal(result.extension, 'kit-test')
  assert.deepEqual(result.items, ['One'])
  assert.deepEqual(result.blocks, [steps()])
  await assert.rejects(
    describerMain(() => ({}), { argv: ['--x'], stdin: Readable.from(['{}']), stdout: {} }),
  )
})

function cliRepo(t) {
  const repo = gitFixture(t)
  repo.write({ 'a.txt': 'a\n' })
  const base = repo.commit('base')
  repo.write({ 'b.txt': 'b\n' })
  const head = repo.commit('head')
  const collect = ({ changes }) => [
    ...changes.map((change) => item('files', `${change.status} ${change.path}`, [change.path])),
    blockFact('files', steps('Run it'), ['b.txt']),
  ]
  return { repo, base, head, collect }
}

test('cliMain reports the range as JSON or one line per fact', (t) => {
  const { repo, base, head, collect } = cliRepo(t)
  const json = JSON.parse(cliMain('kit', collect, ['--base', base, '--json'], { cwd: repo.dir }))
  assert.equal(json.base, base)
  assert.equal(json.head, head)
  assert.deepEqual(
    json.items.map((fact) => fact.text ?? fact.block.kind),
    ['A b.txt', 'steps'],
  )
  const plain = cliMain('kit', collect, ['--base', base, '--head', head], { cwd: repo.dir })
  assert.equal(plain, renderItems(collect({ changes: [{ path: 'b.txt', status: 'A' }] })))
  assert.equal(plain.trim().split('\n').length, 2)
})

test('cliMain --pr reads the PR through the injected reader and diffs from origin/<base branch>', (t) => {
  const { repo, base, head, collect } = cliRepo(t)
  repo.git('update-ref', 'refs/remotes/origin/develop', base)
  const calls = []
  const out = JSON.parse(
    cliMain('kit', collect, ['--pr', '7', '--json'], {
      cwd: repo.dir,
      readPullRequest: (number) => {
        calls.push(number)
        return { headRefOid: head, baseRefName: 'develop' }
      },
    }),
  )
  assert.deepEqual(calls, [7])
  assert.equal(out.base, base)
  assert.equal(out.head, head)
})

test('cliMain rejects an unknown flag, a non-numeric PR and a missing base', (t) => {
  const { repo, collect } = cliRepo(t)
  const reader = () => assert.fail('no PR read expected')
  for (const argv of [['--bogus'], ['--pr', 'x'], ['--json']])
    assert.throws(() => cliMain('kit', collect, argv, { cwd: repo.dir, readPullRequest: reader }))
})
