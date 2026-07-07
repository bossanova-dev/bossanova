import assert from 'node:assert/strict'
import { chmodSync, existsSync, mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const cli = fileURLToPath(new URL('./account-rotation-smoke.mjs', import.meta.url))

function makeFakeBoss({ lsExit = 0 } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'account-rotation-smoke-'))
  const log = join(dir, 'calls.log')
  const fake = join(dir, 'fake-boss.mjs')
  writeFileSync(
    fake,
    `#!/usr/bin/env node
import { appendFileSync } from 'node:fs'
const args = process.argv.slice(2)
appendFileSync(${JSON.stringify(log)}, args.join(' ') + '\\n')
if (args.join(' ') === 'account ls --json') {
  if (${lsExit} !== 0) {
    console.error('daemon unavailable')
    process.exit(${lsExit})
  }
  console.log(JSON.stringify([
    {
      id: 'acct-claude-1',
      provider: 'claude',
      label: 'personal-claude',
      email: 'secret@example.com',
      status: 'active',
      priority: 1,
      health: 'ok',
      cooldown_until: '',
      credential: 'sk-secret'
    },
    {
      id: 'acct-codex-1',
      provider: 'codex',
      label: 'personal-codex',
      email: 'secret2@example.com',
      status: 'active',
      priority: 2,
      health: 'unknown',
      cooldown_until: '2026-07-06T10:00:00Z'
    }
  ]))
  process.exit(0)
}
if (args[0] === 'account' && args[1] === 'test' && args[3] === '--json') {
  const id = args[2]
  console.log(JSON.stringify({
    account: {
      id,
      provider: id.includes('claude') ? 'claude' : 'codex',
      email: 'test-secret@example.com',
      status: id.includes('codex') ? 'disabled' : 'active',
      priority: 1,
      health: id.includes('codex') ? 'failed' : 'ok',
      last_test_error: id.includes('codex') ? 'credential expired' : ''
    },
    live_smoke_ran: true,
    detail: 'sensitive provider detail'
  }))
  process.exit(id.includes('codex') ? 1 : 0)
}
console.error('unexpected args: ' + args.join(' '))
process.exit(2)
`,
  )
  chmodSync(fake, 0o755)
  return { fake, log }
}

test('--help documents no-secret and no-rotation scope', () => {
  const out = execFileSync('node', [cli, '--help'], { encoding: 'utf8' })
  assert.match(out, /prints no secrets/)
  assert.match(out, /does not trigger rotation/)
})

test('--dry-run invokes no boss subprocess', () => {
  const { fake, log } = makeFakeBoss()
  const out = execFileSync('node', [cli, '--dry-run'], {
    encoding: 'utf8',
    env: { ...process.env, BOSS_BIN: fake },
  })
  assert.match(out, /would run: .*account ls --json/)
  assert.equal(existsSync(log), false)
})

test('registry run prints counts and non-secret account fields only', () => {
  const { fake } = makeFakeBoss()
  const out = execFileSync('node', [cli], {
    encoding: 'utf8',
    env: { ...process.env, BOSS_BIN: fake },
  })
  assert.match(out, /claude: 1/)
  assert.match(out, /codex: 1/)
  assert.match(out, /acct-claude-1/)
  assert.match(out, /provider=claude/)
  assert.match(out, /status=active/)
  assert.match(out, /priority=1/)
  assert.match(out, /health=ok/)
  assert.match(out, /cooldown_until=2026-07-06T10:00:00Z/)
  assert.doesNotMatch(out, /secret@example.com/)
  assert.doesNotMatch(out, /personal-claude/)
  assert.doesNotMatch(out, /sk-secret/)
})

test('registry not reachable exits non-zero with daemon hint', () => {
  const { fake } = makeFakeBoss({ lsExit: 7 })
  const result = spawnSync('node', [cli], {
    encoding: 'utf8',
    env: { ...process.env, BOSS_BIN: fake },
  })
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /registry not reachable/)
  assert.match(result.stderr, /is the daemon running/)
})

test('--test exits non-zero on errored account without printing details by default', () => {
  const { fake } = makeFakeBoss()
  const result = spawnSync('node', [cli, '--test'], {
    encoding: 'utf8',
    env: { ...process.env, BOSS_BIN: fake },
  })
  assert.notEqual(result.status, 0)
  assert.match(result.stdout, /acct-claude-1: ok live_smoke_ran=true/)
  assert.match(result.stdout, /acct-codex-1: error live_smoke_ran=true/)
  assert.doesNotMatch(result.stdout, /test-secret@example.com/)
  assert.doesNotMatch(result.stdout, /sensitive provider detail/)
})

