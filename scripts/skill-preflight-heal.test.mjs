import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('..', import.meta.url))

// Execute the shipped shell, with only its binary and installed-toolbox dependencies replaced.
function runPreflight(t, core, scenario) {
  const scratch = mkdtempSync(path.join(tmpdir(), 'boss-preflight-heal-'))
  t.after(() => rmSync(scratch, { recursive: true, force: true }))
  const skill = readFileSync(
    path.join(root, 'lib/bossalib/skillpayload/skills', core, 'SKILL.md'),
    'utf8',
  )
  const start = skill.indexOf('if BOSS_BIN=')
  const shell = skill.slice(start).split('\nfi\n')[0] + '\nfi\n'
  const calls = path.join(scratch, 'calls.jsonl')
  writeFileSync(calls, '')
  writeFileSync(
    path.join(scratch, 'boss'),
    String.raw`#!${process.execPath}
import fs from 'node:fs'
const args = process.argv.slice(2)
fs.appendFileSync(process.env.CALLS, JSON.stringify(args) + '\n')
if (process.env.SCENARIO === 'old-gate') {
  console.error('boss: unknown flag: --gate')
  process.exit(1)
}
if (args.includes('--heal') && process.env.SCENARIO !== 'heal') {
  console.error('boss: unknown flag: --heal')
  process.exit(1)
}
if (process.env.SCENARIO === 'absent') {
  console.error('boss skills gate: claude skill drift detected\n  - ${core}/toolbox/missing.mjs (absent, lossless)')
  process.exit(1)
}
`,
    { mode: 0o755 },
  )
  writeFileSync(path.join(scratch, 'package.json'), '{"type":"module"}')
  symlinkSync(
    path.join(root, 'skills-toolbox/skill-drift-verdict.mjs'),
    path.join(scratch, 'skill-drift-verdict.mjs'),
  )
  writeFileSync(
    path.join(scratch, 'toolbox-drift.mjs'),
    "console.error('fallback toolbox probe ran')\n",
  )
  const result = spawnSync('/bin/bash', ['-eu', '-c', shell], {
    encoding: 'utf8',
    cwd: scratch,
    env: {
      ...process.env,
      PATH: `${scratch}:${process.env.PATH}`,
      BOSS_PLAN_TOOLBOX: scratch,
      BOSS_BUILD_TOOLBOX: scratch,
      SCENARIO: scenario,
      CALLS: calls,
    },
  })
  assert.ifError(result.error)
  return {
    ...result,
    calls: readFileSync(calls, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse),
  }
}

for (const core of ['boss-plan', 'boss-build']) {
  test(`${core}: an older binary retries the gate without heal`, (t) => {
    const result = runPreflight(t, core, 'old-heal')
    assert.equal(result.status, 0, result.stderr)
    assert.deepEqual(result.calls, [
      ['skills', 'check', '--gate', '--heal'],
      ['skills', 'check', '--gate'],
    ])
  })

  test(`${core}: a healed gate needs no retry`, (t) => {
    const result = runPreflight(t, core, 'heal')
    assert.equal(result.status, 0, result.stderr)
    // guidance-audit: allow — observed argv from executing the shipped shell, not prose.
    assert.deepEqual(result.calls, [['skills', 'check', '--gate', '--heal']])
  })

  test(`${core}: the compatibility retry still blocks absent capabilities`, (t) => {
    const result = runPreflight(t, core, 'absent')
    assert.equal(result.status, 1, result.stderr)
    // guidance-audit: allow — observed process invocations exercise compatibility retry.
    assert.deepEqual(result.calls, [
      ['skills', 'check', '--gate', '--heal'],
      ['skills', 'check', '--gate'],
    ])
  })

  test(`${core}: a binary without gate still runs the toolbox probe`, (t) => {
    const result = runPreflight(t, core, 'old-gate')
    assert.equal(result.status, 0, result.stderr)
    // guidance-audit: allow — observed argv from executing the shipped shell, not prose.
    assert.deepEqual(result.calls, [['skills', 'check', '--gate', '--heal']])
    // guidance-audit: allow — stderr proves the stub fallback script executed.
    assert.match(result.stderr, /fallback toolbox probe ran/)
  })
}
