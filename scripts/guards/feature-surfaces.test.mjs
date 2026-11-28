import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { HOSTED_TOOL_SOURCE_FILES, TOOL_SOURCE_FILES } from '../skills/mcp-tool-registry.mjs'
import { formatBaseline } from './baseline.mjs'
import { LEGACY_CEILING } from './feature-map.mjs'
import { BASELINE, REFERENCES_DIR } from './feature-surfaces.mjs'

const cli = fileURLToPath(new URL('./feature-surfaces.mjs', import.meta.url))
const ALL_MCP_SOURCES = [...TOOL_SOURCE_FILES, ...HOSTED_TOOL_SOURCE_FILES]

const README = `---
uiRoots: []
userFacingRoots: [services/boss/api/**]
ignore: ['**/*_test.go']
---

# Features
`

const DEFAULT_NOTES_REF = `# Notes commands

### \`boss notes\`

Manage notes.

### \`boss notes add <body> [flags]\`

Add a note.

### \`boss notes ls [flags]\`

List notes.

\`\`\`markdown
### \`boss notes fenced-fake\`
\`\`\`
`

const DEFAULT_VIEWER_REF = `# Viewer commands

### \`boss viewer approve <id> [flags]\`

Approve a viewer.

### \`boss viewer list [flags]\`

List viewers.
`

const DEFAULT_CALLBACK_REF = `# Callback commands

### \`boss callback remove (<callback-id> | --all) [flags]\`

Remove a callback.
`

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'feature-surfaces-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))

  const put = (file, text = '') => {
    const abs = path.join(root, file)
    fs.mkdirSync(path.dirname(abs), { recursive: true })
    fs.writeFileSync(abs, text)
  }

  const ceilings = new Set()
  const putFeature = (file, frontmatter, id) => {
    put(
      `docs/features/${file}`,
      `---\nid: ${id}\ntitle: ${id}\nowns: [services/boss/api/cli/${id}/**]\n${frontmatter}---\n\n- AC1: Works.\n`,
    )
    ceilings.add(`${id}.AC1`)
    put(LEGACY_CEILING, formatBaseline(ceilings))
  }

  put('docs/features/README.md', README)
  put(BASELINE, '[]\n')
  put(LEGACY_CEILING, '[]\n')
  put(`${REFERENCES_DIR}/notes.md`, DEFAULT_NOTES_REF)
  put(`${REFERENCES_DIR}/viewer.md`, DEFAULT_VIEWER_REF)
  put(`${REFERENCES_DIR}/callback.md`, DEFAULT_CALLBACK_REF)

  for (const rel of ALL_MCP_SOURCES) put(rel, 'package bossmcp\n')
  put(
    TOOL_SOURCE_FILES[0],
    'package bossmcp\nvar _ = []Tool{{Name: "list_notes"}, {Name: "create_note"}}\n',
  )
  put(HOSTED_TOOL_SOURCE_FILES[0], 'package bossmcp\nvar _ = []Tool{{Name: "create_trigger"}}\n')

  const run = (...args) =>
    spawnSync(process.execPath, [cli, ...args, '--root', root], { encoding: 'utf8' })

  const pass = (result) => assert.equal(result.status, 0, result.stdout + result.stderr)

  return { root, put, putFeature, run, pass }
}

test('web-only feature without CLI/MCP names or exemption fails with needs cli and needs mcp', (t) => {
  const f = fixture(t)
  f.putFeature('webonly.md', 'surfaces: [web]\n', 'webonly')
  const result = f.run()
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('webonly needs cli'), result.stderr)
  assert.ok(result.stderr.includes('webonly needs mcp'), result.stderr)
})

