import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'

import {
  CONSUMER_ROOTS,
  exportedNames,
  generatedHeader,
  SOURCE_DIR,
  syncDocsWebHelpers,
  topLevelDeclarations,
  VENDOR_DIR,
  VENDORED_MODULES,
} from './sync-docs-web-helpers.mjs'

const SCRIPT = fileURLToPath(new URL('./sync-docs-web-helpers.mjs', import.meta.url))

const FIXTURE_ROOTS = []
after(() => {
  for (const root of FIXTURE_ROOTS)
    rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
})

const GOOGLE_TAG = `export const GOOGLE_TAG_SCRIPT = 'https://www.googletagmanager.com/gtag/js'\n\nexport function loadGoogleTag(id: string): boolean {\n  return id !== ''\n}\n`
const SENTRY = `export function scrub(input: string): string {\n  return input\n}\n`
const PRICING = `export const CLOUD_PRICE_USD_PER_MONTH = 7\n`

/**
 * A minimal tree that passes once synced: the package's source, one consumer
 * file in each consumer root that imports rather than re-declares, and the
 * generated vendor copy. Each test mutates exactly one thing.
 */
function fixtureRoot(overrides = {}, { sync = true } = {}) {
  const root = mkdtempSync(join(tmpdir(), 'web-helpers-sync-'))
  FIXTURE_ROOTS.push(root)
  const files = {
    [`${SOURCE_DIR}/googleTag.ts`]: GOOGLE_TAG,
    [`${SOURCE_DIR}/sentry-init.ts`]: SENTRY,
    [`${SOURCE_DIR}/pricing.ts`]: PRICING,
    'services/web/src/app/main.tsx':
      "import { loadGoogleTag } from '@bossanova/web-helpers/googleTag'\n",
    'services/marketing/src/lib/appCta.ts':
      "import { loadGoogleTag } from '@bossanova/web-helpers/googleTag'\nexport const APP = 1\n",
    'services/docs/src/theme/Root.tsx':
      "import { loadGoogleTag } from '../vendor/web-helpers/googleTag'\n",
  }
  write(root, files)
  if (sync) assert.equal(syncDocsWebHelpers({ root, write: true }).ok, true)
  write(root, overrides)
  return root
}

function write(root, files) {
  for (const [rel, body] of Object.entries(files)) {
    const full = join(root, rel)
    if (body === null) {
      rmSync(full, { force: true })
      continue
    }
    mkdirSync(dirname(full), { recursive: true })
    writeFileSync(full, body)
  }
}

const check = (root) => syncDocsWebHelpers({ root })
const failures = (root) => check(root).failures.join('\n')

test('the real repository is in sync and re-declares no shared export', () => {
  const result = check(undefined)
  assert.equal(result.ok, true, result.failures.join('\n'))
  // The public mirror strips lib/web-helpers and services/web, leaving nothing to compare.
  if (result.skipped) return
  assert.equal(result.vendored, VENDORED_MODULES.length)
  assert.ok(result.exports > 0, 'the package must export something for the scan to mean anything')
  assert.ok(result.scanned > 0, 'the consumer scan must read files')
})

test('the fixture tree passes once synced, so every red result below is the mutation', () => {
  const result = check(fixtureRoot())
  assert.equal(result.ok, true, result.failures.join('\n'))
  assert.deepEqual(result.written, [])
})

test('a vendored file is the fixed header followed by the source bytes', () => {
  const root = fixtureRoot()
  for (const name of VENDORED_MODULES) {
    assert.equal(
      readFileSync(join(root, VENDOR_DIR, name), 'utf8'),
      generatedHeader(name) + readFileSync(join(root, SOURCE_DIR, name), 'utf8'),
    )
  }
  // Only the modules docs imports are vendored.
  assert.deepEqual(readdirSync(join(root, VENDOR_DIR)).sort(), [...VENDORED_MODULES].sort())
})

