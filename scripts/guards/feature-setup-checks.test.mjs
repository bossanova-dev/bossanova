import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { formatBaseline } from './baseline.mjs'
import { setupCheckFailures } from './feature-setup-checks.mjs'

const CLI = fileURLToPath(new URL('./feature-setup-checks.mjs', import.meta.url))
const IDS_PATH = 'services/bossd/internal/setupcheck/ids.json'
const BASELINE_PATH = 'scripts/guards/feature-setup-checks-baseline.json'

const DEFAULT_IDS = ['daemon-running', 'repo-registered', 'gh-auth']
const DEFAULT_README = `---
uiRoots: []
userFacingRoots: [services/boss/api/**]
ignore: ['**/*_test.go']
---

# Features
`

function fixture(t, { ids = DEFAULT_IDS, baseline = [], files = {} } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'feature-setup-checks-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))

  const git = (...args) => {
    const result = spawnSync('git', args, { cwd: root, encoding: 'utf8' })
    assert.equal(result.status, 0, result.stdout + result.stderr)
  }

  git('init', '-q')
  git('config', 'commit.gpgsign', 'false')
  git('config', 'user.name', 'Test User')
  git('config', 'user.email', 'test@example.com')

  const writeFile = (relPath, content) => {
    const abs = path.join(root, relPath)
    fs.mkdirSync(path.dirname(abs), { recursive: true })
    fs.writeFileSync(abs, content)
  }

  writeFile('docs/features/README.md', DEFAULT_README)
  if (ids !== null) {
    writeFile(
      IDS_PATH,
      typeof ids === 'string'
        ? ids
        : JSON.stringify({ generatedBy: 'make setup-check-ids', ids }, null, 2) + '\n',
    )
  }
  writeFile(BASELINE_PATH, formatBaseline(baseline))

  for (const [relPath, content] of Object.entries(files)) {
    writeFile(relPath, content)
  }

  git('add', '-A')
  git('commit', '-q', '-m', 'init')

  const runCli = (args = ['--check']) =>
    spawnSync(process.execPath, [CLI, '--root', root, ...args], { encoding: 'utf8' })

  return { root, runCli }
}

function featureDoc(id, extraFrontmatter = '') {
  const extra = extraFrontmatter ? `${extraFrontmatter.trimEnd()}\n` : ''
  return `---\nid: ${id}\ntitle: ${id}\nsurfaces: [cli]\n${extra}owns: [services/boss/api/cli/${id}/**]\n---\n\n- AC1: Works.\n`
}

test('feature-setup-checks rejects a feature that declares neither setup checks nor an exemption', (t) => {
  const { runCli } = fixture(t, {
    files: {
      'docs/features/sample.md': featureDoc('sample'),
    },
  })
  const result = runCli()
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('docs/features/sample.md: setup-undeclared:'), result.stderr)
})

test('feature-setup-checks rejects an unknown setup check id', (t) => {
  const { runCli } = fixture(t, {
    files: {
      'docs/features/sample.md': featureDoc('sample', 'setupChecks: [no-such-check]'),
    },
  })
  const result = runCli()
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('docs/features/sample.md: setup-unknown-id:'), result.stderr)
  assert.ok(result.stderr.includes('no-such-check'), result.stderr)
})

test('feature-setup-checks rejects a feature declaring both setupChecks and setupExempt', (t) => {
  const { runCli } = fixture(t, {
    files: {
      'docs/features/sample.md': featureDoc(
        'sample',
        "setupChecks: [gh-auth]\nsetupExempt: 'also exempt'",
      ),
    },
  })
  const result = runCli()
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('docs/features/sample.md: setup-both-declared:'), result.stderr)
})

test('feature-setup-checks rejects an empty setupChecks list in inline and block forms', (t) => {
  for (const field of ['setupChecks: []', 'setupChecks:']) {
    const { runCli } = fixture(t, {
      files: {
        'docs/features/sample.md': featureDoc('sample', field),
      },
    })
    const result = runCli()
    assert.equal(result.status, 1)
    assert.ok(result.stderr.includes('docs/features/sample.md: setup-empty-list:'), result.stderr)
  }
})

test('feature-setup-checks rejects duplicate ids in setupChecks', (t) => {
  const { runCli } = fixture(t, {
    files: {
      'docs/features/sample.md': featureDoc(
        'sample',
        'setupChecks: [daemon-running, daemon-running]',
      ),
    },
  })
  const result = runCli()
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('docs/features/sample.md: setup-duplicate-id:'), result.stderr)
  assert.ok(result.stderr.includes('daemon-running'), result.stderr)
})

test('feature-setup-checks rejects a blank or empty setupExempt reason', (t) => {
  for (const field of ["setupExempt: ''", 'setupExempt:']) {
    const { runCli } = fixture(t, {
      files: {
        'docs/features/sample.md': featureDoc('sample', field),
      },
    })
    const result = runCli()
    assert.equal(result.status, 1)
    assert.ok(
      result.stderr.includes('docs/features/sample.md: setup-exempt-reason:'),
      result.stderr,
    )
  }
})

