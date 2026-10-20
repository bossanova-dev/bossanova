import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { recheckPlan } from './plan-recheck.mjs'
import { DEFAULT_CONFIG } from './skill-config.mjs'

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'plan-recheck-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  execFileSync('git', ['init', '-q', root])
  writeFileSync(
    join(root, 'old.mjs'),
    'export const currentSymbol = 1\n' + '\n'.repeat(12) + 'export const movedSymbol = 2\n',
  )
  writeFileSync(join(root, 'new.mjs'), 'export const relocatedSymbol = 3\n')
  execFileSync('git', ['add', '.'], { cwd: root })
  return { root, check: (text) => recheckPlan(DEFAULT_CONFIG, text, { root }) }
}

test('resolves anchored and unanchored coordinates, relocation, missing paths and fences', (t) => {
  const { check } = fixture(t)
  const plan = [
    '`old.mjs:1` `currentSymbol`',
    '`old.mjs:1` `movedSymbol`',
    '`old.mjs:1` `relocatedSymbol`',
    '`old.mjs:1` `vanishedSymbol`',
    '`gone.mjs:2` `relocatedSymbol`',
    '`old.mjs:2`',
    '`old.mjs:99`',
    '`../outside.mjs:1`',
    '`nested/new.mjs`',
    '```',
    '`ignored.mjs:4`',
    '```',
    '## Original notes',
    '`history.mjs:1`',
  ].join('\n')
  const result = check(plan)
  assert.deepEqual(
    result.items.filter((i) => i.source === 'citation').map((i) => i.kind),
    [
      'current',
      'moved',
      'relocated',
      'anchor-missing',
      'path-missing',
      'current',
      'short-file',
      'path-missing',
    ],
  )
  assert.equal(result.items.find((i) => i.file === 'old.mjs' && i.line === 2).unanchored, true)
  assert.deepEqual(result.items.find((i) => i.kind === 'moved').locations, [
    { file: 'old.mjs', line: 14 },
  ])
  assert.equal(result.items.find((i) => i.file === '../outside.mjs').reason, 'escapes-root')
  assert.equal(check('`nested/new.mjs`').verdict, 'drifted')
  assert.equal(check('`./old.mjs`').items[0].kind, 'present')
})

test('rechecks searches and counts without executing shell syntax', (t) => {
  const { root, check } = fixture(t)
  const result = check(
    `## Premises\n- [ ] Exists — check: \`grep -F currentSymbol old.mjs\`\n- [ ] Missing — check: \`rg -F absent old.mjs\`\n- [ ] Negated — check: \`! git grep -F currentSymbol\`\n- [ ] Has 9 lines — check: \`wc -l old.mjs\`\n- [ ] Has 999 bytes — check: \`wc -c old.mjs\`\n- [ ] Has 5 matches — check: \`grep -c -F currentSymbol old.mjs\`\n- [ ] Unsafe — check: \`grep -F x old.mjs; touch PWNED\`\n- [ ] Regex — check: \`grep -E x old.mjs\`\n- [ ] Inverted — check: \`grep -v -F currentSymbol old.mjs\``,
  )
  assert.deepEqual(
    result.items.filter((i) => i.source === 'premise').map((i) => i.kind),
    [
      'confirmed',
      'refuted',
      'refuted',
      'count-drift',
      'count-drift',
      'count-drift',
      'not-evaluated',
      'not-evaluated',
      'not-evaluated',
    ],
  )
  assert.equal(result.verdict, 'stale')
  assert.equal(existsSync(join(root, 'PWNED')), false)
})

test('CLI JSON and exit contracts, including help and unreadable inputs', (t) => {
  const { root } = fixture(t)
  const cli = new URL('./plan-recheck.mjs', import.meta.url)
  const run = (args) => spawnSync(process.execPath, [cli.pathname, ...args], { encoding: 'utf8' })
  for (const [text, expected] of [
    ['`old.mjs:1` `currentSymbol`', 0],
    ['`old.mjs:1` `movedSymbol`', 1],
  ]) {
    writeFileSync(join(root, 'plan.md'), text)
    const result = run([join(root, 'plan.md'), '--root', root])
    assert.equal(result.status, expected, result.stderr)
    assert.ok(JSON.parse(result.stdout).checked > 0)
  }
  assert.equal(run(['--help']).status, 0)
  for (const args of [[], ['missing-plan'], ['--bad']]) {
    const result = run(args)
    assert.equal(result.status, 2)
    assert.equal(JSON.parse(result.stdout).verdict, 'unknown')
  }
})

test('confirms measurements and marks unsupported or ambiguous measurements manual', (t) => {
  const { root, check } = fixture(t)
  writeFileSync(join(root, 'bytes.bin'), Buffer.from([255, 0, 10]))
  const result = check(
    '## Premises\n- [ ] Has 3 bytes — check: `wc -c bytes.bin`\n- [ ] Has 14 lines — check: `wc -l < old.mjs`\n- [ ] Has 1 match — check: `rg -c -F currentSymbol old.mjs`\n- [ ] Has between 1 and 5 matches — check: `grep -c -F currentSymbol old.mjs`\n- [ ] Host state — check: `boss cron ls`\n- [ ] Exclude — check: `grep -L -F currentSymbol old.mjs`',
  )
  assert.deepEqual(
    result.items.filter((i) => i.source === 'premise').map((i) => i.kind),
    ['confirmed', 'confirmed', 'confirmed', 'not-evaluated', 'not-evaluated', 'not-evaluated'],
  )
  assert.equal(result.verdict, 'current')
})

test('missing premise paths are stale and non-git relocation remains unknown', (t) => {
  const { root, check } = fixture(t)
  assert.equal(check('## Premises\n- [ ] `missing.mjs:1` `missingSymbol`').verdict, 'stale')
  rmSync(join(root, '.git'), { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  const result = check('`old.mjs:1` `missingSymbol`')
  assert.equal(result.items[0].kind, 'unknown')
  assert.equal(recheckPlan(DEFAULT_CONFIG, '', { root: join(root, 'missing') }).verdict, 'unknown')
})

test('shell expansions remain manual even inside double quotes', (t) => {
  const { check } = fixture(t)
  const result = check('## Premises\n- [ ] Expansion — check: `grep -F "$NEEDLE" old.mjs`')
  assert.equal(result.items.find((i) => i.source === 'premise').kind, 'not-evaluated')
})
