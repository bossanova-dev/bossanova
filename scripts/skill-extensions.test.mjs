import { execFileSync } from 'node:child_process'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import {
  ROLE_SCHEMAS,
  discoverExtensions,
  extensionMarker,
  parseFrontmatter,
  validateResult,
} from './skill-extensions.mjs'

function scratchRoot() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'skill-ext-'))
}

function runCli(args) {
  try {
    const stdout = execFileSync('node', ['scripts/skill-extensions.mjs', ...args], {
      encoding: 'utf8',
      cwd: process.cwd(),
    })
    return { status: 0, stdout }
  } catch (err) {
    return { status: err.status, stdout: err.stdout ?? '', stderr: err.stderr ?? '' }
  }
}

function writeSkill(root, name, frontmatterLines) {
  const dir = path.join(root, '.claude', 'skills', name)
  fs.mkdirSync(dir, { recursive: true })
  const fm = ['---', ...frontmatterLines, '---', '', `# ${name}`, ''].join('\n')
  fs.writeFileSync(path.join(dir, 'SKILL.md'), fm)
  return dir
}

test('parseFrontmatter splits frontmatter from body', () => {
  const { data, body } = parseFrontmatter('---\nname: x\n---\n\nhello\n')
  assert.equal(data.name, 'x')
  assert.equal(body.trim(), 'hello')
})

test('extensionMarker reads the x-boss-extension block', () => {
  const { data } = parseFrontmatter(
    '---\nname: bs-review-security\nx-boss-extension:\n  extends: bs-review\n  role: lens\n  order: 20\n---\n',
  )
  assert.deepEqual(extensionMarker(data), { extends: 'bs-review', role: 'lens', order: 20 })
})

test('extensionMarker returns null when the block is absent', () => {
  const { data } = parseFrontmatter('---\nname: bs-review\n---\n')
  assert.equal(extensionMarker(data), null)
})

test('extensionMarker defaults order to 100 when the marker omits it', () => {
  const { data } = parseFrontmatter(
    '---\nname: bs-review-x\nx-boss-extension:\n  extends: bs-review\n  role: lens\n---\n',
  )
  assert.deepEqual(extensionMarker(data), { extends: 'bs-review', role: 'lens', order: 100 })
})

test('discoverExtensions finds prefixed skills, orders by (order, name), filters non-matching', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-review', ['name: bs-review']) // the core skill itself — no marker, excluded
  writeSkill(root, 'bs-review-zeta', [
    'name: bs-review-zeta',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lens',
    '  order: 50',
  ])
  writeSkill(root, 'bs-review-alpha', [
    'name: bs-review-alpha',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lens',
    '  order: 50',
  ])
  writeSkill(root, 'bs-review-early', [
    'name: bs-review-early',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lens',
    '  order: 10',
  ])
  writeSkill(root, 'bs-review-foreign', [
    'name: bs-review-foreign',
    'x-boss-extension:',
    '  extends: bs-plan', // matches the name prefix but declares a different core
    '  role: plan-reviewer',
  ])
  const { extensions, skipped } = discoverExtensions({ core: 'bs-review', root })
  assert.deepEqual(
    extensions.map((e) => e.name),
    ['bs-review-early', 'bs-review-alpha', 'bs-review-zeta'],
  )
  assert.ok(skipped.some((s) => s.name === 'bs-review-foreign' && /extends/.test(s.reason)))
})

test('discoverExtensions omits known cross-role siblings when role is supplied', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-plan-house-style', [
    'name: bs-plan-house-style',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: plan-reviewer',
  ])
  writeSkill(root, 'bs-plan-mislabeled', [
    'name: bs-plan-mislabeled',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: lens', // extends bs-plan but declares the wrong role
  ])
  const { extensions, skipped } = discoverExtensions({
    core: 'bs-plan',
    root,
    role: 'plan-reviewer',
  })
  assert.deepEqual(
    extensions.map((e) => e.name),
    ['bs-plan-house-style'],
  )
  assert.deepEqual(skipped, [])
})

test('discoverExtensions rejects an unknown wrong-role extension into skipped', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-plan-house-style', [
    'name: bs-plan-house-style',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: plan-reviewer',
  ])
  writeSkill(root, 'bs-plan-typo', [
    'name: bs-plan-typo',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: plan-reviwer',
  ])
  const { extensions, skipped } = discoverExtensions({
    core: 'bs-plan',
    root,
    role: 'plan-reviewer',
  })
  assert.deepEqual(
    extensions.map((e) => e.name),
    ['bs-plan-house-style'],
  )
  assert.ok(skipped.some((s) => s.name === 'bs-plan-typo' && /role/.test(s.reason)))
})

