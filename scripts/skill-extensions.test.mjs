import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  DEFAULT_EXTENSION_ROOTS,
  ROLE_SCHEMAS,
  discoverExtensions,
  resolveExtensionRoots,
  validateResult,
} from './skill-extensions.mjs'
import {
  DEFAULT_CONFIG,
  extensionRootsFor,
  loadSkillConfig,
} from '../skills-toolbox/skill-config.mjs'

function scratchRoot() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'skill-ext-'))
}

function writeSkill(root, name, frontmatterLines, skillRoot = '.claude/skills') {
  const dir = path.join(root, skillRoot, name)
  fs.mkdirSync(dir, { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'SKILL.md'),
    ['---', ...frontmatterLines, '---', '', `# ${name}`, ''].join('\n'),
  )
}

function descriptorSummary(result) {
  return {
    extensions: result.extensions.map(({ name, role, order }) => ({ name, role, order })),
    skipped: result.skipped,
  }
}

test('discoverExtensions recognizes a notes extension role', () => {
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
})

test('discoverExtensions scans agent-neutral roots with first-root dedupe by extension name', () => {
  const cases = [
    { name: 'claude-only', roots: ['.claude/skills'] },
    { name: 'codex-only', roots: ['.codex/skills'] },
    { name: 'both roots', roots: ['.claude/skills', '.codex/skills'] },
  ]

  for (const scenario of cases) {
    const root = scratchRoot()
    for (const skillRoot of scenario.roots) {
      writeSkill(
        root,
        'boss-review-alpha',
        [
          'name: boss-review-alpha',
          'x-boss-extension:',
          '  extends: boss-review',
          '  role: round',
          '  order: 20',
        ],
        skillRoot,
      )
      writeSkill(
        root,
        'boss-review-beta',
        [
          'name: boss-review-beta',
          'x-boss-extension:',
          '  extends: boss-review',
          '  role: round',
          '  order: 10',
        ],
        skillRoot,
      )
    }

    const result = discoverExtensions({ core: 'boss-review', root, role: 'round' })
    assert.deepEqual(
      descriptorSummary(result),
      {
        extensions: [
          { name: 'boss-review-beta', role: 'round', order: 10 },
          { name: 'boss-review-alpha', role: 'round', order: 20 },
        ],
        skipped: [],
      },
      scenario.name,
    )
    assert.equal(
      new Set(result.extensions.map((extension) => extension.name)).size,
      2,
      scenario.name,
    )
    if (scenario.name === 'both roots') {
      assert.ok(
        result.extensions.every((extension) =>
          extension.dir.startsWith(path.join(root, '.claude', 'skills')),
        ),
        'both roots should keep the first root descriptor',
      )
    }
  }
})

test('discoverExtensions accepts a roots override and empty agent-neutral roots are a no-op', () => {
  const root = scratchRoot()
  const customRoot = path.join(root, 'custom-skills')
  writeSkill(
    root,
    'boss-build-custom',
    ['name: boss-build-custom', 'x-boss-extension:', '  extends: boss-build', '  role: notes'],
    'custom-skills',
  )

  assert.deepEqual(discoverExtensions({ core: 'boss-build', root, role: 'notes' }), {
    extensions: [],
    skipped: [],
  })
  assert.deepEqual(
    discoverExtensions({
      core: 'boss-build',
      root,
      role: 'notes',
      roots: [customRoot],
    }).extensions.map((extension) => extension.name),
    ['boss-build-custom'],
  )
})

test('extensionRootsFor exposes a project-agnostic default and config override', () => {
  const root = scratchRoot()
  assert.deepEqual(DEFAULT_EXTENSION_ROOTS, ['.claude/skills', '.codex/skills'])
  assert.deepEqual(extensionRootsFor(DEFAULT_CONFIG), DEFAULT_EXTENSION_ROOTS)

  fs.writeFileSync(
    path.join(root, '.boss-skills.json'),
    JSON.stringify({
      extensionRoots: ['custom/skills'],
    }),
  )
  assert.deepEqual(extensionRootsFor(loadSkillConfig({ cwd: root })), ['custom/skills'])
})

