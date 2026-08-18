import { execFileSync } from 'node:child_process'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import {
  EXTENSION_ROLES,
  ROLE_SCHEMAS,
  SKIP_REASONS,
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

// BOS-744: `extensionMarker`'s contract above is unchanged — it is role-generic and simply omits an
// unusable binding. What changes is that `discoverExtensions` no longer lets that omission pass
// SILENTLY for a `role: lens` descriptor. Before this, a lens extension that declared `lens: 42` was
// reported by the core as `unbound` — the same line a descriptor that never declared a binding gets
// — so the operator had no signal that the extension tried to bind and failed.
test('discoverExtensions reports an invalid lens binding as a skip instead of dropping it', () => {
  for (const declared of ['  lens:', '  lens: 42', '  lens: ""']) {
    const root = scratchRoot()
    writeSkill(root, 'bs-review-misbound', [
      'name: bs-review-misbound',
      'x-boss-extension:',
      '  extends: bs-review',
      '  role: lens',
      declared,
    ])
    const { extensions, skipped } = discoverExtensions({ core: 'bs-review', root, role: 'lens' })
    assert.deepEqual(
      extensions,
      [],
      `${declared}: a rejected extension must never reach .extensions`,
    )
    assert.deepEqual(skipped, [
      {
        name: 'bs-review-misbound',
        reason: 'invalid "lens" binding (expected a non-empty string)',
        code: 'invalid-lens-binding',
        deliberate: false,
      },
    ])
  }
})

test('discoverExtensions still admits a lens extension that declares no binding at all', () => {
  // The boundary on the other side of the skip above: an ABSENT `lens` key is the documented
  // unbound-but-valid case (it starts at tier 2), not a failed declaration.
  const root = scratchRoot()
  writeSkill(root, 'bs-review-unbound', [
    'name: bs-review-unbound',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lens',
  ])
  const { extensions, skipped } = discoverExtensions({ core: 'bs-review', root, role: 'lens' })
  assert.deepEqual(
    extensions.map((e) => e.name),
    ['bs-review-unbound'],
  )
  assert.deepEqual(skipped, [])
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
// they serve without adding a key to `.boss-skills.json` `lensMap` (a repo-local surface that
// BOS-850 deliberately keeps separate from the published `DEFAULT_CONFIG` catalogue).
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

// BOS-744, the falsification for the SCOPING of the new `invalid-lens-binding` skip. The test above
// pairs a non-`lens` role with a VALID `lens` value, so it passes with or without the
// `marker.role === 'lens'` guard in discoverExtensions — `marker.lens` is defined either way and the
// skip could never fire. Only an INVALID value on a non-`lens` role separates the two: with the
// guard the descriptor is carried through (the `lens` key is role-generic, so a stray one on another
// role is inert), without it every round extension carrying a mistyped key would be rejected into
// `skipped` and its Tier-1 dispatch would vanish.
test('discoverExtensions does not skip a non-lens role whose stray lens key is invalid', () => {
  for (const declared of ['  lens:', '  lens: 42', '  lens: ""']) {
    const root = scratchRoot()
    writeSkill(root, 'bs-review-strayinvalid', [
      'name: bs-review-strayinvalid',
      'x-boss-extension:',
      '  extends: bs-review',
      '  role: round',
      declared,
    ])
    const { extensions, skipped } = discoverExtensions({ core: 'bs-review', root, role: 'round' })
    assert.deepEqual(
      extensions.map((e) => e.name),
      ['bs-review-strayinvalid'],
      `${declared}: a stray invalid lens key must not reject a non-lens descriptor`,
    )
    // The unusable value is dropped by extensionMarker exactly as before, so the descriptor is
    // byte-identical to one authored without the key at all.
    assert.equal(extensions[0].lens, undefined, declared)
    assert.deepEqual(skipped, [], declared)
  }
})

// BOS-856: `capability` is the round-side mirror of `lens` — it binds a discovered round to the
// review capability it covers, so a core running that capability as a DEFAULT round can tell the
// repo already has it and run one pass instead of two. The byte-identity assertions below are the
// real contract: an extension that declares nothing must be indistinguishable from one authored
// before the key existed, or adding the key silently changes every existing repo's descriptors.
test('discoverExtensions carries a declared capability binding onto the descriptor', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-review-crossmodel', [
    'name: bs-review-crossmodel',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: round',
    '  order: 20',
    '  capability: second-voice',
  ])
  const { extensions } = discoverExtensions({ core: 'bs-review', root, role: 'round' })
  assert.deepEqual(
    extensions.map((e) => e.capability),
    ['second-voice'],
  )
})