test('feature naming a valid CLI command passes and renaming the reference heading fails', (t) => {
  const f = fixture(t)
  f.putFeature(
    'notes.md',
    'surfaces: [web, cli, mcp]\ncli:\n  - boss notes add\n  - boss callback remove\nmcp: [list_notes]\n',
    'notes',
  )
  const ok = f.run()
  f.pass(ok)
  assert.ok(ok.stdout.includes('1 features checked'), ok.stdout)
  assert.ok(ok.stdout.includes('CLI commands resolved'), ok.stdout)
  assert.ok(ok.stdout.includes('MCP tools resolved'), ok.stdout)

  f.putFeature(
    'notes.md',
    'surfaces: [web, cli, mcp]\ncli:\n  - boss notes fenced-fake\nmcp: [list_notes]\n',
    'notes',
  )
  const fenced = f.run()
  assert.equal(fenced.status, 1)
  assert.ok(fenced.stderr.includes('docs/features/notes.md'), fenced.stderr)
  assert.ok(fenced.stderr.includes('boss notes fenced-fake'), fenced.stderr)

  f.putFeature(
    'notes.md',
    'surfaces: [web, cli, mcp]\ncli:\n  - boss notes add\nmcp: [list_notes]\n',
    'notes',
  )
  f.put(
    `${REFERENCES_DIR}/notes.md`,
    DEFAULT_NOTES_REF.replace('boss notes add <body>', 'boss notes create <body>'),
  )
  const renamed = f.run()
  assert.equal(renamed.status, 1)
  assert.ok(renamed.stderr.includes('docs/features/notes.md'), renamed.stderr)
  assert.ok(renamed.stderr.includes('boss notes add'), renamed.stderr)
})

test('feature naming a valid MCP tool passes and removing the tool from the registry fails', (t) => {
  const f = fixture(t)
  f.putFeature(
    'notes.md',
    'surfaces: [web, cli, mcp]\ncli:\n  - boss notes add\nmcp: [list_notes, create_trigger]\n',
    'notes',
  )
  f.pass(f.run())

  f.put(TOOL_SOURCE_FILES[0], 'package bossmcp\nvar _ = []Tool{{Name: "create_note"}}\n')
  const removed = f.run()
  assert.equal(removed.status, 1)
  assert.ok(removed.stderr.includes('docs/features/notes.md'), removed.stderr)
  assert.ok(removed.stderr.includes('list_notes'), removed.stderr)
})

test('whole-surface exemption passes, missing reviewed fails as fatal loader warning, and exempting a declared or named surface fails', (t) => {
  const f = fixture(t)
  f.putFeature(
    'notes.md',
    [
      'surfaces: [web, cli]',
      'cli:',
      '  - boss notes add',
      'exemptions:',
      "  mcp: { exempt: 'No MCP tool applies.', reviewed: BOS-2682 }",
      '',
    ].join('\n'),
    'notes',
  )
  f.pass(f.run())

  f.putFeature(
    'notes.md',
    [
      'surfaces: [web, cli]',
      'cli:',
      '  - boss notes add',
      'exemptions:',
      "  mcp: { exempt: 'No MCP tool applies.' }",
      '',
    ].join('\n'),
    'notes',
  )
  const missingReviewed = f.run()
  assert.equal(missingReviewed.status, 1)
  assert.ok(missingReviewed.stderr.includes('bad-surface-declaration'), missingReviewed.stderr)
  assert.ok(missingReviewed.stderr.includes('docs/features/notes.md'), missingReviewed.stderr)

  f.putFeature(
    'notes.md',
    [
      'surfaces: [web, cli, mcp]',
      'cli:',
      '  - boss notes add',
      'mcp: [list_notes]',
      'exemptions:',
      "  mcp: { exempt: 'Conflict with declared surface.', reviewed: BOS-2682 }",
      '',
    ].join('\n'),
    'notes',
  )
  const declaredConflict = f.run()
  assert.equal(declaredConflict.status, 1)
  assert.ok(declaredConflict.stderr.includes('docs/features/notes.md'), declaredConflict.stderr)
  assert.ok(declaredConflict.stderr.includes('exemptions.mcp'), declaredConflict.stderr)

  f.putFeature(
    'notes.md',
    [
      'surfaces: [web, cli]',
      'cli:',
      '  - boss notes add',
      'mcp: [list_notes]',
      'exemptions:',
      "  mcp: { exempt: 'Conflict with named tool.', reviewed: BOS-2682 }",
      '',
    ].join('\n'),
    'notes',
  )
  const namedConflict = f.run()
  assert.equal(namedConflict.status, 1)
  assert.ok(namedConflict.stderr.includes('docs/features/notes.md'), namedConflict.stderr)
  assert.ok(namedConflict.stderr.includes('exemptions.mcp'), namedConflict.stderr)
})

