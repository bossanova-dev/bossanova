import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { region } from '../test-infra/gate-region-lib.mjs'

const root = fileURLToPath(new URL('../..', import.meta.url))

// Execute the shipped shell, with only its binary and installed-toolbox dependencies replaced.
function runPreflight(t, core, scenario) {
  const scratch = mkdtempSync(path.join(tmpdir(), 'boss-preflight-heal-'))
  t.after(() => rmSync(scratch, { recursive: true, force: true, maxRetries: 3 }))
  const payload = path.join(root, 'lib/bossalib/contract/plugin/skillpayload/skills', core)
  let args
  if (core === 'boss-plan') {
    mkdirSync(path.join(scratch, 'plan/run-steps'), { recursive: true })
    for (const file of ['plan/boss-plan-run.sh', 'plan/run-steps/drift.sh']) {
      copyFileSync(path.join(payload, 'toolbox', file), path.join(scratch, file))
    }
    args = [path.join(scratch, 'plan/boss-plan-run.sh'), 'drift']
  } else {
    const skill = readFileSync(path.join(payload, 'SKILL.md'), 'utf8')
    const shell = region(region(skill, 'if BOSS_BIN='), 'if BOSS_BIN=', '\nfi\n') + '\nfi\n'
    args = ['-eu', '-c', shell]
  }
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
  const result = spawnSync('/bin/bash', args, {
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