test('resolveExtensionRoots returns existing absolute roots in configured order', () => {
  const root = scratchRoot()
  fs.mkdirSync(path.join(root, '.codex', 'skills'), { recursive: true })
  fs.mkdirSync(path.join(root, 'custom', 'skills'), { recursive: true })

  assert.deepEqual(resolveExtensionRoots(root, { extensionRoots: ['missing', '.codex/skills'] }), [
    path.join(root, '.codex', 'skills'),
  ])
  assert.deepEqual(
    resolveExtensionRoots(root, { extensionRoots: ['custom/skills', '.codex/skills'] }),
    [path.join(root, 'custom', 'skills'), path.join(root, '.codex', 'skills')],
  )
})

test('resolveExtensionRoots rejects configured roots outside the repository', () => {
  const root = scratchRoot()
  const outside = scratchRoot()
  fs.mkdirSync(path.join(outside, 'skills'), { recursive: true })

  assert.throws(
    () => resolveExtensionRoots(root, { extensionRoots: [path.join(outside, 'skills')] }),
    /extensionRoots\s+entry\s+escapes\s+repository\s+root/,
  )
  assert.throws(
    () => resolveExtensionRoots(root, { extensionRoots: ['../outside-skills'] }),
    /extensionRoots\s+entry\s+escapes\s+repository\s+root/,
  )
})

test('resolveExtensionRoots rejects roots that symlink outside the repository', () => {
  const root = scratchRoot()
  const outside = scratchRoot()
  fs.mkdirSync(path.join(root, '.claude'), { recursive: true })
  fs.mkdirSync(path.join(outside, 'skills'), { recursive: true })
  fs.symlinkSync(path.join(outside, 'skills'), path.join(root, '.claude', 'skills'), 'dir')

  assert.throws(
    () => resolveExtensionRoots(root),
    /extensionRoots\s+entry\s+escapes\s+repository\s+root/,
  )
})

test('discoverExtensions uses configured extensionRoots and treats first matching root as authoritative', () => {
  const root = scratchRoot()
  fs.mkdirSync(path.join(root, 'custom', 'skills', 'boss-build-notes'), { recursive: true })
  writeSkill(
    root,
    'boss-build-notes',
    ['name: boss-build-notes', 'x-boss-extension:', '  extends: boss-build', '  role: notes'],
    '.codex/skills',
  )
  fs.writeFileSync(
    path.join(root, '.boss-skills.json'),
    JSON.stringify({ extensionRoots: ['custom/skills', '.codex/skills'] }),
  )

  const discovered = discoverExtensions({ core: 'boss-build', root, role: 'notes' })
  assert.deepEqual(discovered.extensions, [])
  assert.equal(discovered.skipped.length, 1)
  assert.equal(discovered.skipped[0].name, 'boss-build-notes')
  assert.equal(discovered.skipped[0].code, 'no-skill-md')
})

test('discoverExtensions silently omits notes extensions for established roles', () => {
  const root = scratchRoot()
  writeSkill(root, 'boss-build-notes', [
    'name: boss-build-notes',
    'x-boss-extension:',
    '  extends: boss-build',
    '  role: notes',
  ])

  const establishedRoles = [
    'lens',
    'round',
    'surface',
    'plan-reviewer',
    'agent-driver',
    'draft',
    'methodology',
    'knowledge',
  ]
  for (const role of establishedRoles) {
    const discovered = discoverExtensions({ core: 'boss-build', root, role })
    assert.deepEqual(discovered, { extensions: [], skipped: [] }, role)

    const cli = spawnSync(
      process.execPath,
      [
        path.join(import.meta.dirname, 'skill-extensions.mjs'),
        'discover',
        '--core',
        'boss-build',
        '--root',
        root,
        '--role',
        role,
        '--json',
      ],
      { encoding: 'utf8' },
    )
    assert.equal(cli.status, 0, role)
    assert.deepEqual(JSON.parse(cli.stdout), { extensions: [], skipped: [] }, role)
  }
})