test('feature-setup-checks rejects multiline and non-scalar YAML setupExempt declarations', (t) => {
  const invalidFields = [
    'setupExempt: |\n  line one\n  line two',
    'setupExempt: >-\n  line one\n  line two',
    'setupExempt: line one\n  line two',
    'setupExempt: line one\n\n  line two',
    'setupExempt: "line one\n  line two"',
    "setupExempt: 'line one\nline two'",
    'setupExempt: { reason: mapped }',
    "setupExempt: 'first'\nsetupExempt: 'second'",
  ]
  for (const field of invalidFields) {
    const { runCli } = fixture(t, {
      files: {
        'docs/features/sample.md': featureDoc('sample', field),
      },
    })
    const result = runCli()
    assert.equal(result.status, 1, `expected failure for:\n${field}`)
    assert.ok(
      result.stderr.includes('docs/features/sample.md: setup-exempt-reason:'),
      `expected setup-exempt-reason for:\n${field}\ngot:\n${result.stderr}`,
    )
  }
})

test('feature-setup-checks accepts valid single-line unquoted and quoted setupExempt forms', (t) => {
  const { runCli } = fixture(t, {
    files: {
      'docs/features/plain.md': featureDoc('plain', 'setupExempt: plain single-line reason'),
      'docs/features/double.md': featureDoc('double', 'setupExempt: "double-quoted reason"'),
      'docs/features/pipe-quoted.md': featureDoc(
        'pipe-quoted',
        "setupExempt: '| literal pipe inside quotes'",
      ),
    },
  })
  const result = runCli()
  assert.equal(result.status, 0, result.stdout + result.stderr)
})

test('feature-setup-checks fails with setup-check-ids-unreadable when ids.json is missing or invalid', (t) => {
  const badCases = [null, '{not-json', [], ['daemon-running', 'daemon-running']]
  for (const ids of badCases) {
    const { runCli } = fixture(t, {
      ids,
      files: {
        'docs/features/sample.md': featureDoc('sample', 'setupChecks: [daemon-running]'),
      },
    })
    const result = runCli()
    assert.equal(result.status, 1)
    assert.ok(result.stderr.includes('setup-check-ids-unreadable'), result.stderr)
  }
})

test('feature-setup-checks accepts a baselined feature with no declaration', (t) => {
  const { runCli } = fixture(t, {
    baseline: ['pending-feature — waits for check'],
    files: {
      'docs/features/pending-feature.md': featureDoc('pending-feature'),
    },
  })
  const result = runCli()
  assert.equal(result.status, 0, result.stdout + result.stderr)
})

test('feature-setup-checks fails as stale when a baselined feature now declares', (t) => {
  for (const declaration of ['setupChecks: [gh-auth]', "setupExempt: 'covered elsewhere'"]) {
    const { runCli } = fixture(t, {
      baseline: ['pending-feature — waits for check'],
      files: {
        'docs/features/pending-feature.md': featureDoc('pending-feature', declaration),
      },
    })
    const result = runCli()
    assert.equal(result.status, 1)
    assert.ok(result.stderr.includes(BASELINE_PATH), result.stderr)
    assert.ok(result.stderr.includes('pending-feature'), result.stderr)
  }
})

test('feature-setup-checks fails as stale when a baseline entry names no feature', (t) => {
  const { runCli } = fixture(t, {
    baseline: ['ghost-feature — removed feature'],
    files: {
      'docs/features/sample.md': featureDoc('sample', 'setupChecks: [gh-auth]'),
    },
  })
  const result = runCli()
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes(BASELINE_PATH), result.stderr)
  assert.ok(result.stderr.includes('ghost-feature'), result.stderr)
})

test('feature-setup-checks skips README.md with no id and accepts valid inline, block and exempt declarations', (t) => {
  const { runCli } = fixture(t, {
    files: {
      'docs/features/inline.md': featureDoc('inline', 'setupChecks: [daemon-running, gh-auth]'),
      'docs/features/block.md': featureDoc(
        'block',
        'setupChecks:\n  - repo-registered\n  - gh-auth',
      ),
      'docs/features/exempt.md': featureDoc(
        'exempt',
        "setupExempt: 'needs only a running daemon, which daemon-running covers'",
      ),
    },
  })
  const result = runCli()
  assert.equal(result.status, 0, result.stdout + result.stderr)
})

test('setupCheckFailures rejects multiline setupExempt, non-array setupChecks, and duplicate baseline keys', () => {
  const multiline = setupCheckFailures({
    features: [
      {
        file: 'docs/features/multi.md',
        id: 'multi',
        setupExempt: 'line one\nline two',
      },
    ],
    ids: DEFAULT_IDS,
    baseline: [],
  })
  assert.ok(multiline.some((f) => f.includes('docs/features/multi.md: setup-exempt-reason:')))

  const nonArray = setupCheckFailures({
    features: [
      {
        file: 'docs/features/scalar.md',
        id: 'scalar',
        setupChecks: 'gh-auth',
      },
    ],
    ids: DEFAULT_IDS,
    baseline: [],
  })
  assert.ok(nonArray.some((f) => f.includes('docs/features/scalar.md: setup-empty-list:')))

  const blankItem = setupCheckFailures({
    features: [
      {
        file: 'docs/features/blank.md',
        id: 'blank',
        setupChecks: ['gh-auth', '   '],
      },
    ],
    ids: DEFAULT_IDS,
    baseline: [],
  })
  assert.ok(blankItem.some((f) => f.includes('docs/features/blank.md: setup-empty-list:')))

  const duplicateBaseline = setupCheckFailures({
    features: [{ file: 'docs/features/dup.md', id: 'dup' }],
    ids: DEFAULT_IDS,
    baseline: ['dup — reason one', 'dup — reason two'],
  })
  assert.ok(duplicateBaseline.some((f) => f.includes('dup')))
})
