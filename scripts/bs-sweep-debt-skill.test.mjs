// Behaviour tests for the bs-sweep-debt survey helper (scripts/bs-sweep-debt-survey.mjs).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  parseDetectorFindings,
  candidateKey,
  validateSurveyCandidate,
  runCli as runSurveyCli,
  DEBT_SURVEY_USAGE,
} from './bs-sweep-debt-survey.mjs'

const rootDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const read = (p) => fs.readFileSync(path.join(rootDir, p), 'utf8')

// ---------------------------------------------------------------------------
// Loss parity — the cheap-tier survey extraction drops no detector finding.
// ---------------------------------------------------------------------------

test('survey loss check: every detector finding surfaces as a candidate (no drop)', () => {
  const fixtureDir = 'scripts/fixtures/bs-sweep-debt'
  const output = read(path.join(fixtureDir, 'detector-output.txt'))
  const expected = JSON.parse(read(path.join(fixtureDir, 'expected-candidates.json')))

  const surfaced = parseDetectorFindings(output)
  const surfacedKeys = new Set(surfaced.map(candidateKey))

  for (const cand of expected) {
    assert.ok(
      surfacedKeys.has(candidateKey(cand)),
      `survey dropped a detector finding: ${candidateKey(cand)}`,
    )
  }
  // Sanity: the fixture spans >=3 findings across >=2 categories (the breadth the survey needs).
  assert.ok(expected.length >= 3, 'fixture must carry >=3 findings')
  assert.ok(new Set(expected.map((c) => c.category)).size >= 2, 'fixture must span >=2 categories')
})

test('survey candidates carry module attribution, repo-root path, and confirmation evidence', () => {
  const output = read('scripts/fixtures/bs-sweep-debt/detector-output.txt')
  const surfaced = parseDetectorFindings(output)

  const bossd = surfaced.find((c) => c.path === 'services/bossd/internal/foo/foo.go')
  assert.deepEqual(
    {
      module: bossd.module,
      path: bossd.path,
      confirmationCommand: bossd.confirmationCommand,
      findingLine: bossd.findingLine,
    },
    {
      module: 'bossd',
      path: 'services/bossd/internal/foo/foo.go',
      confirmationCommand: 'make debt-deadcode-bossd',
      findingLine: 'services/bossd/internal/foo/foo.go:42:6: unreachable func: helperUnused',
    },
  )

  const filesize = surfaced.find((c) => c.path === 'services/boss/internal/views/home.go')
  assert.equal(filesize.module, 'boss')
  assert.equal(filesize.confirmationCommand, 'make debt-filesize-boss')
  assert.equal(
    filesize.excluded,
    'file-axis decomposition candidate: 1778 lines exceeds 2x limit 800',
  )
})

test('survey candidate validation accepts only existing paths inside the declared module', () => {
  const repoRoot = fs.mkdtempSync(path.join(rootDir, '.tmp-bs-sweep-debt-'))
  try {
    fs.mkdirSync(path.join(repoRoot, 'services/bossd/internal/server'), { recursive: true })
    fs.mkdirSync(path.join(repoRoot, 'services/bosso/internal/server'), { recursive: true })
    fs.writeFileSync(path.join(repoRoot, 'services/bossd/internal/server/proxy.go'), '')
    fs.writeFileSync(path.join(repoRoot, 'services/bosso/internal/server/proxy.go'), '')

    const base = { module: 'bossd', path: 'services/bossd/internal/server/proxy.go' }
    assert.deepEqual(validateSurveyCandidate(base, { repoRoot }), { ok: true, reason: null })
    assert.deepEqual(
      validateSurveyCandidate(
        { module: 'bossd', file: 'services/bossd/internal/server/proxy.go' },
        { repoRoot },
      ),
      { ok: false, reason: 'missing path' },
    )

    assert.deepEqual(
      validateSurveyCandidate(
        { module: 'bossd', path: 'services/bosso/internal/server/proxy.go' },
        { repoRoot },
      ),
      { ok: false, reason: 'path is outside declared module bossd' },
    )
    assert.deepEqual(
      validateSurveyCandidate(
        { module: 'bossd', path: 'services/bossd/internal/server/missing.go' },
        { repoRoot },
      ),
      { ok: false, reason: 'path does not exist at repo root' },
    )
    assert.deepEqual(
      validateSurveyCandidate({ module: 'bossd', path: 'internal/server/proxy.go' }, { repoRoot }),
      { ok: false, reason: 'path does not exist at repo root' },
    )
  } finally {
    fs.rmSync(repoRoot, { force: true, recursive: true })
  }
})

// ---------------------------------------------------------------------------
// Detector wiring (BOS-525) — the file-size detector the complexity-hotspot
// playbook points at exists, and its threshold knob is not silently inert.
// ---------------------------------------------------------------------------

