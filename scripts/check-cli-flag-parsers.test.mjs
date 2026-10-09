import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import { checkCliFlagParsers, classifySource, exemptionError } from './check-cli-flag-parsers.mjs'

const HAND_ROLLED = `const args = process.argv.slice(2)
for (let i = 0; i < args.length; i += 1) {
  if (args[i] === '--triggers') triggers = args[++i]
}
`
const STRICT_PARSE = `import { parseArgs } from 'node:util'
const { values } = parseArgs({ options: { triggers: { type: 'string' } } })
`

function fixture(files, baseline = []) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'cli-flag-parsers-'))
  for (const [file, source] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), source)
  }
  fs.mkdirSync(path.join(root, 'scripts'), { recursive: true })
  fs.writeFileSync(
    path.join(root, 'scripts', 'cli-flag-parsers-baseline.json'),
    JSON.stringify(baseline),
  )
  return root
}

test('classifySource flags every hand-rolled comparison shape and strict:false', () => {
  for (const source of [
    "if (arg === '--json') json = true",
    "if ('--json' === arg) json = true",
    "switch (arg) { case '--pr': break }",
    "if (args.includes('--dry-run')) dry = true",
    "if (arg.startsWith('--')) flags[arg.slice(2)] = next",
    'parseArgs({ options, strict: false })',
  ]) {
    assert.ok(classifySource(source), source)
  }
  assert.equal(classifySource(STRICT_PARSE), null)
  assert.equal(classifySource("spawnSync('git', ['log', '--format=%H'])"), null)
})

test('a new hand-rolled parser fails and names parseArgs as the fix', () => {
  const root = fixture({ 'skills-toolbox/new-cli.mjs': HAND_ROLLED })
  const { failures } = checkCliFlagParsers({ repoRoot: root, today: '2026-10-08' })
  assert.equal(failures.length, 1)
  assert.match(failures[0], /^skills-toolbox\/new-cli\.mjs: /)
  assert.match(failures[0], /parseArgs` from 'node:util'/)
})

test('a strict parseArgs CLI and a baselined parser both pass', () => {
  const root = fixture({ 'scripts/strict.mjs': STRICT_PARSE, 'scripts/legacy.mjs': HAND_ROLLED }, [
    'scripts/legacy.mjs',
  ])
  assert.deepEqual(checkCliFlagParsers({ repoRoot: root, today: '2026-10-08' }).failures, [])
})

test('a stale baseline entry fails so the baseline only shrinks', () => {
  const root = fixture({ 'scripts/migrated.mjs': STRICT_PARSE }, ['scripts/migrated.mjs'])
  const { failures } = checkCliFlagParsers({ repoRoot: root, today: '2026-10-08' })
  assert.equal(failures.length, 1)
  assert.match(failures[0], /scripts\/migrated\.mjs no longer hand-rolls/)
})

test('test files are not scanned', () => {
  const root = fixture({ 'scripts/thing.test.mjs': HAND_ROLLED })
  assert.deepEqual(checkCliFlagParsers({ repoRoot: root, today: '2026-10-08' }).failures, [])
})

test('an exemption needs a reason, an unexpired date, and an approver', () => {
  const valid = '// flag-parser-exempt: parses shell text; expires 2027-01-01; approved-by dave\n'
  assert.equal(exemptionError(valid, '2026-10-08'), null)
  assert.match(exemptionError(valid, '2027-01-02'), /expired on 2027-01-01/)
  assert.match(exemptionError('// flag-parser-exempt: because\n', '2026-10-08'), /must read/)

  const root = fixture({ 'scripts/shell-analyser.mjs': valid + HAND_ROLLED })
  assert.deepEqual(checkCliFlagParsers({ repoRoot: root, today: '2026-10-08' }).failures, [])
  const expired = checkCliFlagParsers({ repoRoot: root, today: '2027-06-01' }).failures
  assert.equal(expired.length, 1)
  assert.match(expired[0], /^scripts\/shell-analyser\.mjs: flag-parser-exempt expired/)
})