test('notes discovery is stdout-only and exact for an empty root for every terminal core', () => {
  const root = scratchRoot()
  for (const core of ['boss-build', 'boss-plan', 'boss-review', 'boss-epic', 'boss-repair']) {
    const result = spawnSync(
      process.execPath,
      [
        path.join(import.meta.dirname, 'skill-extensions.mjs'),
        'discover',
        '--core',
        core,
        '--root',
        root,
        '--role',
        'notes',
        '--json',
      ],
      { encoding: 'utf8' },
    )

    assert.equal(result.status, 0, core)
    assert.equal(result.stderr, '', core)
    assert.equal(result.stdout, '{"extensions":[],"skipped":[]}\n', core)
  }
})

test('validateResult accepts lens and round envelopes that declare an inline fallback', () => {
  for (const role of ['lens', 'round']) {
    const envelope = {
      ok: true,
      extension: 'boss-review-example',
      role,
      items: [],
      notes: '',
      error: null,
      fallback: 'wrapped reviewer unavailable; ran the inline rubric',
    }
    assert.deepEqual(validateResult(envelope, role), { ok: true, errors: [] })
  }
})

test('validateResult accepts a well-formed notes envelope', () => {
  const envelope = {
    ok: true,
    extension: 'boss-build-notes',
    role: 'notes',
    items: [{ tag: 'retrospective', body: 'Keep the validation ratchet.', noteId: 'note-123' }],
  }
  assert.deepEqual(validateResult(envelope, 'notes'), { ok: true, errors: [] })
})

test('validateResult rejects a notes item missing noteId', () => {
  const result = validateResult(
    {
      ok: true,
      extension: 'boss-build-notes',
      role: 'notes',
      items: [{ tag: 'retrospective', body: 'Keep the validation ratchet.' }],
    },
    'notes',
  )
  assert.equal(result.ok, false)
  assert.ok(result.errors.some((error) => /missing "noteId"/.test(error)))
})

test('repository entrypoint rejects empty persisted-note identifiers', () => {
  const result = validateResult(
    {
      ok: true,
      extension: 'boss-build-notes',
      role: 'notes',
      items: [{ tag: 'improvement', body: 'Keep the validation ratchet.', noteId: '' }],
    },
    'notes',
  )

  assert.equal(result.ok, false)
  assert.ok(result.errors.includes('item 0 "noteId" is not a non-empty string'))
})

test('validate --role notes returns clean JSON for a missing noteId', () => {
  const result = spawnSync(
    process.execPath,
    [path.join(import.meta.dirname, 'skill-extensions.mjs'), 'validate', '--role', 'notes'],
    {
      encoding: 'utf8',
      input: JSON.stringify({
        ok: true,
        extension: 'boss-build-notes',
        role: 'notes',
        items: [{ tag: 'retrospective', body: 'Keep the validation ratchet.' }],
      }),
    },
  )

  assert.equal(result.status, 1)
  assert.doesNotMatch(result.stderr, /(?:^|\n)(?:Error:|\s+at )/)
  assert.deepEqual(JSON.parse(result.stdout), {
    ok: false,
    errors: ['item 0 missing "noteId"'],
  })
})