test('the debt-filesize detector exists and its threshold knob actually rewrites the config', () => {
  const makefile = read('Makefile')
  const toml = read('scripts/debt/revive-filesize.toml')

  assert.match(makefile, /^debt-filesize-\$\(2\):$/m, 'define-debt-targets must define the target')
  assert.match(
    makefile,
    /^DEBT_FILESIZE_THRESHOLD \?= (\d+)$/m,
    'the threshold must be overridable',
  )
  assert.match(makefile, /REVIVE_PKG\s*:=/, 'the revive tool must be pinned in a variable')
  assert.match(toml, /\[rule\.file-length-limit\]/, 'the detector config must carry the rule')

  // The recipe seds DEBT_FILESIZE_THRESHOLD over the committed default. Replay that exact
  // expression here: a pattern that no longer matches the config would leave the knob inert
  // (the detector would silently keep reporting against the committed default forever).
  const recipe = /^debt-filesize-\$\(2\):$[\s\S]*?^endef$/m.exec(makefile)
  assert.ok(recipe, 'the debt-filesize recipe must live inside define-debt-targets')
  const sed = /sed 's\/(.+)\/(.+)\/'/.exec(recipe[0])
  assert.ok(sed, 'the debt-filesize recipe must carry a sed rewrite of the threshold')
  const pattern = new RegExp(sed[1].replace(/\\\(/g, '(').replace(/\\\)/g, ')'))
  const replacement = sed[2].replace('\\1', '$1').replace('$$(DEBT_FILESIZE_THRESHOLD)', '4242')
  const rewritten = toml.replace(pattern, replacement)
  assert.notEqual(rewritten, toml, 'the sed pattern no longer matches revive-filesize.toml')
  assert.match(rewritten, /max = 4242/, 'the rewrite must land on the file-length-limit max')

  // The committed default must equal the Makefile default, or `make debt-filesize-*` and a
  // bare `revive -config scripts/debt/revive-filesize.toml` would disagree.
  const makeDefault = /^DEBT_FILESIZE_THRESHOLD \?= (\d+)$/m.exec(makefile)[1]
  const tomlDefault = /max = (\d+)/.exec(toml)[1]
  assert.equal(tomlDefault, makeDefault, 'toml max must equal the Makefile threshold default')

  // The recipe's `|| true` makes the detector non-blocking, which also means revive exiting
  // non-zero (unparseable config) presents as "0 findings" — and revive DISABLES
  // file-length-limit when max <= 0, so a bare `0` silences it while looking green. Three
  // guards keep that from becoming a silent all-clear. Pin each guard TOGETHER WITH the
  // `exit 1` it must reach: matching the condition alone would still pass a guard whose body
  // was emptied, or one whose failing half was deleted along with its consequent.
  assert.match(
    recipe[0],
    /case "\$\$\(DEBT_FILESIZE_THRESHOLD\)" in ''\|\*\[!0-9\]\*\|0\*\)[\s\S]{0,220}?exit\s+1;; esac/,
    'a non-numeric/leading-zero DEBT_FILESIZE_THRESHOLD must exit 1 before the config is built',
  )
  assert.match(
    recipe[0],
    /grep -q '\^\\\[rule\\\.file-length-limit\\\]'[\s\S]{0,80}?grep -qE "max = \$\$\(DEBT_FILESIZE_THRESHOLD\)[\s\S]{0,240}?exit\s+1; \}/,
    'a generated config missing the rule or the REQUESTED max must exit 1',
  )
  assert.match(
    recipe[0],
    /trap 'rm -f "\$\$\$\$cfg"' EXIT\s+INT\s+TERM/,
    'the recipe must trap-clean its temp config',
  )
  // The survey parser only understands revive's `default` one-line shape. Fed `friendly`
  // output it returns [] — a 100% silent loss the D8 gate cannot see, because that gate
  // asserts over a static fixture rather than a live run. Pin the formatter to the parser.
  assert.match(
    recipe[0],
    /-formatter\s+default\b/,
    "the recipe must use revive's default formatter; the survey parser silently drops every friendly-shaped finding",
  )
})

// ---------------------------------------------------------------------------
// Ratchet — the always-resident body stays under the post-split ceiling.
// ---------------------------------------------------------------------------

// --- survey CLI command surface -----------------------------------------------

for (const flag of ['--help', '-h', 'help']) {
  test(`survey ${flag} exits 0 and names validate-candidates`, () => {
    let out = ''
    let err = ''
    const code = runSurveyCli([flag], { write: (s) => (out += s), errWrite: (s) => (err += s) })
    assert.equal(code, 0)
    assert.equal(err, '')
    assert.ok(out.includes('validate-candidates'))
    assert.equal(out, DEBT_SURVEY_USAGE)
  })
}

test('survey rejects an unrecognised command instead of reporting success', () => {
  // The inverse-shape defect: any non-matching command used to return 0 having printed
  // nothing, so a typo read as an empty-but-successful survey.
  let out = ''
  let err = ''
  const code = runSurveyCli(['bogus'], { write: (s) => (out += s), errWrite: (s) => (err += s) })
  assert.equal(code, 2)
  assert.equal(out, '')
  // prose-pin: literal-space ok — this is the CLI's own one-line stderr string, not prose.
  assert.match(err, /unknown command: bogus/)
  assert.ok(err.includes('validate-candidates'))
})

test('survey rejects an empty invocation instead of reporting success', () => {
  let out = ''
  let err = ''
  const code = runSurveyCli([], { write: (s) => (out += s), errWrite: (s) => (err += s) })
  assert.equal(code, 2)
  assert.equal(out, '')
  // prose-pin: literal-space ok — this is the CLI's own one-line stderr string, not prose.
  assert.match(err, /unknown command: \(none\)/)
})

test('survey validate-candidates still succeeds and prints JSON', () => {
  let out = ''
  const code = runSurveyCli(['validate-candidates', '[]'], {
    write: (s) => (out += s),
    errWrite: () => {},
  })
  assert.equal(code, 0)
  assert.deepEqual(JSON.parse(out), [])
})
