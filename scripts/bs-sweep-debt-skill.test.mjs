// Behaviour tests for the bs-sweep-debt survey helper (scripts/bs-sweep-debt-survey.mjs).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import {
  parseDetectorFindings,
  candidateKey,
  classifyCandidate,
  complexityAxis,
  validateSurveyCandidate,
  runCli as runSurveyCli,
  DEBT_SURVEY_USAGE,
} from './bs-sweep-debt-survey.mjs'

const rootDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const read = (p) => fs.readFileSync(path.join(rootDir, p), 'utf8')

test('dupl extraction retains every clone in multiline groups and legacy pairs', () => {
  const candidates = parseDetectorFindings(
    [
      '$ make debt-dupl-bosso',
      'cd services/bosso && go run github.com/mibk/dupl@latest -t 150 .',
      'found 2 clones:',
      '  internal/live/ownership.go:858,891',
      '  internal/live/ownership.go:925,956',
      'found 3 clones:',
      '  internal/server/billing_test.go:3552,3574',
      '  internal/server/billing_test.go:3579,3601',
      '  internal/server/billing_test.go:3663,3685',
      'internal/server/proxy_cross_org_cron.go:89,115 internal/server/proxy_cross_org_repos.go:148,185',
      'Found total 3 clone groups.',
    ].join('\n'),
  )
  assert.deepEqual(
    candidates.map((c) => [c.path, c.evidence]),
    [
      [
        'services/bosso/internal/live/ownership.go',
        'services/bosso/internal/live/ownership.go:925,956',
      ],
      [
        'services/bosso/internal/server/billing_test.go',
        'services/bosso/internal/server/billing_test.go:3579,3601',
      ],
      [
        'services/bosso/internal/server/billing_test.go',
        'services/bosso/internal/server/billing_test.go:3663,3685',
      ],
      [
        'services/bosso/internal/server/proxy_cross_org_cron.go',
        'services/bosso/internal/server/proxy_cross_org_repos.go',
      ],
    ],
  )
  assert.equal(new Set(candidates.map(candidateKey)).size, 4)
  for (const candidate of candidates) {
    assert.equal(candidate.category, 'duplication')
    assert.equal(candidate.module, 'bosso')
    assert.equal(candidate.confirmationCommand, 'make debt-dupl-bosso')
  }
})

test('dupl extraction resets clone groups at headers and detector commands', () => {
  const candidates = parseDetectorFindings(
    [
      '$ make debt-dupl-bosso',
      'found 2 clones:',
      '  internal/live/ownership.go:858,891',
      'found 2 clones:',
      '  internal/server/proxy_cross_org_cron.go:89,115',
      '  internal/server/proxy_cross_org_repos.go:148,185',
      '$ make debt-dupl-bossd',
      '  internal/server/other.go:1,10',
      'found 2 clones:',
      '  internal/server/one.go:11,20',
      '  internal/server/two.go:21,30',
    ].join('\n'),
  )
  assert.deepEqual(
    candidates.map((c) => [c.path, c.evidence]),
    [
      [
        'services/bosso/internal/server/proxy_cross_org_cron.go',
        'services/bosso/internal/server/proxy_cross_org_repos.go:148,185',
      ],
      ['services/bossd/internal/server/one.go', 'services/bossd/internal/server/two.go:21,30'],
    ],
  )
})

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
  assert.equal(filesize.excluded, 'decomposition: 1778 lines exceeds 2x limit 800')
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

test('extraction normalizes real module-local paths without weakening path rejection', () => {
  const candidates = parseDetectorFindings(
    [
      '$ make debt-cyclo-bossd',
      '287 main run cmd/main.go:1326:1',
      '$ make debt-filesize-bossd',
      'cmd/main.go:1326: file length is 1200 lines, which exceeds the limit of 800',
      '$ pnpm -C services/web knip',
      'Unused files (1)',
      'src/legacy.ts',
      '$ make debt-deadcode-bossalib',
      'lib/bossalib/x.go:1:1: unreachable func: helperUnused',
      '$ make debt-cyclo-bossd',
      '20 p escape ../outside.go:1:1',
      '20 p wrong services/bosso/main.go:1:1',
    ].join('\n'),
  )
  assert.deepEqual(
    candidates.map((c) => c.path),
    [
      'services/bossd/cmd/main.go',
      'services/bossd/cmd/main.go',
      'services/web/src/legacy.ts',
      'lib/bossalib/x.go',
      '../outside.go',
      'services/bosso/main.go',
    ],
  )
  assert.equal(candidates[0].area, 'services/bossd')
  assert.equal(candidates[0].findingLine, '287 main run cmd/main.go:1326:1')
  assert.equal(validateSurveyCandidate(candidates[0]).ok, true)
  assert.equal(validateSurveyCandidate(candidates[4]).reason, 'path must be repo-root-relative')
})

