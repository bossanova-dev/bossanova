import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
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
  mkdirSync(join(root, 'docs'))
  writeFileSync(join(root, 'docs', 'guide.md'), '# Guide\n')
  execFileSync('git', ['add', '.'], { cwd: root })
  return { root, check: (text) => recheckPlan(DEFAULT_CONFIG, text, { root }) }
}

test('classifies non-prerequisite paths without reporting drift', (t) => {
  const { check } = fixture(t)
  const cases = [
    ['`docs/**/*.md`', 'pattern', { matches: 1 }],
    ['`docs/g?ide.md`', 'pattern', { matches: 1 }],
    ['`docs/missing*.md`', 'pattern', { matches: 0 }],
    ['`skills-toolbox/plan-{a,b}.mjs`', 'pattern', { matches: null }],
    ['`docs/[a-z].md`', 'pattern', { matches: null }],
    ['`~/.claude/skills/boss-build/SKILL.md`', 'outside-repo', { reason: 'home-relative' }],
    ['`$HOME/notes/today.md`', 'outside-repo', { reason: 'variable' }],
    ['`${BOSS_PLAN_TOOLBOX}/plan-recheck.mjs`', 'outside-repo', { reason: 'variable' }],
    ['`/tmp/plan.md`', 'outside-repo', { reason: 'absolute' }],
    ['`https://example.com/fixture.json`', 'url', {}],
    ['New file `scripts/x/new-helper.mjs`', 'planned', {}],
    ['`lib/y/added.mjs` (new)', 'planned', {}],
    ['`scripts/x/new-helper.mjs` mentioned again', 'planned', {}],
    ['a new `./old.mjs`', 'present', {}],
  ]
  const result = check(cases.map(([line]) => line).join('\n'))
  assert.equal(result.verdict, 'current')
  assert.equal(result.items.length, cases.length)
  cases.forEach(([, kind, details], index) => {
    assert.equal(result.items[index].source, 'path')
    assert.equal(result.items[index].kind, kind, cases[index][0])
    for (const [key, value] of Object.entries(details))
      assert.equal(result.items[index][key], value, cases[index][0])
  })
  const missing = check(cases.map(([line]) => line).join('\n') + '\n`missing/prereq.mjs`')
  assert.equal(missing.items.at(-1).kind, 'absent')
  assert.equal(missing.verdict, 'drifted')
})

test('only adjacent explicit markers declare planned paths across mentions', (t) => {
  const { check } = fixture(t)
  for (const prefix of [
    'new',
    'a new',
    'new file',
    'new module',
    'new test',
    'create',
    'creates',
    'add',
    'adds',
  ]) {
    const result = check('`nested/added.mjs`\n' + prefix.toUpperCase() + ' `./nested/added.mjs`')
    assert.equal(result.verdict, 'current', prefix)
    assert.ok(
      result.items.every((item) => item.kind === 'planned'),
      prefix,
    )
  }
  for (const suffix of ['(new, pure)', ': new module', ': a new helper', '— new test', '- new'])
    assert.equal(check('`nested/added.mjs` ' + suffix).items[0].kind, 'planned', suffix)
  for (const text of [
    'New behavior in `nested/prereq.mjs`',
    'renew `nested/prereq.mjs`',
    '`nested/prereq.mjs`\n## Original notes\nNew file `nested/prereq.mjs`',
    '`nested/prereq.mjs`\n```\nNew file `nested/prereq.mjs`\n```',
  ]) {
    assert.equal(check(text).items[0].kind, 'absent', text)
    assert.equal(check(text).verdict, 'drifted', text)
  }
})

test('glob counts remain unevaluated when tracked files cannot be read', (t) => {
  const { root, check } = fixture(t)
  rmSync(join(root, '.git'), { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  const result = check('`docs/**/*.md`')
  assert.equal(result.verdict, 'current')
  assert.equal(result.items[0].kind, 'pattern')
  assert.equal(result.items[0].matches, null)
})

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

test('exact citations use only their adjacent anchors, including wrapped code spans', (t) => {
  const { root, check } = fixture(t)
  writeFileSync(join(root, 'release.sh'), 'printf "%s" "${NAME:-}"\n')
  const cases = [
    ['- `old.mjs:1` (`"ready"`), plus `new.mjs:1` (`relocatedSymbol`).', [[], ['relocatedSymbol']]],
    [
      '- `${NAME:-}` at `release.sh:1`, plus `new.mjs:1` (`relocatedSymbol`).',
      [[], ['relocatedSymbol']],
    ],
    [
      '- `old.mjs:1` (`call(\n  argument)`) and `new.mjs:1` (`relocatedSymbol`).',
      [[], ['relocatedSymbol']],
    ],
    [
      '- `old.mjs:1` `currentSymbol\n  ` and `new.mjs:1` `relocatedSymbol`.',
      [['currentSymbol'], ['relocatedSymbol']],
    ],
    ['- `old.mjs:1` (`currentSymbol`, `relocatedSymbol`).', [['currentSymbol']]],
    [
      '- `currentSymbol` at `old.mjs:1`, plus `new.mjs:1` is `relocatedSymbol()`.',
      [['currentSymbol'], ['relocatedSymbol']],
    ],
  ]
  for (const [text, anchors] of cases) {
    const items = check(text).items.filter((item) => item.source === 'citation')
    assert.deepEqual(
      items.map((item) => item.kind),
      anchors.map(() => 'current'),
      text,
    )
    assert.deepEqual(
      items.map((item) => item.anchors),
      anchors,
      text,
    )
    assert.ok(items.every((item) => item.text === text))
  }
  assert.deepEqual(check('`Manager.Start:12` beside `currentSymbol`').items, [])
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

test('evaluates size and budget claims without guessing other two-number counts', (t) => {
  const { root, check } = fixture(t)
  writeFileSync(join(root, 'budget.txt'), 'one\ntwo\n')
  const cases = [
    ['Has 2 lines against a 5-line budget', '-l', 'confirmed', 5, 3],
    ['Has 2 lines with a budget of 5', '-l', 'confirmed', 5, 3],
    ['Has 1 line against a 5-line cap', '-l', 'count-drift', 5, 3],
    ['Has 2 lines with a limit of 1', '-l', 'confirmed', 1, -1],
    ['Has 8 bytes against a 10-byte limit', '-c', 'confirmed', 10, 2],
    ['Has 8 bytes with a cap of 10', '-c', 'confirmed', 10, 2],
    ['Has between 2 and 5 lines', '-l', 'not-evaluated'],
    ['Has 2 lines against 5 estimates', '-l', 'not-evaluated'],
    ['Has 2 lines against a 5-line budget and a 6-line cap', '-l', 'not-evaluated'],
  ]
  for (const [claim, mode, kind, budget, headroom] of cases) {
    const result = check(`## Premises\n- [ ] ${claim} — check: \`wc ${mode} budget.txt\``)
    const item = result.items.find((entry) => entry.source === 'premise')
    assert.equal(item.kind, kind, claim)
    assert.equal(item.budget, budget, claim)
    assert.equal(item.headroom, headroom, claim)
  }
  const plan = '## Premises\n- [ ] Has 1 line against a 5-line budget — check: `wc -l budget.txt`'
  assert.equal(check(plan).verdict, 'stale')
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

test('evaluates a count premise followed by trailing prose', (t) => {
  const { check } = fixture(t)
  const result = check(
    "## Premises\n- [ ] Has 1 match — check: `grep -c -F currentSymbol old.mjs` (it's counted)",
  )
  assert.equal(result.items.find((item) => item.source === 'premise').kind, 'confirmed')
  assert.equal(result.verdict, 'current')
})