test('named surface list without that surface declared fails', (t) => {
  const f = fixture(t)
  f.putFeature(
    'notes.md',
    'surfaces: [web, mcp]\ncli:\n  - boss notes add\nmcp: [list_notes]\n',
    'notes',
  )
  const result = f.run()
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('docs/features/notes.md'), result.stderr)
  assert.ok(result.stderr.includes('cli'), result.stderr)
})

test('withheld entries validate surface and require command in the feature cli list', (t) => {
  const f = fixture(t)
  f.putFeature(
    'viewer.md',
    [
      'surfaces: [web, cli, mcp]',
      'cli:',
      '  - boss viewer approve',
      '  - boss viewer list',
      'mcp: [list_notes]',
      'withheld:',
      '  viewer-approve:',
      '    surface: mcp',
      '    command: boss viewer approve',
      "    exempt: 'Device approval stays with a human holding a device key.'",
      '    reviewed: BOS-2666',
      '',
    ].join('\n'),
    'viewer',
  )
  f.pass(f.run())

  f.putFeature(
    'viewer.md',
    [
      'surfaces: [web, cli, mcp]',
      'cli:',
      '  - boss viewer list',
      'mcp: [list_notes]',
      'withheld:',
      '  viewer-approve:',
      '    surface: mcp',
      '    command: boss viewer approve',
      "    exempt: 'Device approval stays with a human holding a device key.'",
      '    reviewed: BOS-2666',
      '',
    ].join('\n'),
    'viewer',
  )
  const notInCli = f.run()
  assert.equal(notInCli.status, 1)
  assert.ok(notInCli.stderr.includes('docs/features/viewer.md'), notInCli.stderr)
  assert.ok(notInCli.stderr.includes('boss viewer approve'), notInCli.stderr)

  f.putFeature(
    'viewer.md',
    [
      'surfaces: [web, cli, mcp]',
      'cli:',
      '  - boss viewer approve',
      'mcp: [list_notes]',
      'withheld:',
      '  viewer-approve:',
      '    surface: web',
      '    command: boss viewer approve',
      "    exempt: 'Wrong surface value.'",
      '    reviewed: BOS-2666',
      '',
    ].join('\n'),
    'viewer',
  )
  const badSurface = f.run()
  assert.equal(badSurface.status, 1)
  assert.ok(badSurface.stderr.includes('docs/features/viewer.md'), badSurface.stderr)
  assert.ok(badSurface.stderr.includes('withheld.viewer-approve'), badSurface.stderr)
})

test('macos and ios name lists are rejected while bare surfaces and exemptions pass', (t) => {
  const f = fixture(t)
  f.putFeature(
    'notes.md',
    'surfaces: [web, cli, mcp, macos, ios]\ncli:\n  - boss notes add\nmcp: [list_notes]\n',
    'notes',
  )
  f.pass(f.run())

  f.putFeature(
    'notes.md',
    [
      'surfaces: [web, cli, mcp]',
      'cli:',
      '  - boss notes add',
      'mcp: [list_notes]',
      'exemptions:',
      "  macos: { exempt: 'Not applicable on macOS.', reviewed: BOS-2682 }",
      '',
    ].join('\n'),
    'notes',
  )
  f.pass(f.run())

  f.putFeature(
    'notes.md',
    'surfaces: [web, cli, mcp, macos]\ncli:\n  - boss notes add\nmcp: [list_notes]\nmacos: [NotesWindow]\n',
    'notes',
  )
  const macosNamed = f.run()
  assert.equal(macosNamed.status, 1)
  assert.ok(macosNamed.stderr.includes('docs/features/notes.md'), macosNamed.stderr)
  assert.ok(macosNamed.stderr.includes('macos'), macosNamed.stderr)

  f.putFeature(
    'notes.md',
    'surfaces: [web, cli, mcp, ios]\ncli:\n  - boss notes add\nmcp: [list_notes]\nios: [NotesScreen]\n',
    'notes',
  )
  const iosNamed = f.run()
  assert.equal(iosNamed.status, 1)
  assert.ok(iosNamed.stderr.includes('docs/features/notes.md'), iosNamed.stderr)
  assert.ok(iosNamed.stderr.includes('ios'), iosNamed.stderr)
})

