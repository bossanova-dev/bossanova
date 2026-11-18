import path from 'node:path'
import { isMainModule } from '../main-module.mjs'
import {
  basename,
  describerMain,
  envelopeDescriber,
  item,
  listed,
  plural,
  runCli,
  signalFact,
  MAX_TEXT_CHARS,
} from './describer-kit.mjs'
import { isTestFile } from './verify-proof-spec.mjs'

export const MAX_TEST_FILES_READ = 200
const MAX_BLOB_BYTES = 1024 * 1024
const MAX_AREA_TESTS = 6
/** @typedef {import('./describer-kit.mjs').Change} Change */
/** @typedef {Change & {added: string[], removed: string[], unread?: boolean, contentChanged?: boolean}} TestChange */
/** @param {string} value */
const bounded = (value) => value.replace(/[\r\n\u2028\u2029]+/g, ' ').slice(0, MAX_TEXT_CHARS)
/** @param {string} value */
const quoted = (value) => `\`${value}\``
/** @param {string} file */
const isDiscoverableTest = (file) => isTestFile(file) && !/(^|\/)testdata\//.test(file)

/** Lexical declarations only; dynamic JavaScript titles are deliberately absent.
 * @param {string} file @param {string} source @returns {string[]} */
export function testCases(file, source) {
  let pattern
  if (file.endsWith('.go')) pattern = /^func\s+((?:Test|Benchmark|Fuzz|Example)\w*)\s*\(/gm
  else if (file.endsWith('.py')) pattern = /^[ \t]*(?:async\s+)?def\s+(test_\w+)\s*\(/gm
  else if (/\.(?:[cm]?[jt]s|[jt]sx)$/.test(file)) {
    pattern =
      /^[ \t]*(?:test|it|describe)(?:\.(?:only|skip|todo))?\s*\(\s*(['"`])((?:\\.|(?!\1)[^\\\r\n])*)\1/gm
    return [
      ...new Set(
        [...source.matchAll(pattern)]
          .filter((m) => m[1] !== '`' || !m[2].includes('${'))
          .map((m) => m[2]),
      ),
    ]
  } else return []
  return [...new Set([...source.matchAll(pattern)].map((m) => m[1]))]
}

/** Changed non-test subjects; mapping expresses a likely relationship, not coverage.
 * @param {string} testPath @param {Change[]} changes @returns {string[]} */
export function testSubjects(testPath, changes) {
  const dir = path.posix.dirname(testPath)
  const go = testPath.endsWith('_test.go')
  const dirs = new Set([dir])
  if (!go && ['__tests__', 'tests', 'test'].includes(basename(dir)))
    dirs.add(path.posix.dirname(dir))
  /** @param {string} file */
  const stem = (file) =>
    basename(file)
      .replace(/\.[^.]+$/, '')
      .replace(/\.(?:test|spec)$|_test$|^test_/g, '')
  return changes
    .filter(
      (c) =>
        !isTestFile(c.path) &&
        !/(^|\/)testdata\//.test(c.path) &&
        dirs.has(path.posix.dirname(c.path)) &&
        (go ? c.path.endsWith('.go') : stem(c.path) === stem(testPath)),
    )
    .map((c) => c.path)
}

/** @param {TestChange[]} changes */
export function testsSignal(changes) {
  if (!changes.length) return null
  const counts = [
    ['A', 'added'],
    ['M', 'changed'],
    ['D', 'removed'],
    ['R', 'renamed'],
  ]
    .map(([status, label]) => [changes.filter((c) => c.status === status).length, label])
    .filter(([count]) => count)
  const added = changes.reduce((n, c) => n + c.added.length, 0)
  const unread = changes.filter((c) => c.unread).length
  return {
    key: 'tests',
    label: 'Tests',
    state: changes.some(
      (c) =>
        c.status === 'D' ||
        c.contentChanged ||
        (c.previousPath && isDiscoverableTest(c.previousPath) && !isDiscoverableTest(c.path)) ||
        c.removed.length ||
        (c.unread && c.status !== 'A'),
    )
      ? 'review'
      : changes.some((c) => c.status === 'A' || c.added.length)
        ? 'additive'
        : 'safe',
    summary: bounded(
      `${plural(changes.length, 'test file')} (${counts.map(([count, label]) => `${count} ${label}`).join(', ')}), ${plural(added, 'new case')}${unread ? `, ${unread} not inspected` : ''}: ${listed(changes.map((c) => quoted(basename(c.path))))}`,
    ),
  }
}

/** @param {string[] | undefined} checks */
export function checksSignal(checks) {
  const names = [...new Set(checks ?? [])]
  return names.length
    ? {
        key: 'checks',
        label: 'Checks',
        state: 'proven',
        summary: bounded(`${names.length} passed on this head: ${listed(names)}`),
      }
    : null
}

/** @param {TestChange} change */
function testText(change) {
  const status =
    change.status === 'R'
      ? `renamed from ${quoted(basename(change.previousPath ?? ''))}`
      : { A: 'added', M: 'changed', D: 'removed' }[change.status]
  const cases = [
    change.unread ? 'case names not inspected' : '',
    change.added.length ? `new ${listed(change.added.map(quoted))}` : '',
    change.removed.length ? `removed ${listed(change.removed.map(quoted))}` : '',
  ].filter(Boolean)
  return bounded(
    `Tests ${quoted(basename(change.path))} (${status})${cases.length ? `: ${cases.join('; ')}` : ''}`,
  )
}

/** @param {import('./describer-kit.mjs').Git} git @param {string} ref @param {string[]} files */
function readTests(git, ref, files) {
  if (!files.length) return new Map()
  const readable = git
    .run(['ls-tree', '-rl', '-z', ref, '--', ...files])
    .split('\0')
    .flatMap((row) => {
      const entry = /^\d+ blob \S+\s+(\d+)\t([\s\S]+)$/.exec(row)
      return entry && Number(entry[1]) <= MAX_BLOB_BYTES ? [entry[2]] : []
    })
  return git.readFiles(ref, readable)
}

/** @param {Parameters<import('./describer-kit.mjs').CollectItems>[0]} input
 * @param {any} [envelope] @returns {import('./describer-kit.mjs').Fact[]} */
function collectTestFacts({ git, base, head, allChanges }, envelope) {
  const tests = allChanges.filter(
    (c) => isDiscoverableTest(c.path) || (c.previousPath && isDiscoverableTest(c.previousPath)),
  )
  const readable = tests.slice(0, MAX_TEST_FILES_READ)
  const before = readTests(
    git,
    base,
    readable.filter((c) => c.status !== 'A').map((c) => c.previousPath ?? c.path),
  )
  const after = readTests(
    git,
    head,
    readable.filter((c) => c.status !== 'D').map((c) => c.path),
  )
  /** @param {string} file @param {string | undefined} text */
  const names = (file, text) =>
    isDiscoverableTest(file) && text && Buffer.byteLength(text) <= MAX_BLOB_BYTES
      ? testCases(file, text)
      : []
  const changes = tests.map((c) => {
    const available =
      (c.status === 'A' || before.has(c.previousPath ?? c.path)) &&
      (c.status === 'D' || after.has(c.path))
    if (!available) return { ...c, added: [], removed: [], unread: true }
    const old = names(c.previousPath ?? c.path, before.get(c.previousPath ?? c.path))
    const next = names(c.path, after.get(c.path))
    return {
      ...c,
      // Case names cannot establish that existing assertions still run or remain intact.
      contentChanged:
        isDiscoverableTest(c.previousPath ?? c.path) &&
        c.status !== 'A' &&
        before.get(c.previousPath ?? c.path) !== after.get(c.path),
      added: next.filter((n) => !old.includes(n)),
      removed: old.filter((n) => !next.includes(n)),
    }
  })
  const areaPaths = new Set(envelope?.area?.paths ?? allChanges.map((c) => c.path))
  const facts = changes
    .map((c) => {
      const subjects = testSubjects(c.path, allChanges)
      return item('tests', testText(c), subjects.length ? subjects : [c.path])
    })
    .filter((f) => f.paths.some((p) => areaPaths.has(p)))
  const kept = facts.slice(0, MAX_AREA_TESTS)
  if (facts.length > MAX_AREA_TESTS)
    kept.push(
      item('tests', `Tests: ${facts.length - MAX_AREA_TESTS} more files changed`, [...areaPaths]),
    )
  if (envelope?.area?.area === 'housekeeping') {
    for (const signal of [testsSignal(changes), checksSignal(envelope.checks)])
      if (signal) kept.push(signalFact(signal, [...areaPaths]))
  }
  return kept
}

/** @param {any} envelope @param {{cwd?: string}} [options] */
export const describeTests = (envelope, options) =>
  envelopeDescriber((input) => collectTestFacts(input, envelope))(envelope, options)
export const main = () => describerMain(describeTests)
if (isMainModule(import.meta.url)) {
  if (process.argv.length > 2) runCli(import.meta.url, 'boss-verify-tests', collectTestFacts)
  else await main()
}