test('discoverExtensions reports an unknown requested role as a skip', () => {
  const root = scratchRoot()
  writeSkill(root, 'boss-proof-marketing', [
    'name: boss-proof-marketing',
    'x-boss-extension:',
    '  extends: boss-proof',
    '  role: surface',
  ])
  const { extensions, skipped } = discoverExtensions({
    core: 'boss-proof',
    root,
    role: 'surfcae',
  })
  assert.deepEqual(extensions, [])
  assert.ok(
    skipped.some(
      (s) => s.name === 'boss-proof-marketing' && /unknown requested role "surfcae"/.test(s.reason),
    ),
    `expected unknown requested role skip, got ${JSON.stringify(skipped)}`,
  )
})

test('discoverExtensions ignores boss-plan draft sibling during plan-review discovery', () => {
  const root = scratchRoot()
  writeSkill(root, 'boss-plan-draft', [
    'name: boss-plan-draft',
    'x-boss-extension:',
    '  extends: boss-plan',
    '  role: draft',
  ])
  const { extensions, skipped } = discoverExtensions({
    core: 'boss-plan',
    root,
    role: 'plan-reviewer',
  })
  assert.deepEqual(extensions, [])
  assert.deepEqual(skipped, [])
})

test('discoverExtensions returns every marker-matched role when role is omitted', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-plan-reviewer', [
    'name: bs-plan-reviewer',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: plan-reviewer',
  ])
  writeSkill(root, 'bs-plan-lensy', [
    'name: bs-plan-lensy',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: lens',
  ])
  const { extensions } = discoverExtensions({ core: 'bs-plan', root })
  assert.deepEqual(extensions.map((e) => e.name).sort(), ['bs-plan-lensy', 'bs-plan-reviewer'])
})

test('discoverExtensions is a no-op when the skills dir is absent', () => {
  const root = scratchRoot()
  const { extensions } = discoverExtensions({ core: 'bs-review', root })
  assert.deepEqual(extensions, [])
})

test('discoverExtensions skips a malformed manifest without throwing', () => {
  const root = scratchRoot()
  const dir = path.join(root, '.claude', 'skills', 'bs-review-broken')
  fs.mkdirSync(dir, { recursive: true })
  fs.writeFileSync(path.join(dir, 'SKILL.md'), 'no frontmatter here\n')
  const { extensions, skipped } = discoverExtensions({ core: 'bs-review', root })
  assert.deepEqual(extensions, [])
  assert.ok(skipped.some((s) => s.name === 'bs-review-broken'))
})

test('validateResult accepts a well-formed lens envelope', () => {
  const envelope = {
    ok: true,
    extension: 'bs-review-security',
    role: 'lens',
    items: [{ severity: 'Warning', file: 'a.go', line: 3, title: 't', detail: 'd' }],
  }
  assert.deepEqual(validateResult(envelope, 'lens'), { ok: true, errors: [] })
})

test('validateResult accepts a well-formed round envelope', () => {
  const envelope = {
    ok: true,
    extension: 'boss-review-requesting',
    role: 'round',
    items: [{ severity: 'Warning', file: 'a.go', line: 3, title: 't', detail: 'd' }],
  }
  assert.deepEqual(validateResult(envelope, 'round'), { ok: true, errors: [] })
})

test('validateResult rejects a round envelope missing a findings key', () => {
  const envelope = {
    ok: true,
    extension: 'boss-review-requesting',
    role: 'round',
    items: [{ severity: 'Warning', file: 'a.go', line: 3, title: 't' }],
  }
  const result = validateResult(envelope, 'round')
  assert.equal(result.ok, false)
  assert.ok(result.errors.some((e) => /missing "detail"/.test(e)))
})

test('validateResult accepts a well-formed surface envelope', () => {
  const envelope = {
    ok: true,
    extension: 'bs-proof-x',
    role: 'surface',
    items: [{ path: 'shot.png', caption: 'c', evidenceTokens: ['t'] }],
  }
  assert.deepEqual(validateResult(envelope, 'surface'), { ok: true, errors: [] })
})

test('validateResult rejects an envelope missing the ok / extension fields', () => {
  const result = validateResult({ role: 'lens', items: [] }, 'lens')
  assert.equal(result.ok, false)
  assert.ok(result.errors.some((e) => /ok is not a boolean/.test(e)))
  assert.ok(result.errors.some((e) => /extension is not a non-empty string/.test(e)))
})

test('validateResult rejects a handled-failure envelope (ok:false) and surfaces its error', () => {
  const envelope = {
    ok: false,
    extension: 'bs-review-security',
    role: 'lens',
    items: [],
    error: 'subagent timed out',
  }
  const result = validateResult(envelope, 'lens')
  assert.equal(result.ok, false)
  assert.ok(result.errors.some((e) => /ok:false/.test(e) && /subagent timed out/.test(e)))
})

test('validateResult rejects ok:false without an error field, with a fallback reason', () => {
  const envelope = { ok: false, extension: 'x', role: 'lens', items: [] }
  const result = validateResult(envelope, 'lens')
  assert.equal(result.ok, false)
  assert.ok(result.errors.some((e) => /ok:false/.test(e) && /no error detail/.test(e)))
})