test('the check fails on a hand-edited vendored docs file', () => {
  const root = fixtureRoot()
  const target = join(root, VENDOR_DIR, 'googleTag.ts')
  writeFileSync(target, readFileSync(target, 'utf8').replace("id !== ''", 'true'))
  assert.match(
    failures(root),
    /vendor\/web-helpers\/googleTag\.ts: differs from lib\/web-helpers\/src\/googleTag\.ts; edit the source, never the copy/,
  )
})

test('a one-byte difference is drift too', () => {
  const root = fixtureRoot()
  const target = join(root, VENDOR_DIR, 'sentry-init.ts')
  writeFileSync(target, `${readFileSync(target, 'utf8')}\n`)
  assert.match(failures(root), /vendor\/web-helpers\/sentry-init\.ts: differs/)
})

test('a source edit nobody re-synced is drift', () => {
  const root = fixtureRoot({
    [`${SOURCE_DIR}/sentry-init.ts`]: `${SENTRY}export const SCRUB_VERSION = 2\n`,
  })
  assert.match(failures(root), /vendor\/web-helpers\/sentry-init\.ts: differs/)
})

test('the check fails on a missing vendored file', () => {
  const root = fixtureRoot({ [`${VENDOR_DIR}/sentry-init.ts`]: null })
  assert.match(failures(root), /vendor\/web-helpers\/sentry-init\.ts: missing; run/)
})

test('the check fails on an extra file in the vendor directory', () => {
  const root = fixtureRoot({ [`${VENDOR_DIR}/pricing.ts`]: PRICING })
  assert.match(
    failures(root),
    /vendor\/web-helpers\/pricing\.ts: not written by the generator; the vendor directory holds only googleTag\.ts, sentry-init\.ts/,
  )
})

test('the check fails on a nested extra file in the vendor directory', () => {
  const root = fixtureRoot({ [`${VENDOR_DIR}/old/googleTag.ts`]: GOOGLE_TAG })
  assert.match(
    failures(root),
    /vendor\/web-helpers\/old\/googleTag\.ts: not written by the generator/,
  )
})

for (const consumer of CONSUMER_ROOTS) {
  test(`the check fails on a re-declared export function loadGoogleTag under ${consumer}`, () => {
    const root = fixtureRoot({
      [`${consumer}/analytics/gtag.ts`]:
        "export function loadGoogleTag(id: string): boolean {\n  return id === 'x'\n}\n",
    })
    assert.match(
      failures(root),
      new RegExp(
        `${consumer.replaceAll('/', '\\/')}\\/analytics\\/gtag\\.ts:1: declares loadGoogleTag, which lib\\/web-helpers\\/src\\/googleTag\\.ts exports`,
      ),
    )
  })
}

test('an unexported second implementation of a shared helper is caught too', () => {
  const root = fixtureRoot({
    'services/web/src/lib/scrubber.ts': 'const scrub = (s: string) => s\nexport default scrub\n',
  })
  assert.match(failures(root), /services\/web\/src\/lib\/scrubber\.ts:1: declares scrub/)
})

test('the vendor directory itself is not a second implementation', () => {
  const root = fixtureRoot()
  const result = check(root)
  assert.equal(result.ok, true, result.failures.join('\n'))
})

test('importing, re-exporting or naming a helper in a nested scope is not a re-declaration', () => {
  const root = fixtureRoot({
    'services/web/src/a.ts':
      "export { loadGoogleTag } from '@bossanova/web-helpers/googleTag'\nfunction wrap() {\n  const scrub = 1\n  return scrub\n}\nexport const used = wrap\n",
  })
  const result = check(root)
  assert.equal(result.ok, true, result.failures.join('\n'))
})

