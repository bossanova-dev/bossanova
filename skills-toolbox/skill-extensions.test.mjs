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
    const stdout = execFileSync('node', ['skills-toolbox/skill-extensions.mjs', ...args], {
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

test('extensionMarker surfaces the optional lens binding when declared', () => {
  const { data } = parseFrontmatter(
    '---\nname: bs-review-golang\nx-boss-extension:\n  extends: bs-review\n  role: lens\n  order: 40\n  lens: go\n---\n',
  )
  assert.deepEqual(extensionMarker(data), {
    extends: 'bs-review',
    role: 'lens',
    order: 40,
    lens: 'go',
  })
})

test('extensionMarker omits the lens field entirely when the marker declares none', () => {
  const { data } = parseFrontmatter(
    '---\nname: bs-review-round\nx-boss-extension:\n  extends: bs-review\n  role: round\n---\n',
  )
  const marker = extensionMarker(data)
  assert.equal('lens' in marker, false, `expected no lens key, got ${JSON.stringify(marker)}`)
})

test('extensionMarker ignores a non-string or empty lens binding rather than failing', () => {
  for (const declared of ['  lens:', '  lens: 42', '  lens: ""']) {
    const { data } = parseFrontmatter(
      `---\nname: bs-review-x\nx-boss-extension:\n  extends: bs-review\n  role: lens\n${declared}\n---\n`,
    )
    const marker = extensionMarker(data)
    assert.ok(marker, `marker should still resolve for ${declared}`)
    assert.equal('lens' in marker, false, `${declared}: ${JSON.stringify(marker)}`)
  }
})

test('extensionMarker keeps a quoted numeric lens binding as a string', () => {
  // A config lens id only has to be a non-empty string, so "42" is a legal id. The
  // quoted marker must survive frontmatter coercion as the string "42" — coercing it
  // to Number 42 makes extensionMarker drop the field and the lens never dispatches.
  const { data } = parseFrontmatter(
    '---\nname: bs-review-42\nx-boss-extension:\n  extends: bs-review\n  role: lens\n  lens: "42"\n---\n',
  )
  const marker = extensionMarker(data)
  assert.equal(marker.lens, '42')
  assert.equal(typeof marker.lens, 'string')
})

test('discoverExtensions binds a descriptor to a quoted numeric lens id', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-review-numeric', [
    'name: bs-review-numeric',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lens',
    '  lens: "42"',
  ])
  const { extensions } = discoverExtensions({ core: 'bs-review', root, role: 'lens' })
  assert.equal(extensions.length, 1)
  assert.equal(extensions[0].lens, '42')
})

test('coercion keeps unquoted integers numeric and quoted ones stringly typed', () => {
  const marker = (declared) =>
    extensionMarker(
      parseFrontmatter(
        `---\nname: bs-review-x\nx-boss-extension:\n  extends: bs-review\n  role: lens\n${declared}\n---\n`,
      ).data,
    )
  // Bare integer -> Number, and extensionMarker honours it as the declared order.
  assert.equal(marker('  order: 20').order, 20)
  // Quoted -> string, which is not a valid order, so the default applies.
  assert.equal(marker('  order: "20"').order, 100)
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

// BOS-678: the lens binding is declared extension-side, as an optional `lens: <lensMap id>` key
// on the marker, so `boss-review` Phase 1 can index discovered lens extensions by the lens entry
// they serve without adding a key to `.boss-skills.json` `lensMap` (which `skill-config.test.mjs`
// deep-equals against `DEFAULT_CONFIG`, i.e. against the published payload's defaults).
test('discoverExtensions carries a declared lens binding onto the descriptor', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-review-golang', [
    'name: bs-review-golang',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lens',
    '  order: 40',
    '  lens: go',
  ])
  const { extensions } = discoverExtensions({ core: 'bs-review', root, role: 'lens' })
  assert.deepEqual(
    extensions.map((e) => e.lens),
    ['go'],
  )
})

test('discoverExtensions omits the lens field for an unbound extension', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-review-unbound', [
    'name: bs-review-unbound',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lens',
  ])
  const { extensions } = discoverExtensions({ core: 'bs-review', root, role: 'lens' })
  assert.equal(extensions.length, 1)
  assert.deepEqual(Object.keys(extensions[0]).sort(), ['dir', 'name', 'order', 'role', 'skillPath'])
})