test('validateResult rejects a role mismatch', () => {
  const envelope = { ok: true, extension: 'x', role: 'surface', items: [] }
  const result = validateResult(envelope, 'lens')
  assert.equal(result.ok, false)
  assert.ok(result.errors.some((e) => /role/.test(e)))
})

test('validateResult rejects items missing required keys', () => {
  const envelope = {
    ok: true,
    extension: 'x',
    role: 'plan-reviewer',
    items: [{ severity: 'Warning', title: 't' }], // missing section + detail
  }
  const result = validateResult(envelope, 'plan-reviewer')
  assert.equal(result.ok, false)
  assert.ok(result.errors.length >= 1)
})

test('validateResult never throws on a non-object envelope', () => {
  const result = validateResult(null, 'lens')
  assert.equal(result.ok, false)
})

test('ROLE_SCHEMAS enumerates the consumer roles', () => {
  assert.deepEqual(Object.keys(ROLE_SCHEMAS).sort(), ['lens', 'plan-reviewer', 'round', 'surface'])
  assert.deepEqual(ROLE_SCHEMAS.round, ROLE_SCHEMAS.lens)
})

test('extension contract documents draft as a plan-writing exception', () => {
  const contract = fs.readFileSync('docs/skills/extension-contract.md', 'utf8')
  assert.match(contract, /`draft` → `\{ mode, planPath, ticket, designDoc\? \}`/)
  assert.match(contract, /`draft` is the behavior-writing exception/)
  assert.match(contract, /does not use `ROLE_SCHEMAS`, `items\[\]`, or `validate --role draft`/)
  assert.match(contract, /may write only `context\.planPath` and `outPath`/)
})

test('CLI discover --json prints extensions and exits 0 with none', () => {
  const root = scratchRoot()
  const out = execFileSync(
    'node',
    ['scripts/skill-extensions.mjs', 'discover', '--core', 'bs-review', '--root', root, '--json'],
    { encoding: 'utf8', cwd: process.cwd() },
  )
  const parsed = JSON.parse(out)
  assert.deepEqual(parsed.extensions, [])
})

test('CLI discover --json lists a discovered extension', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-plan-house-style', [
    'name: bs-plan-house-style',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: plan-reviewer',
    '  order: 30',
  ])
  const out = execFileSync(
    'node',
    ['scripts/skill-extensions.mjs', 'discover', '--core', 'bs-plan', '--root', root, '--json'],
    { encoding: 'utf8', cwd: process.cwd() },
  )
  const parsed = JSON.parse(out)
  assert.equal(parsed.extensions.length, 1)
  assert.equal(parsed.extensions[0].name, 'bs-plan-house-style')
  assert.equal(parsed.extensions[0].role, 'plan-reviewer')
})

test('CLI discover --role omits known cross-role siblings', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-plan-house-style', [
    'name: bs-plan-house-style',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: plan-reviewer',
  ])
  writeSkill(root, 'bs-plan-mislabeled', [
    'name: bs-plan-mislabeled',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: lens',
  ])
  const out = execFileSync(
    'node',
    [
      'scripts/skill-extensions.mjs',
      'discover',
      '--core',
      'bs-plan',
      '--role',
      'plan-reviewer',
      '--root',
      root,
      '--json',
    ],
    { encoding: 'utf8', cwd: process.cwd() },
  )
  const parsed = JSON.parse(out)
  assert.deepEqual(
    parsed.extensions.map((e) => e.name),
    ['bs-plan-house-style'],
  )
  assert.deepEqual(parsed.skipped, [])
})

test('CLI discover exits 2 when --core has no value', () => {
  const run = runCli(['discover', '--json'])
  assert.equal(run.status, 2)
})

test('CLI validate exits 0 on a valid envelope file and 1 on an invalid one', () => {
  const root = scratchRoot()
  const good = path.join(root, 'good.json')
  fs.writeFileSync(
    good,
    JSON.stringify({
      ok: true,
      extension: 'bs-review-x',
      role: 'lens',
      items: [{ severity: 'W', file: 'a', line: 1, title: 't', detail: 'd' }],
    }),
  )
  const okRun = runCli(['validate', '--role', 'lens', '--file', good])
  assert.equal(okRun.status, 0)
  assert.equal(JSON.parse(okRun.stdout).ok, true)

  const bad = path.join(root, 'bad.json')
  fs.writeFileSync(bad, JSON.stringify({ role: 'surface', items: [] }))
  const badRun = runCli(['validate', '--role', 'lens', '--file', bad])
  assert.equal(badRun.status, 1)
  assert.equal(JSON.parse(badRun.stdout).ok, false)
})

test('CLI validate reports a missing file as clean JSON, never a stack trace', () => {
  const missing = path.join(scratchRoot(), 'nope.json')
  const run = runCli(['validate', '--role', 'lens', '--file', missing])
  assert.equal(run.status, 1)
  const parsed = JSON.parse(run.stdout) // must be parseable JSON, not a thrown stack trace
  assert.equal(parsed.ok, false)
  assert.ok(parsed.errors.length >= 1)
})