test('ROLE_SCHEMAS has the exact validated consumer-role ratchet', () => {
  // BOS-851 re-baselined this name-exact list from five keys to six by adding `knowledge`.
  // The list is EXACT rather than a superset check so a role added to `ROLE_SCHEMAS` without a
  // documented contract (docs/skills/extension-contract.md) cannot arrive unannounced.
  //
  // BOS-744 re-baselines it six → nine ON PURPOSE. `KNOWN_EXTENSION_ROLES` and `ROLE_SCHEMAS` are
  // no longer two hand-maintained literals that can drift: both are derived from one
  // `EXTENSION_ROLES` table, so every role discovery accepts necessarily declares a result schema.
  // `draft`, `methodology` and `agent-driver` therefore appear here — they ship BEHAVIOUR rather
  // than an `items[]` findings array, and their schemas describe the named top-level fields their
  // documented results carry, which is what stopped `validateResult` answering `unknown role` for a
  // role discovery had just handed the core.
  // BOS-1376 (2026-10-06): nine → ten; completion has its own validated result contract.
  assert.deepEqual(Object.keys(ROLE_SCHEMAS).sort(), [
    'agent-driver',
    'completion',
    'draft',
    'knowledge',
    'lens',
    'methodology',
    'notes',
    'plan-reviewer',
    'round',
    'surface',
  ])
  assert.deepEqual(ROLE_SCHEMAS.notes, ['tag', 'body', 'noteId'])
  assert.deepEqual(ROLE_SCHEMAS.knowledge, ['path', 'title', 'kind'])
  assert.deepEqual(ROLE_SCHEMAS.draft, ['planPath'])
})

test('repo-authored notes extensions are discoverable for each terminal core', () => {
  const root = path.resolve(import.meta.dirname, '..')
  const cores = ['boss-build', 'boss-plan', 'boss-review', 'boss-epic', 'boss-repair']

  for (const core of cores) {
    const { extensions, skipped } = discoverExtensions({ core, root, role: 'notes' })
    const found = extensions.find((extension) => extension.name === `${core}-notes`)
    assert.ok(found, core)
    assert.equal(found.role, 'notes', core)
    assert.deepEqual(skipped, [], core)
  }
})

// BOS-851: the `knowledge` role. Unlike `notes` (post-terminal, one core each), a knowledge
// extension runs pre-PR and writes a file into the tree, so `path` is what proves the artifact
// was persisted — the structural analogue of the notes contract's `noteId`.

// The AC pins the *file* form of the validate CLI specifically, because that is the form the
// core's dispatch uses (`validate --role knowledge --file "<outPath>"`). Piping stdin would
// exercise a different read branch than the one that ships.
function validateEnvelopeFile(role, envelope) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'skill-ext-validate-'))
  const file = path.join(dir, 'result.json')
  fs.writeFileSync(file, JSON.stringify(envelope))
  return spawnSync(
    process.execPath,
    [
      path.join(import.meta.dirname, 'skill-extensions.mjs'),
      'validate',
      '--role',
      role,
      '--file',
      file,
    ],
    { encoding: 'utf8' },
  )
}

test('discoverExtensions recognizes a knowledge extension role', () => {
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
  assert.equal(extensions[0].role, 'knowledge')
  assert.equal(extensions[0].order, 40)
  assert.deepEqual(skipped, [])
})

test('validateResult accepts a well-formed knowledge envelope', () => {
  const envelope = {
    ok: true,
    extension: 'boss-build-knowledge',
    role: 'knowledge',
    items: [
      {
        path: 'docs/solutions/testing/ratchet-rebaseline.md',
        title: 'Re-baseline a byte ratchet from a measurement',
        kind: 'solution',
      },
    ],
  }
  assert.deepEqual(validateResult(envelope, 'knowledge'), { ok: true, errors: [] })
})

test('an empty knowledge items array is a legitimate success, not a failed dispatch', () => {
  // A run may genuinely produce nothing worth recording. That has to stay distinguishable from a
  // dispatch that failed — which reports `{ok:false}` and is rejected by the case below.
  const result = validateEnvelopeFile('knowledge', {
    ok: true,
    extension: 'boss-build-knowledge',
    role: 'knowledge',
    items: [],
  })
  assert.equal(result.status, 0)
  assert.deepEqual(JSON.parse(result.stdout), { ok: true, errors: [] })
})