// A stale `failed` account can pass a fresh credential test (exit 0, cleared
// last_test_error) yet still be non-rotatable. --test must report it as error.
function makeFailedHealthBoss() {
  const dir = mkdtempSync(join(tmpdir(), 'account-rotation-smoke-failed-'))
  const fake = join(dir, 'fake-boss.mjs')
  writeFileSync(
    fake,
    `#!/usr/bin/env node
const args = process.argv.slice(2)
if (args.join(' ') === 'account ls --json') {
  console.log(JSON.stringify([
    { id: 'acct-stale-1', provider: 'claude', label: 'l', email: 'e', status: 'active', priority: 1, health: 'failed', cooldown_until: '' }
  ]))
  process.exit(0)
}
if (args[0] === 'account' && args[1] === 'test' && args[3] === '--json') {
  console.log(JSON.stringify({
    account: { id: args[2], provider: 'claude', email: 'e', status: 'active', priority: 1, health: 'failed', last_test_error: '' },
    live_smoke_ran: true,
    detail: ''
  }))
  process.exit(0)
}
process.exit(2)
`,
  )
  chmodSync(fake, 0o755)
  return { fake }
}

test('--test reports error for a failed-health account that passes a fresh test', () => {
  const { fake } = makeFailedHealthBoss()
  const result = spawnSync('node', [cli, '--test'], {
    encoding: 'utf8',
    env: { ...process.env, BOSS_BIN: fake },
  })
  assert.notEqual(result.status, 0)
  assert.match(result.stdout, /acct-stale-1: error live_smoke_ran=true/)
})

// The smoke's `ok` must require full rotation eligibility (active, healthy, not
// cooling), matching the rotation selector — a passing credential test alone
// does not make a disabled or cooling account rotatable. `testAccount` is the
// account object the fake returns from `boss account test --json`.
function makeEligibilityBoss(id, testAccount) {
  const dir = mkdtempSync(join(tmpdir(), 'account-rotation-smoke-elig-'))
  const fake = join(dir, 'fake-boss.mjs')
  writeFileSync(
    fake,
    `#!/usr/bin/env node
const args = process.argv.slice(2)
if (args.join(' ') === 'account ls --json') {
  console.log(JSON.stringify([${JSON.stringify({ id, provider: 'claude', label: 'l', email: 'e', status: 'active', priority: 1, health: 'ok', cooldown_until: '' })}]))
  process.exit(0)
}
if (args[0] === 'account' && args[1] === 'test' && args[3] === '--json') {
  console.log(JSON.stringify({ account: ${JSON.stringify(testAccount)}, live_smoke_ran: true, detail: '' }))
  process.exit(0)
}
process.exit(2)
`,
  )
  chmodSync(fake, 0o755)
  return { fake }
}

function runTestMode(fake) {
  return spawnSync('node', [cli, '--test'], {
    encoding: 'utf8',
    env: { ...process.env, BOSS_BIN: fake },
  })
}

test('--test reports error for a disabled account that passes a fresh test', () => {
  const { fake } = makeEligibilityBoss('acct-disabled-1', {
    id: 'acct-disabled-1',
    provider: 'claude',
    email: 'e',
    status: 'disabled',
    priority: 1,
    health: 'ok',
    cooldown_until: '',
    last_test_error: '',
  })
  const result = runTestMode(fake)
  assert.notEqual(result.status, 0)
  assert.match(result.stdout, /acct-disabled-1: error live_smoke_ran=true/)
})

test('--test reports error for a cooling account that passes a fresh test', () => {
  const { fake } = makeEligibilityBoss('acct-cooling-1', {
    id: 'acct-cooling-1',
    provider: 'claude',
    email: 'e',
    status: 'active',
    priority: 1,
    health: 'ok',
    cooldown_until: '2999-01-01T00:00:00Z',
    last_test_error: '',
  })
  const result = runTestMode(fake)
  assert.notEqual(result.status, 0)
  assert.match(result.stdout, /acct-cooling-1: error live_smoke_ran=true/)
})

test('--test reports ok when an expired cooldown no longer blocks rotation', () => {
  const { fake } = makeEligibilityBoss('acct-expired-1', {
    id: 'acct-expired-1',
    provider: 'claude',
    email: 'e',
    status: 'active',
    priority: 1,
    health: 'ok',
    cooldown_until: '2000-01-01T00:00:00Z',
    last_test_error: '',
  })
  const result = runTestMode(fake)
  assert.equal(result.status, 0)
  assert.match(result.stdout, /acct-expired-1: ok live_smoke_ran=true/)
})
