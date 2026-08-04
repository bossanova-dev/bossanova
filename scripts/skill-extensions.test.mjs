import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import { ROLE_SCHEMAS, discoverExtensions, validateResult } from './skill-extensions.mjs'

function scratchRoot() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'skill-ext-'))
}

function writeSkill(root, name, frontmatterLines) {
  const dir = path.join(root, '.claude', 'skills', name)
  fs.mkdirSync(dir, { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'SKILL.md'),
    ['---', ...frontmatterLines, '---', '', `# ${name}`, ''].join('\n'),
  )
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
  assert.deepEqual(Object.keys(ROLE_SCHEMAS).sort(), [
    'lens',
    'notes',
    'plan-reviewer',
    'round',
    'surface',
  ])
  assert.deepEqual(ROLE_SCHEMAS.notes, ['tag', 'body', 'noteId'])
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
  for (const core of ['boss-build', 'boss-plan', 'boss-review', 'boss-epic', 'boss-repair']) {
    const published = publishedSpine(root, core)
    const extension = fs.readFileSync(
      path.join(root, '.claude', 'skills', `${core}-notes`, 'SKILL.md'),
      'utf8',
    )

    assert.match(published, /at most five\s+secret-scrubbed candidate observations/, core)
    assert.match(published, /maximum 8 KiB/, core)
    assert.match(published, /"?observationPath"?:\s*"<NOTES_OBSERVATIONS>"/, core)
    assert.match(extension, /context\.observationPath/, core)
    assert.match(extension, /only completed-run\s+observation source/, core)
  }
})