test('validate --role knowledge --file exits 0 for a well-formed envelope', () => {
  const result = validateEnvelopeFile('knowledge', {
    ok: true,
    extension: 'boss-build-knowledge',
    role: 'knowledge',
    items: [{ path: 'CONCEPTS.md', title: 'Reviewed tip', kind: 'concept' }],
  })
  assert.equal(result.status, 0)
  assert.equal(result.stderr, '')
  assert.deepEqual(JSON.parse(result.stdout), { ok: true, errors: [] })
})

test('validate --role knowledge returns clean JSON for a missing path', () => {
  const result = validateEnvelopeFile('knowledge', {
    ok: true,
    extension: 'boss-build-knowledge',
    role: 'knowledge',
    items: [{ title: 'Re-baseline a byte ratchet', kind: 'solution' }],
  })

  assert.equal(result.status, 1)
  assert.doesNotMatch(result.stderr, /(?:^|\n)(?:Error:|\s+at )/)
  assert.deepEqual(JSON.parse(result.stdout), {
    ok: false,
    errors: ['item 0 missing "path"'],
  })
})

test('the widened item guard rejects an empty or whitespace-only knowledge path', () => {
  // Without the guard widening, `path: ""` satisfies the `in` check and an extension that wrote
  // no file at all would report a persisted artifact. `path` is the whole proof of persistence.
  for (const blank of ['', '   ']) {
    const result = validateEnvelopeFile('knowledge', {
      ok: true,
      extension: 'boss-build-knowledge',
      role: 'knowledge',
      items: [{ path: blank, title: 'Ghost artifact', kind: 'solution' }],
    })
    assert.equal(result.status, 1, JSON.stringify(blank))
    assert.deepEqual(
      JSON.parse(result.stdout).errors,
      ['item 0 "path" is not a non-empty string'],
      JSON.stringify(blank),
    )
  }
})

test('a handled-failure knowledge envelope is rejected with its own error text surfaced', () => {
  const result = validateEnvelopeFile('knowledge', {
    ok: false,
    extension: 'boss-build-knowledge',
    role: 'knowledge',
    items: [],
    notes: '',
    error: 'knowledge methodology skill unavailable',
  })

  assert.equal(result.status, 1)
  assert.deepEqual(JSON.parse(result.stdout).errors, [
    'extension reported failure (ok:false): knowledge methodology skill unavailable',
  ])
})

test('the repo-authored knowledge extension is discoverable for boss-build', () => {
  const root = path.resolve(import.meta.dirname, '..')
  const { extensions, skipped } = discoverExtensions({
    core: 'boss-build',
    root,
    role: 'knowledge',
  })
  const found = extensions.find((extension) => extension.name === 'boss-build-knowledge')
  assert.ok(found)
  assert.equal(found.role, 'knowledge')
  assert.deepEqual(skipped, [])
})

// ── Extension names in the docs must exist on disk (BOS-1341) ─────────────────────────────
//
// The contract used to hand-count and hand-list the repo-local extensions, and the list went
// stale against .claude/skills while every sibling rename conflicted on it. The list is gone; this
// ties every extension NAME the two docs still mention to a directory, so a rename that forgets a
// doc reds here instead of leaving a dangling name.

const EXTENSION_CORES = ['plan', 'build', 'review', 'epic', 'repair', 'finalize', 'verify', 'proof']
const EXTENSION_NAME = new RegExp(
  String.raw`(?<![\w-])boss-(${EXTENSION_CORES.join('|')})-([a-z0-9-]*)(?:\{([a-z0-9,-]*)\})?([a-z0-9-]*)`,
  'g',
)

// Names the docs use to illustrate the naming rule, which no extension is meant to carry.
const ILLUSTRATIVE_EXTENSION_NAMES = new Set(['boss-review-x', 'boss-review-helper'])