test('generation is idempotent and removes files it does not own', () => {
  const root = fixtureRoot({ [`${VENDOR_DIR}/stale.ts`]: '// stale\n' })
  const first = syncDocsWebHelpers({ root, write: true })
  assert.equal(first.ok, true, first.failures.join('\n'))
  assert.deepEqual(first.written, [`${VENDOR_DIR}/stale.ts (removed)`])
  const before = VENDORED_MODULES.map((name) => readFileSync(join(root, VENDOR_DIR, name), 'utf8'))
  const second = syncDocsWebHelpers({ root, write: true })
  assert.deepEqual(second.written, [])
  assert.deepEqual(
    VENDORED_MODULES.map((name) => readFileSync(join(root, VENDOR_DIR, name), 'utf8')),
    before,
  )
})

test('generation from nothing writes every vendored module', () => {
  const root = fixtureRoot({}, { sync: false })
  const result = syncDocsWebHelpers({ root, write: true })
  assert.equal(result.ok, true, result.failures.join('\n'))
  assert.deepEqual(
    result.written.sort(),
    VENDORED_MODULES.map((name) => `${VENDOR_DIR}/${name}`).sort(),
  )
})

test('the public projection, with the package and the apps stripped, passes and says so', () => {
  const root = mkdtempSync(join(tmpdir(), 'web-helpers-sync-public-'))
  FIXTURE_ROOTS.push(root)
  write(root, { [`${VENDOR_DIR}/googleTag.ts`]: GOOGLE_TAG })
  const result = check(root)
  assert.equal(result.ok, true)
  assert.match(result.skipped, /public projection/)
})

test('a missing package source beside the web app fails rather than skipping', () => {
  const root = fixtureRoot({
    [`${SOURCE_DIR}/googleTag.ts`]: null,
    [`${SOURCE_DIR}/sentry-init.ts`]: null,
    [`${SOURCE_DIR}/pricing.ts`]: null,
  })
  rmSync(join(root, SOURCE_DIR), { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  assert.match(failures(root), /lib\/web-helpers\/src: missing/)
})

test('exportedNames and topLevelDeclarations read only top-level declarations', () => {
  const text = [
    'export const A = 1',
    'export function b() {}',
    'export async function c() {}',
    'export class D {}',
    'export let e = 1',
    'export type F = string',
    'export interface G {}',
    'const h = 1',
    'function i() {}',
    '  const nested = 1',
    "export { j } from './j'",
  ].join('\n')
  assert.deepEqual(exportedNames(text), ['A', 'b', 'c', 'D', 'e'])
  assert.deepEqual(
    topLevelDeclarations(text).map((d) => d.name),
    ['A', 'b', 'c', 'D', 'e', 'h', 'i'],
  )
  assert.equal(topLevelDeclarations(text).find((d) => d.name === 'h').line, 8)
})

// The acceptance criterion is the EXIT CODE `make -C scripts lint` reads, which
// the return value alone cannot prove.
const run = (...args) => spawnSync(process.execPath, [SCRIPT, ...args], { encoding: 'utf8' })

test('--check exits 1 on drift and 0 on a synced tree', () => {
  const clean = run('--check', '--root', fixtureRoot())
  assert.equal(clean.status, 0, clean.stderr)
  assert.match(
    clean.stdout,
    /web-helpers OK \(2 vendored docs file\(s\) fresh; 0 of \d+ shared exports/,
  )

  const drifted = run('--check', '--root', fixtureRoot({ [`${VENDOR_DIR}/googleTag.ts`]: null }))
  assert.equal(drifted.status, 1, drifted.stdout)
  assert.match(drifted.stderr, /googleTag\.ts: missing/)
})

test('writing without --check exits 1 while a re-declaration remains', () => {
  const root = fixtureRoot({ 'services/docs/src/googleTag.ts': GOOGLE_TAG })
  const result = run('--root', root)
  assert.equal(result.status, 1, result.stdout)
  assert.match(result.stderr, /services\/docs\/src\/googleTag\.ts:3: declares loadGoogleTag/)
  assert.match(result.stderr, /import it from the vendored copy/)
})

test('an unknown flag is rejected rather than ignored', () => {
  const result = run('--chek')
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /Unknown option '--chek'/)
})
