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