/** Every `boss-<core>-<suffix>` token in `markdown`, brace forms (`prefix-{a,b}`) expanded. */
function extensionNameTokens(markdown) {
  const names = []
  for (const [, core, head, alternatives, tail] of markdown.matchAll(EXTENSION_NAME)) {
    const suffixes =
      alternatives === undefined
        ? [`${head}${tail}`]
        : alternatives.split(',').map((alt) => `${head}${alt}${tail}`)
    for (const suffix of suffixes) {
      const trimmed = suffix.replace(/-+$/, '')
      if (trimmed !== '') names.push(`boss-${core}-${trimmed}`)
    }
  }
  return [...new Set(names)]
}

function missingExtensionNames(markdown, root) {
  return extensionNameTokens(markdown).filter(
    (name) =>
      !ILLUSTRATIVE_EXTENSION_NAMES.has(name) &&
      !fs.existsSync(path.join(root, '.claude', 'skills', name, 'SKILL.md')),
  )
}

test('extensionNameTokens expands brace forms and skips placeholder names', () => {
  assert.deepEqual(
    extensionNameTokens(
      'see `boss-proof-{docs,tui}`, `.claude/skills/boss-build-ce/`, `boss-plan-<reviewer>` ' +
        'and `x-boss-extension`; `boss-build-ce` again',
    ),
    ['boss-proof-docs', 'boss-proof-tui', 'boss-build-ce'],
  )
})

test('every extension name in the contract and the docs-site guide exists on disk', () => {
  const root = path.resolve(import.meta.dirname, '..')
  for (const doc of [
    path.join('docs', 'skills', 'extension-contract.md'),
    path.join('services', 'docs', 'docs', 'skills', 'extensions.md'),
  ]) {
    const markdown = fs.readFileSync(path.join(root, doc), 'utf8')
    assert.ok(extensionNameTokens(markdown).length > 0, `${doc} must still name an extension`)
    assert.deepEqual(missingExtensionNames(markdown, root), [], doc)
  }
})

test('a renamed extension still named in a doc is reported as missing', () => {
  const root = path.resolve(import.meta.dirname, '..')
  assert.deepEqual(
    missingExtensionNames('lens example: `boss-review-golang-renamed`, `boss-review-x`', root),
    ['boss-review-golang-renamed'],
  )
})

test('completion extensions are discoverable and validate conditional merge results', () => {
  const root = scratchRoot()
  writeSkill(root, 'boss-build-merge', [
    'name: boss-build-merge',
    'x-boss-extension:',
    '  extends: boss-build',
    '  role: completion',
  ])
  assert.equal(
    discoverExtensions({ core: 'boss-build', root, role: 'completion' }).extensions.length,
    1,
  )
  const merged = {
    ok: true,
    extension: 'boss-build-merge',
    role: 'completion',
    action: 'merged',
    reason: 'eligible',
    mergeSha: 'a'.repeat(40),
  }
  const skipped = { ...merged, action: 'skipped', reason: 'not-opted-in', mergeSha: '' }
  for (const result of [merged, skipped])
    assert.equal(validateResult(result, 'completion').ok, true)
  for (const result of [
    { ...merged, mergeSha: '' },
    { ...merged, mergeSha: 'bad' },
    { ...skipped, mergeSha: merged.mergeSha },
    { ...merged, action: 'other' },
    { ...skipped, reason: '' },
    { ...skipped, ok: false },
  ])
    assert.equal(validateResult(result, 'completion').ok, false, JSON.stringify(result))
  const file = path.join(root, 'result.json')
  fs.writeFileSync(file, JSON.stringify(merged))
  const cli = spawnSync(
    process.execPath,
    [
      path.join(import.meta.dirname, 'skill-extensions.mjs'),
      'validate',
      '--role',
      'completion',
      '--file',
      file,
    ],
    { encoding: 'utf8' },
  )
  assert.equal(cli.status, 0, cli.stderr)
  assert.equal(JSON.parse(cli.stdout).ok, true)
})