test('discoverExtensions does not filter a round extension carrying a stray lens key', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-review-strayround', [
    'name: bs-review-strayround',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: round',
    '  lens: go',
  ])
  const { extensions, skipped } = discoverExtensions({ core: 'bs-review', root, role: 'round' })
  assert.deepEqual(
    extensions.map((e) => e.name),
    ['bs-review-strayround'],
  )
  assert.equal(extensions[0].lens, 'go')
  assert.deepEqual(skipped, [])
})

test('discoverExtensions recognizes notes extensions and their result schema', () => {
  const root = scratchRoot()
  writeSkill(root, 'boss-build-notes', [
    'name: boss-build-notes',
    'x-boss-extension:',
    '  extends: boss-build',
    '  role: notes',
  ])

  const { extensions, skipped } = discoverExtensions({ core: 'boss-build', root, role: 'notes' })
  assert.deepEqual(
    extensions.map((extension) => extension.name),
    ['boss-build-notes'],
  )
  assert.deepEqual(skipped, [])
  assert.deepEqual(ROLE_SCHEMAS.notes, ['tag', 'body', 'noteId'])
})

test('validateResult rejects notes fields that are not non-empty strings', () => {
  for (const [field, value] of [
    ['tag', null],
    ['body', {}],
    ['noteId', ''],
  ]) {
    const item = { tag: 'improvement', body: 'Keep the validation ratchet.', noteId: 'note-123' }
    item[field] = value

    const result = validateResult(
      {
        ok: true,
        extension: 'boss-build-notes',
        role: 'notes',
        items: [item],
      },
      'notes',
    )

    assert.equal(result.ok, false, field)
    assert.ok(
      result.errors.includes(`item 0 "${field}" is not a non-empty string`),
      `${field}: ${result.errors.join(', ')}`,
    )
  }
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

// Repo-local tier-1 discovery regression (BOS-288). Bossanova rides its own opinionated
// extensions because every worktree carries the committed `.claude/skills/boss-<skill>-*`
// and this discover helper scans repo-local (`root` = cwd), never the installed payload.
// This guards against a future move of discovery to the installed root, or an extension
// being deleted / mis-marked. Assert INCLUSION of the known committed extensions (not an
// exact-count equality) so adding a new extension never turns this into a churn magnet.
const repoRoot = path.resolve(import.meta.dirname, '..')

test('discoverExtensions finds the committed boss-plan draft extension repo-local', () => {
  const { extensions } = discoverExtensions({ core: 'boss-plan', root: repoRoot, role: 'draft' })
  const found = extensions.find((e) => e.name === 'boss-plan-draft')
  assert.ok(found, `expected boss-plan-draft in ${JSON.stringify(extensions.map((e) => e.name))}`)
  assert.equal(found.role, 'draft')
})

test('discoverExtensions finds the committed boss-review round extensions repo-local', () => {
  const { extensions } = discoverExtensions({ core: 'boss-review', root: repoRoot, role: 'round' })
  const names = extensions.map((e) => e.name)
  for (const expected of [
    'boss-review-requesting',
    'boss-review-crossmodel',
    'boss-review-thermonuclear',
  ]) {
    assert.ok(names.includes(expected), `expected ${expected} in ${JSON.stringify(names)}`)
  }
  assert.ok(
    extensions.every((e) => e.role === 'round'),
    `every round descriptor should carry role "round": ${JSON.stringify(extensions)}`,
  )
})

test('discoverExtensions finds the committed boss-review lens extensions with their bindings', () => {
  const { extensions, skipped } = discoverExtensions({
    core: 'boss-review',
    root: repoRoot,
    role: 'lens',
  })
  assert.deepEqual(skipped, [])
  const bound = Object.fromEntries(extensions.map((e) => [e.name, e.lens]))
  assert.deepEqual(bound, {
    'boss-review-golang': 'go',
    'boss-review-tui': 'tui',
    'boss-review-web': 'web',
  })
})

test('discoverExtensions finds the committed boss-build methodology extension repo-local', () => {
  const out = execFileSync(
    'node',
    [
      'skills-toolbox/skill-extensions.mjs',
      'discover',
      '--core',
      'boss-build',
      '--role',
      'methodology',
      '--json',
    ],
    { encoding: 'utf8', cwd: process.cwd() },
  )
  const { extensions } = JSON.parse(out)
  const found = extensions.find((e) => e.name === 'boss-build-superpowers')
  assert.ok(
    found,
    `expected boss-build-superpowers in ${JSON.stringify(extensions.map((e) => e.name))}`,
  )
  assert.equal(found.role, 'methodology')
})

test('discoverExtensions finds the committed boss-proof surface extensions repo-local', () => {
  const { extensions } = discoverExtensions({ core: 'boss-proof', root: repoRoot, role: 'surface' })
  const names = extensions.map((e) => e.name)
  for (const expected of ['boss-proof-web', 'boss-proof-marketing', 'boss-proof-docs']) {
    assert.ok(names.includes(expected), `expected ${expected} in ${JSON.stringify(names)}`)
  }
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

// `parseFrontmatter` does not throw on a broken fence — it returns empty `data`, exactly like a
// SKILL.md whose frontmatter is valid and simply declares no marker. Cores are entitled to ignore
// the latter (a markerless same-prefix helper is a deliberate non-extension), so collapsing the
// two would let a genuine Tier-1 extension whose fence is broken, or whose marker is half-written,
// vanish under that exemption and let the lens report Tier 2 as its intended tier. Each failed
// declaration therefore needs a reason of its own, distinct from the markerless one.
test('discoverExtensions separates malformed frontmatter and a partial marker from markerless', () => {
  const root = scratchRoot()
  const write = (name, text) => {
    const dir = path.join(root, '.claude', 'skills', name)
    fs.mkdirSync(dir, { recursive: true })
    fs.writeFileSync(path.join(dir, 'SKILL.md'), text)
  }
  // A real lens extension whose closing `---` is missing.
  write(
    'bs-review-unfenced',
    '---\nname: bs-review-unfenced\nx-boss-extension:\n  extends: bs-review\n  role: lens\n\n# body\n',
  )
  // A real lens extension whose marker omits the required `role`.
  write(
    'bs-review-partial',
    '---\nname: bs-review-partial\nx-boss-extension:\n  extends: bs-review\n  lens: tui\n---\n\n# body\n',
  )
  // A deliberate non-extension: valid frontmatter, no marker at all.
  writeSkill(root, 'bs-review-helper', ['name: bs-review-helper', 'description: a helper'])

  const { extensions, skipped } = discoverExtensions({ core: 'bs-review', root, role: 'lens' })

  assert.deepEqual(extensions, [])
  const reasonFor = (name) => skipped.find((s) => s.name === name)?.reason
  assert.equal(reasonFor('bs-review-helper'), 'missing x-boss-extension marker')
  assert.equal(reasonFor('bs-review-unfenced'), 'malformed frontmatter: no parseable --- block')
  assert.equal(
    reasonFor('bs-review-partial'),
    'incomplete x-boss-extension marker: needs string "extends" and "role"',
  )
  // The markerless exemption a core applies must key off that exact reason, so no failed
  // declaration may share it.
  assert.equal(
    skipped.filter((s) => s.reason === 'missing x-boss-extension marker').length,
    1,
    `only the markerless helper may carry the exempt reason, got ${JSON.stringify(skipped)}`,
  )
})

test('parseFrontmatter reports whether a delimited block was present', () => {
  assert.equal(parseFrontmatter('---\nname: x\n---\n\nhello\n').hasFrontmatter, true)
  assert.equal(parseFrontmatter('no frontmatter here\n').hasFrontmatter, false)
  assert.equal(parseFrontmatter('---\nname: x\n\nhello\n').hasFrontmatter, false)
})

// A core loads a discovered extension by reading its descriptor's `skillPath` from disk, so an
// extension directory whose SKILL.md is absent must surface as a *reasoned skip* rather than
// silently vanishing. Cores gate their Tier-2/Tier-3 fallbacks on the Tier-1 dispatch SUCCEEDING,
// and this skip entry is the signal they fall through on — an empty `skipped` here would let a
// failed Tier 1 masquerade as "no extension configured" and silently drop the layer.
test('discoverExtensions reports a reasoned skip when SKILL.md is absent', () => {
  const root = scratchRoot()
  fs.mkdirSync(path.join(root, '.claude', 'skills', 'bs-review-headless'), { recursive: true })

  const { extensions, skipped } = discoverExtensions({ core: 'bs-review', root })

  assert.deepEqual(extensions, [])
  assert.deepEqual(skipped, [{ name: 'bs-review-headless', reason: 'no SKILL.md' }])
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
  assert.deepEqual(Object.keys(ROLE_SCHEMAS).sort(), [
    'lens',
    'notes',
    'plan-reviewer',
    'round',
    'surface',
  ])
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
    [
      'skills-toolbox/skill-extensions.mjs',
      'discover',
      '--core',
      'bs-review',
      '--root',
      root,
      '--json',
    ],
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
    [
      'skills-toolbox/skill-extensions.mjs',
      'discover',
      '--core',
      'bs-plan',
      '--root',
      root,
      '--json',
    ],
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
      'skills-toolbox/skill-extensions.mjs',
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