test('discoverExtensions omits capability for absent, blank, or non-string declarations', () => {
  // One descriptor is built with no capability key at all — that is "today's" shape — and every
  // malformed declaration must produce a descriptor whose JSON matches it exactly.
  const baselineRoot = scratchRoot()
  writeSkill(baselineRoot, 'bs-review-plain', [
    'name: bs-review-plain',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: round',
  ])
  const baseline = discoverExtensions({ core: 'bs-review', root: baselineRoot, role: 'round' })
    .extensions[0]
  assert.equal('capability' in baseline, false)
  const stripPaths = (d) => JSON.stringify({ ...d, dir: '', skillPath: '', name: '' })

  for (const declared of ['  capability:', '  capability: 42', '  capability: ""']) {
    const root = scratchRoot()
    writeSkill(root, 'bs-review-plain', [
      'name: bs-review-plain',
      'x-boss-extension:',
      '  extends: bs-review',
      '  role: round',
      declared,
    ])
    const { extensions } = discoverExtensions({ core: 'bs-review', root, role: 'round' })
    assert.equal(extensions.length, 1, `${declared} should still be discovered`)
    assert.equal(
      stripPaths(extensions[0]),
      stripPaths(baseline),
      `${declared}: descriptor must be byte-identical to the undeclared one`,
    )
  }
})

