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

test('repo-authored notes extensions report recording failures as unsuccessful envelopes', () => {
  const root = path.resolve(import.meta.dirname, '..')
  for (const core of ['boss-build', 'boss-plan', 'boss-review', 'boss-epic', 'boss-repair']) {
    const skill = fs.readFileSync(
      path.join(root, '.claude', 'skills', `${core}-notes`, 'SKILL.md'),
      'utf8',
    )
    assert.match(skill, /"ok": false/, core)
    assert.match(skill, /"items": \[\]/, core)
    assert.match(skill, /"error": "<reason>"/, core)
  }
})

// BOS-674: a core's step-by-step spine is not always one file. boss-build's Steps 8-12 —
// including the Step 12 post-terminal notes dispatch — were extracted into
// references/finalize-and-stop.md, so reading SKILL.md alone reports the notes contract as
// deleted when it merely moved. The list is EXPLICIT rather than a walk of references/, so a
// clause deleted outright still turns this red. Mirrors `coreBodyFiles` in
// services/boss/internal/skillinstall/skills_manifest_test.go.
const CORE_BODY_FILES = {
  'boss-build': ['SKILL.md', 'references/finalize-and-stop.md'],
}

const publishedSpine = (root, core) =>
  (CORE_BODY_FILES[core] ?? ['SKILL.md'])
    .map((rel) =>
      fs.readFileSync(
        path.join(root, 'services', 'boss', 'internal', 'skillinstall', 'skills', core, rel),
        'utf8',
      ),
    )
    .join('\n')

test('fresh notes workers receive only a bounded completed-run observation artifact', () => {
  const root = path.resolve(import.meta.dirname, '..')
  const contract = fs.readFileSync(
    path.join(root, 'docs', 'skills', 'extension-contract.md'),
    'utf8',
  )

  assert.match(
    contract,
    /observationPath/,
    'the extension contract doc no longer documents the key the cores send',
  )

  for (const core of ['boss-build', 'boss-plan', 'boss-review', 'boss-epic', 'boss-repair']) {
    const published = publishedSpine(root, core)
    const extension = fs.readFileSync(
      path.join(root, '.claude', 'skills', `${core}-notes`, 'SKILL.md'),
      'utf8',
    )

    assert.match(published, /at\s+most\s+five\s+secret-scrubbed\s+candidate\s+observations/, core)
    assert.match(published, /maximum\s+8\s+KiB/, core)
    assert.match(published, /"?observationPath"?:\s*"<NOTES_OBSERVATIONS>"/, core)
    assert.match(extension, /context\.observationPath/, core)
    assert.match(extension, /only\s+completed-run\s+observation\s+source/, core)
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

test('the repo-authored knowledge extension reports failures as unsuccessful envelopes', () => {
  const root = path.resolve(import.meta.dirname, '..')
  const skill = fs.readFileSync(
    path.join(root, '.claude', 'skills', 'boss-build-knowledge', 'SKILL.md'),
    'utf8',
  )
  assert.match(skill, /"ok": false/)
  assert.match(skill, /"items": \[\]/)
  assert.match(skill, /"error": "<reason>"/)
})
