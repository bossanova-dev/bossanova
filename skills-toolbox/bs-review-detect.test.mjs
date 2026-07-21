import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { matchLenses, secondVoiceAgent } from './bs-review-detect.mjs'
import { loadSkillConfig } from './skill-config.mjs'

const scriptPath = fileURLToPath(new URL('./bs-review-detect.mjs', import.meta.url))
const repoRoot = fileURLToPath(new URL('..', import.meta.url))

/** Run the CLI block as a subprocess and return { stdout, status }. */
function runCli(args = [], input = '', cwd = repoRoot) {
  const res = spawnSync(process.execPath, [scriptPath, ...args], {
    input,
    encoding: 'utf8',
    cwd, // default repoRoot so loadSkillConfig() finds the committed .boss-skills.json
  })
  return { stdout: res.stdout.trim(), status: res.status }
}

// A minimal registry mirroring the .boss-skills.json lensMap shape (id/skill/glob +
// per-lens fallbackRubric) so the unit tests do not depend on the repo config.
const LENSES = [
  { id: 'go', skill: 'golang-pro', glob: '**/*.go', fallbackRubric: 'go rubric' },
  { id: 'tui', skill: 'tui-design', glob: 'services/boss/**', fallbackRubric: 'tui rubric' },
  { id: 'web', skill: 'impeccable', glob: 'services/web/**', fallbackRubric: 'web rubric' },
  { id: 'db', skill: 'database-review', glob: '**/migrations/**', fallbackRubric: 'db rubric' },
]

test('matchLenses selects the go lens with its skill and fallback for a .go change', () => {
  const m = matchLenses(['services/bossd/internal/tmux/tmux.go', 'README.md'], LENSES)
  const go = m.find((x) => x.lens === 'go')
  assert.ok(go, 'go lens matched')
  assert.equal(go.skill, 'golang-pro')
  assert.equal(go.fallbackRubric, 'go rubric')
  assert.deepEqual(go.files, ['services/bossd/internal/tmux/tmux.go'])
})

test('matchLenses matches tui and web by path prefix (and go by suffix)', () => {
  const m = matchLenses(
    ['services/boss/internal/views/attach.go', 'services/web/src/App.tsx'],
    LENSES,
  )
  // the .go file under services/boss is both go and tui; the tsx is web
  assert.deepEqual(m.map((x) => x.lens).sort(), ['go', 'tui', 'web'])
})

test('matchLenses on a docs-only diff returns no lenses (always-on rounds still cover it)', () => {
  assert.deepEqual(matchLenses(['docs/foo.md', 'CONCEPTS.md'], LENSES), [])
})

test('matchLenses selects the db lens for a migration path with skill database-review', () => {
  const m = matchLenses(
    ['services/bossd/migrations/20260101000000_add_x.sql', 'internal/store/store.go'],
    LENSES,
  )
  const db = m.find((x) => x.lens === 'db')
  assert.ok(db, 'db lens matched')
  assert.equal(db.skill, 'database-review')
  assert.equal(db.fallbackRubric, 'db rubric')
  assert.deepEqual(db.files, ['services/bossd/migrations/20260101000000_add_x.sql'])
})

test('matchLenses does NOT select the db lens for a non-migration diff', () => {
  const m = matchLenses(['internal/store/store.go', 'docs/schema.md'], LENSES)
  assert.ok(
    !m.some((x) => x.lens === 'db'),
    'no db lens for a .go/docs diff outside migrations/',
  )
})

test('matchLenses with an empty/absent registry degrades to no lenses', () => {
  assert.deepEqual(matchLenses(['x.go'], []), [])
  assert.deepEqual(matchLenses(['x.go'], undefined), [])
})

test('matchLenses with an empty/absent file list returns no lenses', () => {
  assert.deepEqual(matchLenses([], LENSES), [])
  assert.deepEqual(matchLenses(undefined, LENSES), [])
})

test('every configured lens in the fixture carries a non-empty inline fallback rubric', () => {
  for (const l of LENSES) {
    assert.ok(typeof l.fallbackRubric === 'string' && l.fallbackRubric.trim().length > 0)
  }
})