test('discoverExtensions does not filter a lens extension carrying a stray capability key', () => {
  // Same role-generic tolerance `lens` gets: the key is meaningful only for rounds, but a stray
  // one is inert rather than a discovery failure.
  const root = scratchRoot()
  writeSkill(root, 'bs-review-straylens', [
    'name: bs-review-straylens',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lens',
    '  capability: second-voice',
  ])
  const { extensions, skipped } = discoverExtensions({ core: 'bs-review', root, role: 'lens' })
  assert.deepEqual(
    extensions.map((e) => e.name),
    ['bs-review-straylens'],
  )
  assert.equal(extensions[0].capability, 'second-voice')
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

test('discoverExtensions recognizes knowledge extensions and their result schema', () => {
  const root = scratchRoot()
  writeSkill(root, 'boss-build-knowledge', [
    'name: boss-build-knowledge',
    'x-boss-extension:',
    '  extends: boss-build',
    '  role: knowledge',
    '  order: 40',
  ])

  const { extensions, skipped } = discoverExtensions({
    core: 'boss-build',
    root,
    role: 'knowledge',
  })
  assert.deepEqual(
    extensions.map((extension) => extension.name),
    ['boss-build-knowledge'],
  )
  assert.deepEqual(skipped, [])
  assert.deepEqual(ROLE_SCHEMAS.knowledge, ['path', 'title', 'kind'])
})

test('validateResult rejects knowledge fields that are not non-empty strings', () => {
  // `path` is a knowledge item's proof of persistence, exactly as `noteId` is for notes: an item
  // carrying `path: ''` passes the presence check while proving no artifact reached the tree.
  for (const [field, value] of [
    ['path', ''],
    ['path', '   '],
    ['title', null],
    ['kind', {}],
  ]) {
    const item = {
      path: 'docs/solutions/skills/extension-roles.md',
      title: 'A new role needs two registry edits',
      kind: 'solution',
    }
    item[field] = value

    const result = validateResult(
      {
        ok: true,
        extension: 'boss-build-knowledge',
        role: 'knowledge',
        items: [item],
      },
      'knowledge',
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
  const found = extensions.find((e) => e.name === 'boss-plan-compound-engineering')
  assert.ok(
    found,
    `expected boss-plan-compound-engineering in ${JSON.stringify(extensions.map((e) => e.name))}`,
  )
  assert.equal(found.role, 'draft')
})

test('discoverExtensions finds the committed boss-review round extensions repo-local', () => {
  const { extensions } = discoverExtensions({ core: 'boss-review', root: repoRoot, role: 'round' })
  const names = extensions.map((e) => e.name)
  for (const expected of [
    'boss-review-ce',
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

test('the committed boss-review rounds declare the capabilities they cover', () => {
  // These bindings are what suppress boss-review's default rounds for this repo. Losing one does
  // not fail loudly — it just makes the repo run the capability twice — so assert them directly.
  const { extensions } = discoverExtensions({ core: 'boss-review', root: repoRoot, role: 'round' })
  const bound = Object.fromEntries(extensions.map((e) => [e.name, e.capability]))
  assert.equal(bound['boss-review-ce'], 'code-review')
  assert.equal(bound['boss-review-crossmodel'], 'second-voice')
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
  const found = extensions.find((e) => e.name === 'boss-build-ce')
  assert.ok(found, `expected boss-build-ce in ${JSON.stringify(extensions.map((e) => e.name))}`)
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
  assert.deepEqual(skipped, [
    {
      name: 'bs-review-headless',
      reason: 'no SKILL.md',
      code: 'no-skill-md',
      deliberate: false,
    },
  ])
})

// BOS-744, the exhaustiveness ratchet. Note 7's failure mode is that the deliberate-vs-broken
// classification lived in ONE consuming core's prose, keyed on a literal reason string, so every
// other site had to restate it and any NEW reason defaulted to "not classified anywhere". Owning the
// classification in the helper only helps if a new `skipped.push` cannot arrive unclassified — this
// drives discovery through a fixture reproducing every code and fails when an emitted code is absent
// from the exported table.
test('every skip discoverExtensions can emit carries a classified code', () => {
  const root = scratchRoot()
  const writeRaw = (name, text) => {
    const dir = path.join(root, '.claude', 'skills', name)
    fs.mkdirSync(dir, { recursive: true })
    fs.writeFileSync(path.join(dir, 'SKILL.md'), text)
  }
  // no-skill-md
  fs.mkdirSync(path.join(root, '.claude', 'skills', 'bs-review-nofile'), { recursive: true })
  // malformed-frontmatter
  writeRaw(
    'bs-review-unfenced',
    '---\nname: x\nx-boss-extension:\n  extends: bs-review\n\n# body\n',
  )
  // incomplete-marker
  writeRaw('bs-review-partial', '---\nname: x\nx-boss-extension:\n  extends: bs-review\n---\n')
  // missing-marker
  writeSkill(root, 'bs-review-helper', ['name: bs-review-helper'])
  // extends-unrelated-core: a `bs-review-*` directory is unreachable from `bs-plan`, so this
  // marker is simply wrong and stays reportable from EVERY core that enumerates it.
  writeSkill(root, 'bs-review-foreign', [
    'name: bs-review-foreign',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: lens',
  ])
  // extends-other-core: the genuine nesting case. `bs-plan` really does own `bs-plan-notes`, so
  // the `bs` pass below must stay quiet about it.
  writeSkill(root, 'bs-plan-notes', [
    'name: bs-plan-notes',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: lens',
  ])
  // wrong-role
  writeSkill(root, 'bs-review-typo', [
    'name: bs-review-typo',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lenz',
  ])
  // invalid-lens-binding
  writeSkill(root, 'bs-review-misbound', [
    'name: bs-review-misbound',
    'x-boss-extension:',
    '  extends: bs-review',
    '  role: lens',
    '  lens: 42',
  ])

  const emitted = new Set()
  for (const { core, role } of [
    { core: 'bs-review', role: 'lens' },
    // 'lenz' is an unknown REQUESTED role; it is a property of the caller's argument rather than
    // of any fixture, so it needs its own pass.
    { core: 'bs-review', role: 'lenz' },
    // The NESTING core. `bs-` prefixes every directory above, so this pass is the only one that
    // can reach `extends-other-core` — and it reaches it through `bs-plan-notes`, whose declared
    // `bs-plan` genuinely owns it. `bs-review-foreign` is NOT that case even from here: no core
    // named `bs-plan` can ever enumerate a `bs-review-*` directory, so it stays reportable.
    { core: 'bs', role: 'lens' },
  ]) {
    const { skipped } = discoverExtensions({ core, root, role })
    for (const entry of skipped) {
      assert.ok(
        entry.code in SKIP_REASONS,
        `skip code ${JSON.stringify(entry.code)} for ${entry.name} is absent from SKIP_REASONS — classify it there rather than letting it default to silence: ${JSON.stringify(entry)}`,
      )
      assert.equal(typeof entry.deliberate, 'boolean', JSON.stringify(entry))
      assert.equal(
        entry.deliberate,
        SKIP_REASONS[entry.code].deliberate,
        `${entry.code}: entry disagrees with the exported classification`,
      )
      assert.equal(typeof entry.reason, 'string', JSON.stringify(entry))
      assert.deepEqual(Object.keys(entry).sort(), ['code', 'deliberate', 'name', 'reason'])
      emitted.add(entry.code)
    }
  }
  // The one code no fixture can reach: `unreadable-frontmatter` fires only when reading the file
  // throws, which needs a filesystem fault rather than a content shape. It is classified all the
  // same, so the reachable set is the whole table minus that one.
  assert.deepEqual(
    [...emitted].sort(),
    Object.keys(SKIP_REASONS)
      .filter((code) => code !== 'unreadable-frontmatter')
      .sort(),
    'the fixture must reproduce every reachable skip code, or the ratchet stops being exhaustive',
  )
})

test('exactly the two non-extension skips are classified deliberate', () => {
  // A `deliberate: true` skip is the contract working: the directory is a same-prefix skill that is
  // not an extension of THIS core, so reporting it would cry wolf on every run. Everything else is a
  // misconfiguration a core MUST record. This partition is the whole point of the field, so it is
  // pinned name-exact rather than by count.
  const deliberate = Object.entries(SKIP_REASONS)
    .filter(([, spec]) => spec.deliberate)
    .map(([code]) => code)
    .sort()
  assert.deepEqual(deliberate, ['extends-other-core', 'missing-marker'])
  assert.deepEqual(Object.keys(SKIP_REASONS).sort(), [
    'extends-other-core',
    'extends-unrelated-core',
    'incomplete-marker',
    'invalid-lens-binding',
    'malformed-frontmatter',
    'missing-marker',
    'no-skill-md',
    'unknown-requested-role',
    'unreadable-frontmatter',
    'wrong-role',
  ])
})

// A wrong `extends` on a correctly-named directory is unreachable from the core it names — the
// contract fixes an extension's directory to `<core>-<suffix>` and discovery scans only that prefix
// — so nothing else would ever report it. Suppressing it as "somebody else's extension" is the
// silent drop this ticket exists to end, which is why it is a SEPARATE code from the nesting case.
test('a wrong extends is reportable, while a nesting core stays quiet about its neighbours', () => {
  const root = scratchRoot()
  writeSkill(root, 'bs-review-typoextends', [
    'name: bs-review-typoextends',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: lens',
  ])

  // From `bs-review`: `bs-plan` is not a `bs-review-` descendant, so the marker is simply wrong.
  const near = discoverExtensions({ core: 'bs-review', root, role: 'lens' })
  assert.deepEqual(near.extensions, [])
  assert.deepEqual(near.skipped, [
    {
      name: 'bs-review-typoextends',
      reason: 'extends "bs-plan", not "bs-review"',
      code: 'extends-unrelated-core',
      deliberate: false,
    },
  ])

  // From `bs`, whose prefix also matches the directory, the verdict is UNCHANGED. The question is
  // whether the DECLARED core could own this directory, not whether it is a sub-core of the one
  // asking: `bs-plan` can only ever enumerate `bs-plan-*`, so no core anywhere would report this
  // typo if `bs` stayed quiet about it. Classifying on the two core names instead would suppress it
  // here — the silent drop this ticket exists to end.
  const nesting = discoverExtensions({ core: 'bs', root, role: 'lens' })
  assert.deepEqual(nesting.extensions, [])
  assert.deepEqual(nesting.skipped, [
    {
      name: 'bs-review-typoextends',
      reason: 'extends "bs-plan", not "bs"',
      code: 'extends-unrelated-core',
      deliberate: false,
    },
  ])

  // The genuine nesting case, for contrast: `bs-plan` really does own `bs-plan-notes`, so `bs` must
  // stay quiet about it or the warning fires on every run for as long as that extension exists.
  writeSkill(root, 'bs-plan-notes', [
    'name: bs-plan-notes',
    'x-boss-extension:',
    '  extends: bs-plan',
    '  role: lens',
  ])
  const owned = discoverExtensions({ core: 'bs', root, role: 'lens' })
  assert.deepEqual(
    owned.skipped.find((s) => s.name === 'bs-plan-notes'),
    {
      name: 'bs-plan-notes',
      reason: 'extends "bs-plan", not "bs"',
      code: 'extends-other-core',
      deliberate: true,
    },
  )
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
    extension: 'boss-review-ce',
    role: 'round',
    items: [{ severity: 'Warning', file: 'a.go', line: 3, title: 't', detail: 'd' }],
  }
  assert.deepEqual(validateResult(envelope, 'round'), { ok: true, errors: [] })
})

test('validateResult rejects a round envelope missing a findings key', () => {
  const envelope = {
    ok: true,
    extension: 'boss-review-ce',
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

test('ROLE_SCHEMAS enumerates every role discovery accepts', () => {
  // Name-exact rather than a count, so a role cannot arrive without a documented contract
  // (docs/skills/extension-contract.md). BOS-744 re-baselined this list from six keys to nine ON
  // PURPOSE: `KNOWN_EXTENSION_ROLES` and `ROLE_SCHEMAS` are now DERIVED from one `EXTENSION_ROLES`
  // table, so the drift that let discovery accept `draft`/`methodology`/`agent-driver` while
  // `validateResult` answered `unknown role` is no longer expressible.
  assert.deepEqual(Object.keys(ROLE_SCHEMAS).sort(), [
    'agent-driver',
    'draft',
    'knowledge',
    'lens',
    'methodology',
    'notes',
    'plan-reviewer',
    'round',
    'surface',
  ])
  assert.deepEqual(ROLE_SCHEMAS.round, ROLE_SCHEMAS.lens)
})

test('the merged role table is the single source for discovery and validation', () => {
  // The invariant that replaces the two hand-maintained literals: every role in the table declares a
  // non-empty result schema, and ROLE_SCHEMAS is exactly its projection. A role added to
  // EXTENSION_ROLES with no `keys` fails here rather than surfacing as an empty schema that
  // validates everything.
  assert.deepEqual(Object.keys(ROLE_SCHEMAS).sort(), Object.keys(EXTENSION_ROLES).sort())
  for (const [role, spec] of Object.entries(EXTENSION_ROLES)) {
    assert.ok(['items', 'fields'].includes(spec.kind), `${role}: unknown result kind ${spec.kind}`)
    assert.ok(
      Array.isArray(spec.keys) && spec.keys.length > 0,
      `${role}: declares no result schema`,
    )
    assert.deepEqual(ROLE_SCHEMAS[role], spec.keys, role)
    assert.notEqual(
      validateResult({}, role).errors[0],
      `unknown role "${role}"`,
      `${role}: discovery accepts it, so validateResult must not answer "unknown role"`,
    )
  }
})

test('validateResult accepts the behavior-shipping roles that ship no items array', () => {
  assert.deepEqual(
    validateResult(
      { ok: true, extension: 'boss-plan-x', role: 'draft', planPath: '.linear-plans/A-1.md' },
      'draft',
    ),
    { ok: true, errors: [] },
  )
  // `planPath` is a claim of persistence, exactly like `notes`' `noteId`: blank proves nothing.
  assert.deepEqual(
    validateResult({ ok: true, extension: 'boss-plan-x', role: 'draft', planPath: '' }, 'draft'),
    { ok: false, errors: ['"planPath" is not a non-empty string'] },
  )
  const taskContract = {
    taskId: 'Task 1',
    filesTouched: [],
    testsAddedOrPassing: [],
    interfaceSignatures: [],
    residualRisks: [],
    decisionsRecorded: [],
    commitsMade: [],
  }
  assert.deepEqual(validateResult(taskContract, 'methodology'), { ok: true, errors: [] })
  const { commitsMade, ...missingCommits } = taskContract
  assert.deepEqual(validateResult(missingCommits, 'methodology'), {
    ok: false,
    errors: ['missing "commitsMade"'],
  })
  const surfaceRun = {
    surface: 'tui',
    captureShapes: [],
    brief: {},
    agentResult: { passed: true, summary: '', evidence: [], steps: [] },
    hasFailure: false,
    noSurface: false,
    scanTexts: [],
    elapsedMs: 1,
    reasonCode: null,
  }
  assert.deepEqual(validateResult(surfaceRun, 'agent-driver'), { ok: true, errors: [] })
})

test('extension contract documents draft as a plan-writing exception', () => {
  const contract = fs.readFileSync('docs/skills/extension-contract.md', 'utf8')
  assert.match(contract, /`draft` → `\{ mode, planPath, ticket, designDoc\? \}`/)
  assert.match(contract, /`draft` is the behavior-writing exception/)
  assert.match(contract, /does not use `items\[\]`/)
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