test('Go exported dead-code is excluded conservatively; knip exports remain eligible', () => {
  for (const symbol of ['ExportedFunc', '(*Manager).Transition', 'hidden.Transition']) {
    assert.match(
      classifyCandidate({ category: 'dead-code', path: 'lib/bossalib/x.go', evidence: symbol })
        .excluded,
      /^exported-symbol:/,
    )
  }
  for (const symbol of ['helperUnused', '(*Manager).transition']) {
    assert.equal(
      classifyCandidate({ category: 'dead-code', path: 'lib/bossalib/x.go', evidence: symbol })
        .excluded,
      undefined,
    )
  }
  assert.equal(
    classifyCandidate({
      category: 'dead-code',
      path: 'services/web/x.ts',
      evidence: 'ExportedFunc',
    }).excluded,
    undefined,
  )
})

test('stdlib vuln scope and fixed version survive extraction without excluding module vulns', () => {
  const candidates = parseDetectorFindings(
    read('scripts/fixtures/bs-sweep-debt/detector-output.txt'),
  )
  const stdlib = candidates.find((c) => c.vulnScope === 'stdlib')
  assert.ok(stdlib)
  assert.match(stdlib.evidence, /Fixed in: go1/)
  assert.match(classifyCandidate(stdlib).excluded, /^toolchain-only:/)
  const module = candidates.find((c) => c.evidence.startsWith('GO-2023-1571'))
  assert.equal(module.vulnScope, 'module')
  assert.equal(classifyCandidate(module).excluded, undefined)
})

test('path-scoped intentional literals exclude only matching portability findings and warn when stale', () => {
  const base = { category: 'portability', path: 'scripts/format-staged.sh' }
  assert.match(
    classifyCandidate({ ...base, findingLine: 'SCRIPT_DIR=$(CDPATH= cd "x")' }).excluded,
    /^known-intentional:/,
  )
  assert.equal(classifyCandidate({ ...base, findingLine: 'readlink -f x' }).excluded, undefined)
  const fixture = fs.mkdtempSync(path.join(os.tmpdir(), 'debt-intentional-'))
  try {
    fs.mkdirSync(path.join(fixture, 'scripts/debt'), { recursive: true })
    fs.writeFileSync(path.join(fixture, base.path), 'unrelated literal')
    fs.writeFileSync(
      path.join(fixture, 'scripts/debt/portability-known-intentional.json'),
      JSON.stringify([
        { path: base.path, contains: 'ABSENT-LITERAL', reason: 'test' },
        { path: 'scripts/missing.sh', contains: 'x', reason: 'gone' },
      ]),
    )
    const run = spawnSync(
      process.execPath,
      [path.join(rootDir, 'scripts/bs-sweep-debt-survey.mjs'), 'validate-candidates', '[]'],
      { cwd: fixture, encoding: 'utf8' },
    )
    assert.equal(run.status, 0, run.stderr)
    assert.deepEqual(JSON.parse(run.stdout), [])
    assert.equal(run.stderr.match(/stale intentional entry/g).length, 2)
  } finally {
    fs.rmSync(fixture, { recursive: true, force: true })
  }
})

