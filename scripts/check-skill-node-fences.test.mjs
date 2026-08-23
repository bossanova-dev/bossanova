#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  SKILL_ROOTS,
  checkSkillNodeFencesInRepo,
  extractNodeHeredocsFromShell,
} from './check-skill-node-fences.mjs'

const md = (...lines) => `${lines.join('\n')}\n`

function makeRepo(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'skill-node-fences-'))
  for (const [rel, contents] of Object.entries(files)) {
    const full = path.join(root, rel)
    fs.mkdirSync(path.dirname(full), { recursive: true })
    fs.writeFileSync(full, contents)
  }
  return root
}

const skill = (name) => path.join('.claude', 'skills', name, 'SKILL.md')

test('well-formed quoted NODE heredoc has no finding', async () => {
  const root = makeRepo({
    [skill('valid')]: md('```bash', "node <<'NODE'", 'console.log("ok")', 'NODE', '```'),
  })
  try {
    assert.deepEqual(await checkSkillNodeFencesInRepo(root), [])
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test('unbalanced paren reports one SKILL coordinate with node syntax text', async () => {
  const root = makeRepo({
    [skill('invalid')]: md('intro', '```bash', "node <<'NODE'", 'console.log(', 'NODE', '```'),
  })
  try {
    const findings = await checkSkillNodeFencesInRepo(root)
    assert.equal(findings.length, 1)
    assert.equal(findings[0].file, skill('invalid'))
    assert.equal(findings[0].line, 3)
    assert.match(findings[0].message, /SyntaxError|Unexpected\s+end\s+of\s+input/)
    assert.doesNotMatch(findings[0].message, /check-skill-node-fences-|node-heredoc-/)
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test('NODE delimiter on a cat command is ignored', async () => {
  const root = makeRepo({
    [skill('cat')]: md('```bash', "cat <<'NODE'", 'console.log(', 'NODE', '```'),
  })
  try {
    assert.deepEqual(await checkSkillNodeFencesInRepo(root), [])
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test('env and assignment prefixes still identify node commands', () => {
  const heredocs = extractNodeHeredocsFromShell(
    [
      "BOSS_BUILD_TOOLBOX=tools node --input-type=module <<'NODE_SCRIPT'",
      'console.log("a")',
      'NODE_SCRIPT',
      "env -u NODE_OPTIONS NODE_ENV=test node <<'MJS'",
      'console.log("b")',
      'MJS',
    ].join('\n'),
  )
  assert.deepEqual(
    heredocs.map((h) => ({ lineOffset: h.lineOffset, delimiter: h.delimiter })),
    [
      { lineOffset: 0, delimiter: 'NODE_SCRIPT' },
      { lineOffset: 3, delimiter: 'MJS' },
    ],
  )
})

test('node heredocs inside assignment command substitutions are extracted', () => {
  const heredocs = extractNodeHeredocsFromShell(
    [
      'RESULT="$(node --input-type=module - "$PAYLOAD" <<\'NODE\'',
      'const input = JSON.parse(process.argv[2])',
      'console.log(input.ok)',
      'NODE',
      ')"',
    ].join('\n'),
  )
  assert.deepEqual(
    heredocs.map((h) => ({ lineOffset: h.lineOffset, body: h.body })),
    [
      {
        lineOffset: 0,
        body: ['const input = JSON.parse(process.argv[2])', 'console.log(input.ok)'].join('\n'),
      },
    ],
  )
})

test('unquoted and tab-stripping node heredocs are extracted', () => {
  const heredocs = extractNodeHeredocsFromShell(
    [
      'node <<NODE',
      'console.log("a")',
      'NODE',
      "node <<-'NODE'",
      '\tconsole.log("b")',
      '\tNODE',
    ].join('\n'),
  )
  assert.deepEqual(
    heredocs.map((h) => ({ lineOffset: h.lineOffset, body: h.body })),
    [
      { lineOffset: 0, body: 'console.log("a")' },
      { lineOffset: 3, body: '\tconsole.log("b")' },
    ],
  )
})

test('blockquote and list-indented fences produce correct opener line numbers', async () => {
  const root = makeRepo({
    [skill('containers')]: md(
      '> ```bash',
      "> node <<'NODE'",
      '> console.log(',
      '> NODE',
      '> ```',
      '',
      '- item',
      '  ```bash',
      "  node <<'JS'",
      '  console.log(',
      '  JS',
      '  ```',
    ),
  })
  try {
    const findings = await checkSkillNodeFencesInRepo(root)
    assert.deepEqual(
      findings.map(({ file, line }) => ({ file, line })),
      [
        { file: skill('containers'), line: 2 },
        { file: skill('containers'), line: 9 },
      ],
    )
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test('non-shell fences are skipped', async () => {
  const root = makeRepo({
    [skill('js-fence')]: md('```js', "node <<'NODE'", 'console.log(', 'NODE', '```'),
  })
  try {
    assert.deepEqual(await checkSkillNodeFencesInRepo(root), [])
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test('two heredocs in one fence are both checked', async () => {
  const root = makeRepo({
    [skill('two')]: md(
      '```bash',
      "node <<'NODE'",
      'console.log(',
      'NODE',
      "node <<'MJS'",
      'if (',
      'MJS',
      '```',
    ),
  })
  try {
    const findings = await checkSkillNodeFencesInRepo(root)
    assert.equal(findings.length, 2)
    assert.deepEqual(
      findings.map((finding) => finding.line),
      [2, 5],
    )
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test('real skill roots pass node heredoc syntax check', async () => {
  const repoRoot = path.join(path.dirname(new URL(import.meta.url).pathname), '..')
  let stats = null
  assert.deepEqual(await checkSkillNodeFencesInRepo(repoRoot, { onStats: (s) => (stats = s) }), [])
  assert.equal(stats.checked, 4)
})

test('skill roots exclude generated codex mirrors', () => {
  assert.ok(!SKILL_ROOTS.some((root) => root.includes('.codex')))
})