test('baseline rejects stale entries and new offenders, and --update writes sorted shared baseline', (t) => {
  const f = fixture(t)
  f.putFeature('zebra.md', 'surfaces: [web]\n', 'zebra')
  f.putFeature('alpha.md', 'surfaces: [web]\n', 'alpha')

  f.pass(f.run('--update'))
  const expected = ['alpha needs cli', 'alpha needs mcp', 'zebra needs cli', 'zebra needs mcp']
  assert.equal(fs.readFileSync(path.join(f.root, BASELINE), 'utf8'), formatBaseline(expected))
  f.pass(f.run())
  f.pass(f.run('--check'))

  f.putFeature('alpha.md', 'surfaces: [web, cli]\ncli:\n  - boss notes add\n', 'alpha')
  const stale = f.run()
  assert.equal(stale.status, 1)
  assert.ok(stale.stderr.includes('alpha needs cli'), stale.stderr)
  assert.ok(stale.stderr.includes('no longer occurs'), stale.stderr)

  f.pass(f.run('--update'))
  f.putFeature('beta.md', 'surfaces: [web]\n', 'beta')
  const added = f.run()
  assert.equal(added.status, 1)
  assert.ok(added.stderr.includes('beta needs cli'), added.stderr)
  assert.ok(added.stderr.includes('beta needs mcp'), added.stderr)

  const beforeHardFail = fs.readFileSync(path.join(f.root, BASELINE), 'utf8')
  f.putFeature('beta.md', 'surfaces: [web, cli]\ncli:\n  - boss no-such-cmd\n', 'beta')
  const hardFailUpdate = f.run('--update')
  assert.equal(hardFailUpdate.status, 1)
  assert.ok(hardFailUpdate.stderr.includes('boss no-such-cmd'), hardFailUpdate.stderr)
  assert.equal(fs.readFileSync(path.join(f.root, BASELINE), 'utf8'), beforeHardFail)
})

test('tripwires fail on missing or empty references dir and missing or empty MCP tool sources', (t) => {
  {
    const f = fixture(t)
    f.putFeature(
      'notes.md',
      'surfaces: [web, cli, mcp]\ncli:\n  - boss notes add\nmcp: [list_notes]\n',
      'notes',
    )
    fs.rmSync(path.join(f.root, REFERENCES_DIR), {
      recursive: true,
      force: true,
      maxRetries: 5,
      retryDelay: 100,
    })
    const res = f.run()
    assert.equal(res.status, 1)
    assert.ok(res.stderr.includes(REFERENCES_DIR), res.stderr)
  }

  {
    const f = fixture(t)
    f.putFeature(
      'notes.md',
      'surfaces: [web, cli, mcp]\ncli:\n  - boss notes add\nmcp: [list_notes]\n',
      'notes',
    )
    fs.rmSync(path.join(f.root, REFERENCES_DIR), {
      recursive: true,
      force: true,
      maxRetries: 5,
      retryDelay: 100,
    })
    fs.mkdirSync(path.join(f.root, REFERENCES_DIR), { recursive: true })
    const res = f.run()
    assert.equal(res.status, 1)
    assert.ok(res.stderr.includes(REFERENCES_DIR), res.stderr)
  }

  {
    const f = fixture(t)
    f.putFeature(
      'notes.md',
      'surfaces: [web, cli, mcp]\ncli:\n  - boss notes add\nmcp: [list_notes]\n',
      'notes',
    )
    fs.unlinkSync(path.join(f.root, TOOL_SOURCE_FILES[1]))
    const res = f.run()
    assert.equal(res.status, 1)
    assert.ok(res.stderr.includes(TOOL_SOURCE_FILES[1]), res.stderr)
  }

  {
    const f = fixture(t)
    f.putFeature(
      'notes.md',
      'surfaces: [web, cli, mcp]\ncli:\n  - boss notes add\nmcp: [list_notes]\n',
      'notes',
    )
    for (const rel of ALL_MCP_SOURCES) f.put(rel, 'package bossmcp\n')
    const res = f.run()
    assert.equal(res.status, 1)
    assert.ok(res.stderr.toLowerCase().includes('zero'), res.stderr)
  }
})

test('CLI rejects unknown flags, positionals, and conflicting modes', (t) => {
  const f = fixture(t)
  for (const args of [['--check', '--update'], ['--unknown'], ['extra']]) {
    const res = f.run(...args)
    assert.notEqual(res.status, 0, `expected failure for ${args.join(' ')}`)
  }
})