test('complexity axis prefers eligible files and excludes decomposition and build constraints', () => {
  const repoRoot = fs.mkdtempSync(path.join(rootDir, '.tmp-bs-sweep-debt-'))
  try {
    fs.writeFileSync(path.join(repoRoot, 'plain.go'), 'package p')
    fs.writeFileSync(path.join(repoRoot, 'tag.go'), '//go:build custom\npackage p')
    fs.writeFileSync(path.join(repoRoot, 'plain_linux.go'), 'package p')
    const file = (p) => ({
      category: 'complexity-hotspot',
      path: p,
      evidence: '1200 lines (limit 800)',
      confirmationCommand: 'make debt-filesize-boss',
    })
    const cyclo = {
      category: 'complexity-hotspot',
      evidence: 'f',
      confirmationCommand: 'make debt-cyclo-boss',
    }
    assert.equal(
      complexityAxis([classifyCandidate(file('plain.go'), { repoRoot }), cyclo]).axis,
      'file',
    )
    for (const p of ['tag.go', 'plain_linux.go']) {
      const c = classifyCandidate(file(p), { repoRoot })
      assert.match(c.excluded, /^build-constrained:/)
      assert.equal(complexityAxis([c, cyclo]).axis, 'function')
    }
    const oversized = classifyCandidate(
      { ...file('plain.go'), evidence: '1601 lines (limit 800)' },
      { repoRoot },
    )
    assert.match(oversized.excluded, /^decomposition:/)
    assert.equal(complexityAxis([oversized]).axis, 'none')
    assert.equal(complexityAxis([]).axis, 'none')
  } finally {
    fs.rmSync(repoRoot, { recursive: true, force: true })
  }
})

test('CLI extract, validation and axis apply guardrails without losing excluded candidates', () => {
  let out = ''
  const io = { write: (s) => (out += s), errWrite: () => {} }
  assert.equal(
    runSurveyCli(
      ['extract', path.join(rootDir, 'scripts/fixtures/bs-sweep-debt/detector-output.txt')],
      io,
    ),
    0,
  )
  assert.ok(JSON.parse(out).some((c) => c.excluded?.startsWith('toolchain-only:')))
  out = ''
  assert.equal(
    runSurveyCli(
      [
        'validate-candidates',
        JSON.stringify([
          {
            category: 'dead-code',
            module: 'scripts',
            path: 'scripts/bs-sweep-debt-survey.mjs',
            evidence: 'ExportedFunc',
            confirmationCommand: 'make debt-deadcode-boss',
          },
        ]),
      ],
      io,
    ),
    0,
  )
  assert.match(JSON.parse(out)[0].excluded, /^exported-symbol:/)
  out = ''
  assert.equal(runSurveyCli(['axis', '[]'], io), 0)
  assert.equal(JSON.parse(out).axis, 'none')
  for (const verb of ['extract', 'validate-candidates', 'axis', 'no-change-evidence'])
    assert.ok(DEBT_SURVEY_USAGE.includes(verb))
})

test('NO_CHANGE evidence preserves ranked rejection reasons and validation drops before cleanup', () => {
  const payload = {
    surveyedCategories: ['dead-code', 'security'],
    surveyedAreas: ['a', 'b', 'c'],
    candidates: [
      { path: 'a.go', category: 'dead-code', excluded: 'exported-symbol: API' },
      { path: 'b.go', category: 'security', score: 5, rejected: 'no safe mitigation' },
    ],
  }
  let out = '',
    err = ''
  const io = { write: (s) => (out += s), errWrite: (s) => (err += s) }
  assert.equal(
    runSurveyCli(
      [
        'no-change-evidence',
        JSON.stringify({ status: 'ok', kind: 'none', payload }),
        'bs-sweep-debt survey: dropped c.go: invalid module',
      ],
      io,
    ),
    0,
  )
  assert.match(out, /- b.go \(security\): no safe mitigation/)
  assert.match(out, /- c.go \(validation\): invalid module/)
  assert.ok(out.indexOf('b.go') < out.indexOf('a.go'))
  payload.candidates.push({ path: 'c.go', category: 'dead-code', rejected: 'requires API change' })
  assert.equal(
    runSurveyCli(
      ['no-change-evidence', JSON.stringify({ status: 'ok', kind: 'none', payload })],
      io,
    ),
    0,
  )
  out = ''
  err = ''
  assert.equal(
    runSurveyCli(
      ['no-change-evidence', JSON.stringify({ status: 'ok', payload: { candidates: [] } })],
      io,
    ),
    3,
  )
  assert.equal(err.match(/incomplete:/g).length, 3)
})