test('every lens in the committed .boss-skills.json carries a non-empty fallbackRubric', () => {
  const { lensMap } = loadSkillConfig({ cwd: repoRoot })
  assert.ok(lensMap.length >= 1, 'lensMap is non-empty')
  for (const rule of lensMap) {
    assert.ok(
      typeof rule.fallbackRubric === 'string' && rule.fallbackRubric.trim().length > 0,
      `lens "${rule.id}" needs a non-empty fallbackRubric`,
    )
  }
})

test('the committed web lens fallbackRubric is byte-identical to the prior baked fallback', () => {
  const { lensMap } = loadSkillConfig({ cwd: repoRoot })
  const web = lensMap.find((r) => r.id === 'web')
  assert.ok(web, 'web lens present')
  assert.equal(
    web.fallbackRubric,
    'review through an inline React/TypeScript/web-UI rubric: component correctness, hook/effect races, accessibility, type-boundary cleanliness, dead/duplicated code',
  )
})

test('secondVoiceAgent returns the opposite agent', () => {
  assert.equal(secondVoiceAgent('claude'), 'codex')
  assert.equal(secondVoiceAgent('codex'), 'claude')
})

test('secondVoiceAgent defaults unknown/empty agent to codex (claude is the common host)', () => {
  assert.equal(secondVoiceAgent(''), 'codex')
  assert.equal(secondVoiceAgent('opencode'), 'codex')
})

test('CLI --second-voice claude prints codex', () => {
  const { stdout, status } = runCli(['--second-voice', 'claude'])
  assert.equal(stdout, 'codex')
  assert.equal(status, 0)
})

test('CLI --second-voice with missing value defaults to codex', () => {
  const { stdout, status } = runCli(['--second-voice'])
  assert.equal(stdout, 'codex')
  assert.equal(status, 0)
})

test('CLI --second-voice opencode prints codex', () => {
  const { stdout, status } = runCli(['--second-voice', 'opencode'])
  assert.equal(stdout, 'codex')
  assert.equal(status, 0)
})

test('CLI --lenses classifies a newline-separated file list from the config registry', () => {
  const { stdout, status } = runCli(
    ['--lenses'],
    'services/bossd/internal/x.go\nservices/web/src/App.tsx\ndocs/readme.md\n',
  )
  assert.equal(status, 0)
  const lenses = JSON.parse(stdout)
  const byLens = Object.fromEntries(lenses.map((l) => [l.lens, l]))
  assert.equal(byLens.go.skill, 'golang-pro')
  assert.equal(byLens.web.skill, 'impeccable')
  assert.ok(byLens.go.fallbackRubric && byLens.go.fallbackRubric.length > 0)
  assert.ok(byLens.web.fallbackRubric && byLens.web.fallbackRubric.length > 0)
  // the docs file matches no lens (still covered by the always-on rounds)
  assert.ok(!lenses.some((l) => l.files.includes('docs/readme.md')))
})

test('CLI --lenses on empty stdin yields no lenses', () => {
  const { stdout, status } = runCli(['--lenses'], '')
  assert.equal(stdout, '[]')
  assert.equal(status, 0)
})

test('CLI --lenses without a .boss-skills.json still emits per-lens fallbackRubric', () => {
  // Reviewer regression: from a checkout lacking .boss-skills.json, loadSkillConfig()
  // returns DEFAULT_CONFIG. Every emitted lens must still carry a non-empty inline
  // fallback so a non-vendored skill (e.g. impeccable for web) can degrade gracefully.
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-detect-'))
  try {
    const { stdout, status } = runCli(['--lenses'], 'pkg/x.go\nservices/web/src/App.tsx\n', dir)
    assert.equal(status, 0)
    const lenses = JSON.parse(stdout)
    assert.ok(lenses.length >= 1, 'defaults still classify the changed files')
    for (const l of lenses) {
      assert.ok(
        typeof l.fallbackRubric === 'string' && l.fallbackRubric.trim().length > 0,
        `lens "${l.lens}" from defaults needs a non-empty fallbackRubric`,
      )
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
