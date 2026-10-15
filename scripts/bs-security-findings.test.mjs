// Tests for scripts/bs-security-findings.mjs. Every fixture below is invented: the
// report shapes mimic real reviewer output, but no finding, path or identifier is
// taken from a real security review.
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  RECORD_SCHEMA_VERSION,
  RUN_LAYOUT,
  SECURITY_LEDGER_QUERY,
  SECURITY_MARKER_PREFIX,
  classifyAgainstLedger,
  classifyDefect,
  fetchSecurityLedger,
  fingerprint,
  markerLine,
  mergeFindings,
  nearDuplicateCandidates,
  normaliseConfidence,
  normaliseRecord,
  normaliseSeverity,
  parseFindingsMarkdown,
  parseSecurityMarkers,
  renderKnownList,
  verifyLocations,
} from './bs-security-findings.mjs'

const SCRIPT = path.join(path.dirname(fileURLToPath(import.meta.url)), 'bs-security-findings.mjs')
const ROOT = '/synthetic/root'

const CLAUDE_REPORT = `# Synthetic lens report

## Findings

### 1. Widget handler skips the owner check before delete

- Severity: medium (threat model a)
- Location: services/widget/handler.go:42 (\`deleteWidget(...)\` never calls \`requireOwner\`, see notes); services/widget/routes.go (\`registerRoutes\`)
- Issue: The delete handler trusts the widget id from the request
  and never compares the caller to the owner.
- Exploit: Any signed-in user can delete another user's widget.
- Fix: Call requireOwner before deleting.
- Confidence: confirmed-by-reading (race window)

### Note on scope

This lens only looked at the widget service.

## Checked and sound

- \`ListWidgets\` filters by owner.
`

const CODEX_REPORT = `### Gizmo store joins request paths without cleaning them

- **Severity:** low/medium
- **Locations:** [${ROOT}/lib/gizmo/store.go:17](${ROOT}/lib/gizmo/store.go:17), [lib/gizmo/load.go:3](${ROOT}/lib/gizmo/load.go:3), [/elsewhere/synthetic/other.go:9](/elsewhere/synthetic/other.go:9)
- **Defect:** \`loadGizmo\` joins a caller path without cleaning it.
- **Impact:** A crafted name escapes the gizmo directory.
- **Fix:** Clean and contain the path.
- **Confidence:** likely
`

function record(overrides = {}) {
  const base = {
    source: 'claude',
    lens: 'authz-widgets',
    lensFamily: 'authz',
    title: 'Widget handler skips the owner check before delete',
    severity: 'medium',
    severityRaw: 'medium',
    locations: [{ path: 'services/widget/handler.go', line: 42, note: '', raw: '' }],
    primaryPath: 'services/widget/handler.go',
    defect: 'The delete handler never compares the caller to the owner.',
    impact: 'Any user can delete any widget.',
    fix: 'Check the owner.',
    confidence: 'likely',
    confidenceRaw: 'likely',
  }
  const rec = { ...base, ...overrides }
  if (overrides.locations && !('primaryPath' in overrides)) {
    rec.primaryPath = rec.locations[0]?.path ?? null
  }
  return { ...rec, fingerprint: fingerprint(rec) }
}

test('module exports a versioned schema and the run-dir layout', () => {
  assert.equal(RECORD_SCHEMA_VERSION, 1)
  assert.deepEqual(RUN_LAYOUT, {
    findingsDir: 'findings',
    codexFindings: 'codex/findings.json',
    secretScanFindings: 'secret-scan/findings.json',
    merged: 'merged.json',
  })
  assert.match(SECURITY_LEDGER_QUERY, /state \{ type name \}/)
  assert.match(SECURITY_LEDGER_QUERY, /\btitle\b/)
})

test('parseFindingsMarkdown maps Issue/Exploit labels to defect/impact', () => {
  const { records } = parseFindingsMarkdown(CLAUDE_REPORT, {
    source: 'claude',
    lens: 'authz-widgets',
    lensFamily: 'authz',
    root: ROOT,
  })
  assert.equal(records.length, 1)
  const [r] = records
  assert.equal(r.title, 'Widget handler skips the owner check before delete')
  assert.equal(
    r.defect,
    'The delete handler trusts the widget id from the request and never compares the caller to the owner.',
  )
  assert.equal(r.impact, "Any signed-in user can delete another user's widget.")
  assert.equal(r.fix, 'Call requireOwner before deleting.')
  assert.equal(r.severity, 'medium')
  assert.equal(r.severityRaw, 'medium (threat model a)')
  assert.equal(r.confidence, 'confirmed-by-reading')
  assert.equal(r.confidenceRaw, 'confirmed-by-reading (race window)')
  assert.equal(r.source, 'claude')
  assert.equal(r.lens, 'authz-widgets')
  assert.equal(r.lensFamily, 'authz')
  assert.deepEqual(
    r.locations.map(({ path: p, line, note }) => ({ path: p, line, note })),
    [
      {
        path: 'services/widget/handler.go',
        line: 42,
        note: '`deleteWidget(...)` never calls `requireOwner`, see notes',
      },
      { path: 'services/widget/routes.go', line: null, note: '`registerRoutes`' },
    ],
  )
  assert.equal(r.primaryPath, 'services/widget/handler.go')
  assert.match(r.fingerprint, /^sf-[0-9a-f]{12}$/)
})

test('parseFindingsMarkdown reduces link-wrapped locations to repo-relative path:line', () => {
  const { records } = parseFindingsMarkdown(CODEX_REPORT, {
    source: 'codex',
    lens: 'codex',
    lensFamily: 'cross-model',
    root: ROOT,
  })
  assert.equal(records.length, 1)
  const [r] = records
  assert.equal(r.defect, '`loadGizmo` joins a caller path without cleaning it.')
  assert.equal(r.impact, 'A crafted name escapes the gizmo directory.')
  assert.equal(r.severity, 'medium')
  assert.deepEqual(
    r.locations.map(({ path: p, line, outsideRoot }) => ({ path: p, line, outsideRoot })),
    [
      { path: 'lib/gizmo/store.go', line: 17, outsideRoot: undefined },
      { path: 'lib/gizmo/load.go', line: 3, outsideRoot: undefined },
      { path: '/elsewhere/synthetic/other.go', line: 9, outsideRoot: true },
    ],
  )
  assert.equal(r.primaryPath, 'lib/gizmo/store.go')
})

test('parseFindingsMarkdown skips and counts headings without a severity', () => {
  const { records, skipped } = parseFindingsMarkdown(
    `${CLAUDE_REPORT}\n#### Missing location\n\n- Severity: high\n- Issue: nowhere.\n`,
    { source: 'claude', lens: 'authz-widgets', lensFamily: 'authz', root: ROOT },
  )
  assert.equal(records.length, 1)
  assert.deepEqual(skipped, [
    { heading: 'Findings', reason: 'no-severity' },
    { heading: 'Note on scope', reason: 'no-severity' },
    { heading: 'Checked and sound', reason: 'no-severity' },
    { heading: 'Missing location', reason: 'no-location' },
  ])
})

test('parseFindingsMarkdown does not treat a dotted call expression as a path', () => {
  const text = `### Server accepts unbounded request bodies

- Severity: high
- Location: connect.WithReadMaxBytes(1 << 20) is never set; services/api/server.go:88 (\`newServer\`); Makefile:12; widget.Handler
- Defect: The server reads bodies without a size limit.
- Confidence: plausible
`
  const { records } = parseFindingsMarkdown(text, {
    source: 'claude',
    lens: 'dos',
    lensFamily: 'dos',
    root: ROOT,
    isTopLevelFile: (name) => name === 'Makefile',
  })
  assert.deepEqual(
    records[0].locations.map(({ path: p, line }) => `${p}:${line}`),
    ['services/api/server.go:88', 'Makefile:12'],
  )
  assert.equal(records[0].primaryPath, 'services/api/server.go')
})

test('parseFindingsMarkdown keeps a bulleted top-level file:line inside the Location field', () => {
  const text = `### Build image runs as root

- Severity: medium
- Location: services/a.go:3
- Dockerfile:12
- services/b.go:9
- Defect: The image never drops privileges.
- Makefile:42
- Fix: Add a USER line.
`
  // A top-level file that shares a label's name must not swallow that label.
  const topLevel = new Set(['Dockerfile', 'Makefile', 'Defect'])
  const { records } = parseFindingsMarkdown(text, {
    source: 'claude',
    lens: 'build',
    lensFamily: 'build',
    root: ROOT,
    isTopLevelFile: (name) => topLevel.has(name),
  })
  assert.deepEqual(
    records[0].locations.map(({ path: p, line }) => `${p}:${line}`),
    ['services/a.go:3', 'Dockerfile:12', 'services/b.go:9'],
  )
  // Outside the Location field a bulleted file:line still ends the current field.
  assert.equal(records[0].defect, 'The image never drops privileges.')
  assert.equal(records[0].fix, 'Add a USER line.')
})

test('normaliseRecord makes JSON paths repo-relative before fingerprinting', () => {
  const base = { title: 'Gizmo store joins request paths', severity: 'high', defect: 'traversal' }
  const at = (p) =>
    normaliseRecord({ ...base, locations: [{ path: p, line: 1 }] }, { source: 'codex', root: ROOT })
  const same = [
    'lib/gizmo/store.go',
    './lib/gizmo/store.go',
    `${ROOT}/lib/gizmo/store.go`,
    'lib//gizmo/store.go',
  ].map(at)
  for (const r of same) {
    assert.equal(r.primaryPath, 'lib/gizmo/store.go')
    assert.equal(r.locations[0].path, 'lib/gizmo/store.go')
    assert.equal(r.locations[0].outsideRoot, undefined)
    assert.equal(r.fingerprint, same[0].fingerprint)
  }
  const outside = at('/elsewhere/synthetic/other.go')
  assert.equal(outside.locations[0].path, '/elsewhere/synthetic/other.go')
  assert.equal(outside.locations[0].outsideRoot, true)
})

test('normaliseSeverity handles parentheticals, ranges and garbage', () => {
  const table = [
    ['medium (threat model a)', 'medium', false],
    ['low/medium', 'medium', false],
    ['Critical (threat model d)', 'critical', false],
    ['**High**', 'high', false],
    ['low (could become high)', 'low', false],
    ['Informational', 'info', false],
    ['moderate', 'medium', false],
    ['garbage', 'high', true],
    ['', 'high', true],
    [undefined, 'high', true],
  ]
  for (const [raw, severity, severityUnparsed] of table) {
    assert.deepEqual(normaliseSeverity(raw), { severity, severityUnparsed }, `input ${raw}`)
  }
})

test('normaliseConfidence takes the leading token and defaults to speculative', () => {
  assert.equal(normaliseConfidence('confirmed-by-reading (race window)'), 'confirmed-by-reading')
  assert.equal(normaliseConfidence('**Likely** — needs a test'), 'likely')
  assert.equal(normaliseConfidence('pretty sure'), 'speculative')
  assert.equal(normaliseConfidence(undefined), 'speculative')
})

test('fingerprint is stable when only the line number changes', () => {
  const a = record()
  const b = record({
    locations: [{ path: 'services/widget/handler.go', line: 97, note: '', raw: '' }],
  })
  assert.match(a.fingerprint, /^sf-[0-9a-f]{12}$/)
  assert.equal(a.fingerprint, b.fingerprint)
  // A renumbered heading must not mint a new fingerprint either.
  assert.equal(
    fingerprint({ ...a, title: '7. Widget handler skips the owner check before delete' }),
    a.fingerprint,
  )
})

test('fingerprint changes when the primary path changes', () => {
  const a = record()
  const b = record({
    locations: [{ path: 'services/widget/other.go', line: 42, note: '', raw: '' }],
  })
  assert.notEqual(a.fingerprint, b.fingerprint)
  assert.notEqual(
    fingerprint({ ...a, lensFamily: 'cross-model' }),
    a.fingerprint,
    'the lens family is part of the fingerprint',
  )
})

// Golden values: filed Linear issues carry these fingerprints as markers, so any
// change to the key, the defect-class table or the title stem re-files every
// finding. If one of these goes red, the change needs a migration, not a re-pin.
test('fingerprint values are pinned for fixed findings', () => {
  const golden = [
    [
      {
        lensFamily: 'authz',
        primaryPath: 'services/widget/handler.go',
        title: 'Widget handler skips the owner check before delete',
        defect: 'The delete handler never compares the caller to the owner.',
      },
      'sf-18064a9649de',
    ],
    [
      {
        lensFamily: 'cross-model',
        primaryPath: 'lib/gizmo/store.go',
        title: '3. The gizmo store joins a request path into the data directory',
        defect: 'A ../ segment escapes the directory.',
      },
      'sf-ce7cab61b71b',
    ],
    [
      {
        lensFamily: 'secret-history',
        primaryPath: 'config/app.env',
        title: 'An API key is committed in the history',
        defect: 'A live key was committed.',
      },
      'sf-e8a8f18754ad',
    ],
    [
      {
        lensFamily: 'build',
        primaryPath: null,
        title: 'Odd naming in the build script',
        defect: 'Cosmetic.',
      },
      'sf-0ea8350685b4',
    ],
  ]
  for (const [rec, fp] of golden) assert.equal(fingerprint(rec), fp, rec.title)
})

test('classifyDefect maps keywords to the closed class set', () => {
  assert.equal(
    classifyDefect('Widget handler skips the owner check', 'missing authorization'),
    'authz',
  )
  assert.equal(classifyDefect('Gizmo path traversal', ''), 'path-traversal')
  assert.equal(classifyDefect('Unbounded request body', 'memory exhaustion'), 'dos')
  assert.equal(classifyDefect('Odd naming', 'cosmetic'), 'other')
})

function memoryReader(files) {
  return (abs) => {
    if (!Object.hasOwn(files, abs)) {
      const err = new Error(`ENOENT: ${abs}`)
      err.code = 'ENOENT'
      throw err
    }
    return files[abs]
  }
}

function memoryRealpath(files) {
  return (abs) => {
    memoryReader(files)(abs)
    return abs
  }
}

const HANDLER_LINES = Array.from({ length: 60 }, (_, i) =>
  i === 49 ? '\tif err := requireOwner(ctx, w); err != nil {' : `\t// line ${i + 1}`,
)
const FILES = { [`${ROOT}/services/widget/handler.go`]: `${HANDLER_LINES.join('\n')}\n` }

test('verifyLocations reports missing-file, line-out-of-range, token-not-found and outside-root', () => {
  const loc = (p, line, note = '', extra = {}) => ({ path: p, line, note, raw: '', ...extra })
  const cases = [
    [loc('services/widget/nope.go', 3), 'location-unverified', 'missing-file'],
    [loc('services/widget/handler.go', 500), 'location-unverified', 'line-out-of-range'],
    [
      loc('services/widget/handler.go', 5, '`absentToken` is used'),
      'location-unverified',
      'token-not-found',
    ],
    [loc('../escape.go', 1), 'location-unverified', 'outside-root'],
    [
      loc('/elsewhere/synthetic/other.go', 9, '', { outsideRoot: true }),
      'location-unverified',
      'outside-root',
    ],
    [
      loc('services/widget/handler.go', 42, '`requireOwner(...)` is skipped'),
      'location-verified',
      null,
    ],
    [loc('services/widget/handler.go', null, '`requireOwner`'), 'location-verified', null],
    [
      loc('services/widget/handler.go', 60, '`ab` is too short to check'),
      'location-verified',
      null,
    ],
  ]
  const records = cases.map(([l]) => record({ locations: [l] }))
  const verified = verifyLocations(records, {
    root: ROOT,
    readFile: memoryReader(FILES),
    realpath: memoryRealpath(FILES),
  })
  cases.forEach(([, status, reason], i) => {
    assert.equal(verified[i].locationStatus, status, `case ${i}`)
    assert.equal(verified[i].locations[0].reason, reason, `case ${i}`)
    assert.equal(verified[i].locations[0].verified, status === 'location-verified', `case ${i}`)
  })
  assert.equal(records[0].locationStatus, undefined, 'inputs are not mutated')

  // The record follows its primary location, and nothing is dropped.
  const mixed = verifyLocations(
    [
      record({
        locations: [loc('services/widget/nope.go', 1), loc('services/widget/handler.go', 50)],
      }),
    ],
    { root: ROOT, readFile: memoryReader(FILES), realpath: memoryRealpath(FILES) },
  )
  assert.equal(mixed[0].locations.length, 2)
  assert.equal(mixed[0].locationStatus, 'location-unverified')
  assert.equal(mixed[0].locations[1].verified, true)
})

test('verifyLocations treats a symlink that escapes the root as outside-root', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-security-findings-'))
  try {
    const root = path.join(tmp, 'repo')
    fs.mkdirSync(path.join(root, 'lib'), { recursive: true })
    fs.writeFileSync(path.join(tmp, 'outside.txt'), 'secret\n')
    fs.writeFileSync(path.join(root, 'lib/inside.txt'), 'fine\n')
    fs.symlinkSync(path.join(tmp, 'outside.txt'), path.join(root, 'lib/escape.txt'))
    fs.symlinkSync(path.join(tmp, 'outside.txt'), path.join(root, 'escape-dir-target'))
    fs.symlinkSync(tmp, path.join(root, 'up'))
    fs.symlinkSync(path.join(root, 'lib/inside.txt'), path.join(root, 'lib/alias.txt'))
    const at = (p) => record({ locations: [{ path: p, line: 1, note: '', raw: '' }] })
    const out = verifyLocations(
      [
        'lib/escape.txt',
        'escape-dir-target',
        'up/outside.txt',
        'lib/inside.txt',
        'lib/alias.txt',
      ].map(at),
      { root },
    )
    assert.deepEqual(
      out.map((r) => r.locations[0].reason),
      ['outside-root', 'outside-root', 'outside-root', null, null],
    )
    // A root reached through a symlink still contains its own files.
    const linkedRoot = path.join(tmp, 'linked-repo')
    fs.symlinkSync(root, linkedRoot)
    const [viaLink] = verifyLocations([at('lib/inside.txt')], { root: linkedRoot })
    assert.equal(viaLink.locationStatus, 'location-verified')
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true })
  }
})

test('verifyLocations leaves a preset locationStatus untouched', () => {
  const preset = record({
    source: 'history-scan',
    lensFamily: 'secret-history',
    locations: [{ path: 'gone/file.env', line: 1, note: '', raw: '' }],
    locationStatus: 'location-verified',
    evidence: { blob: 'synthetic-blob' },
  })
  const [out] = verifyLocations([preset], {
    root: ROOT,
    readFile: memoryReader({}),
    realpath: memoryRealpath({}),
  })
  assert.deepEqual(out, preset)
})

test('normaliseRecord trusts a preset locationStatus only from the history scan', () => {
  const input = {
    title: 'Gizmo token is logged',
    severity: 'high',
    locations: [{ path: 'lib/gizmo/gone.go', line: 1 }],
    locationStatus: 'location-verified',
    ledger: { status: 'known', issues: ['SYN-1'], reason: 'claimed' },
    reporters: [{ source: 'claude', lens: 'forged' }],
  }
  const verify = (rec) =>
    verifyLocations([rec], {
      root: ROOT,
      readFile: memoryReader({}),
      realpath: memoryRealpath({}),
    })[0]

  const codex = normaliseRecord(input, { source: 'codex' })
  assert.equal(codex.locationStatus, undefined)
  assert.equal(codex.ledger, undefined)
  assert.equal(codex.reporters, undefined)
  assert.equal(verify(codex).locationStatus, 'location-unverified')

  // A record in the Codex file cannot claim the history scan's trust.
  const spoofed = normaliseRecord({ ...input, source: 'history-scan' }, { source: 'codex' })
  assert.equal(spoofed.locationStatus, undefined)
  assert.equal(verify(spoofed).locationStatus, 'location-unverified')

  const scan = normaliseRecord(input, { source: 'history-scan', trustLocationStatus: true })
  assert.equal(scan.locationStatus, 'location-verified')
  assert.equal(scan.ledger, undefined)
  assert.equal(verify(scan).locationStatus, 'location-verified')

  const bogus = normaliseRecord(
    { ...input, locationStatus: 'yes' },
    { source: 'history-scan', trustLocationStatus: true },
  )
  assert.equal(bogus.locationStatus, undefined)
})

test('normaliseRecord trusts locationStatus only on the caller option, never a self-claim', () => {
  const input = {
    source: 'history-scan',
    title: 'Gizmo token is logged',
    severity: 'high',
    locations: [{ path: 'lib/gizmo/gone.go', line: 1 }],
    locationStatus: 'location-verified',
  }
  // No caller default at all: the record's own source claim earns no trust.
  assert.equal(normaliseRecord(input).locationStatus, undefined)
  // A history-scan source without the explicit option earns none either.
  assert.equal(normaliseRecord(input, { source: 'history-scan' }).locationStatus, undefined)
})

test('normaliseRecord takes provenance from the caller, not the record', () => {
  const honest = normaliseRecord(
    { title: 'Widget delete skips the owner check', severity: 'high', locations: [] },
    { source: 'codex', lens: 'codex', lensFamily: 'cross-model' },
  )
  // A Codex-file record claiming a Claude lens must not move into that lens's
  // fingerprint namespace, where a filed marker would classify it as known.
  const claimed = normaliseRecord(
    {
      source: 'claude',
      lens: 'authz-widgets',
      lensFamily: 'authz',
      title: 'Widget delete skips the owner check',
      severity: 'high',
      locations: [],
    },
    { source: 'codex', lens: 'codex', lensFamily: 'cross-model' },
  )
  assert.equal(claimed.source, 'codex')
  assert.equal(claimed.lens, 'codex')
  assert.equal(claimed.lensFamily, 'cross-model')
  assert.equal(claimed.fingerprint, honest.fingerprint)
  // A caller that names only the source still decides the lens and family.
  const sourceOnly = normaliseRecord(
    { lens: 'authz-widgets', lensFamily: 'authz', title: 't', severity: 'high' },
    { source: 'codex' },
  )
  assert.equal(sourceOnly.lens, 'codex')
  assert.equal(sourceOnly.lensFamily, 'cross-model')
})

test('normaliseRecord keeps a non-integer JSON line so verification rejects it', () => {
  const at = (line) =>
    normaliseRecord(
      { title: 't', severity: 'high', locations: [{ path: 'services/widget/handler.go', line }] },
      { source: 'codex', root: ROOT },
    )
  const verify = (rec) =>
    verifyLocations([rec], {
      root: ROOT,
      readFile: memoryReader(FILES),
      realpath: memoryRealpath(FILES),
    })[0]
  assert.equal(at('3').locations[0].line, 3)
  assert.equal(verify(at('3')).locationStatus, 'location-verified')
  for (const line of [undefined, null, '']) assert.equal(at(line).locations[0].line, null)
  for (const line of ['999999', 1.5, 'abc', -2]) {
    const out = verify(at(line))
    assert.equal(out.locations[0].reason, 'line-out-of-range', `line ${line}`)
    assert.equal(out.locationStatus, 'location-unverified', `line ${line}`)
  }
})

test('normaliseRecord builds locations and the record from an allowlist', () => {
  const rec = normaliseRecord(
    {
      title: 'Gizmo token is logged',
      severity: 'high',
      locations: [
        {
          path: 'lib/gizmo/gone.go',
          line: 1,
          note: 'n',
          verified: true,
          reason: null,
          outsideRoot: true,
          extra: 'x',
        },
      ],
      locationStatus: 'location-verified',
      suppressed: true,
      severityUnparsed: true,
      evidence: { blob: 'synthetic-blob' },
    },
    { source: 'history-scan', trustLocationStatus: true, root: ROOT },
  )
  // Per-location verdicts are never taken from input, even when the record's
  // status is trusted (verifyLocations then leaves the record untouched).
  assert.deepEqual(rec.locations, [
    { path: 'lib/gizmo/gone.go', line: 1, note: 'n', raw: 'lib/gizmo/gone.go:1' },
  ])
  assert.equal(rec.suppressed, undefined)
  assert.equal(rec.severityUnparsed, undefined)
  assert.deepEqual(rec.evidence, { blob: 'synthetic-blob' })
  assert.equal(rec.locationStatus, 'location-verified')
  const [verified] = verifyLocations([rec], {
    root: ROOT,
    readFile: memoryReader({}),
    realpath: memoryRealpath({}),
  })
  assert.equal(verified.locations[0].verified, undefined)
  // outsideRoot is re-derived from the path, not copied.
  const outside = normaliseRecord(
    { title: 't', severity: 'high', locations: [{ path: '/elsewhere/x.go', line: 1 }] },
    { source: 'codex', root: ROOT },
  )
  assert.equal(outside.locations[0].outsideRoot, true)
})

test('mergeFindings keeps every source and the maximum severity', () => {
  const a = {
    ...record({ lens: 'authz-a', severity: 'medium', confidence: 'confirmed-by-reading' }),
    locationStatus: 'location-unverified',
  }
  const b = {
    ...record({
      lens: 'authz-b',
      severity: 'high',
      severityRaw: 'high (threat model b)',
      defect: 'Text from the high-severity member.',
      confidence: 'plausible',
      locations: [{ path: 'services/widget/handler.go', line: 44, note: '', raw: '' }],
    }),
    locationStatus: 'location-verified',
  }
  const c = {
    ...record({
      title: 'Gizmo store joins request paths',
      severity: 'low',
      locations: [{ path: 'lib/gizmo/store.go', line: 1, note: '', raw: '' }],
    }),
    locationStatus: 'location-unverified',
  }
  assert.equal(a.fingerprint, b.fingerprint)
  const merged = mergeFindings([c, a, b])
  assert.equal(merged.length, 2)
  const [top, bottom] = merged
  assert.equal(top.fingerprint, a.fingerprint)
  assert.deepEqual(top.reporters, [
    { source: 'claude', lens: 'authz-a' },
    { source: 'claude', lens: 'authz-b' },
  ])
  assert.equal(top.severity, 'high')
  assert.equal(top.severityRaw, 'high (threat model b)')
  assert.equal(top.defect, 'Text from the high-severity member.')
  assert.equal(top.confidence, 'confirmed-by-reading')
  assert.equal(top.locationStatus, 'location-verified')
  assert.deepEqual(
    top.locations.map((l) => l.line),
    [44, 42],
  )
  assert.equal(bottom.severity, 'low')
  // Byte-stable regardless of input order.
  assert.equal(JSON.stringify(mergeFindings([b, a, c])), JSON.stringify(merged))
})

test('nearDuplicateCandidates pairs same path and class with different fingerprints', () => {
  const claude = record()
  const codex = record({ source: 'codex', lens: 'codex', lensFamily: 'cross-model' })
  const elsewhere = record({
    locations: [{ path: 'lib/gizmo/store.go', line: 1, note: '', raw: '' }],
  })
  assert.deepEqual(nearDuplicateCandidates([claude, codex, elsewhere]), [
    {
      fingerprints: [claude.fingerprint, codex.fingerprint].sort(),
      primaryPath: 'services/widget/handler.go',
      defectClass: 'authz',
    },
  ])
})

test('parseSecurityMarkers lifts line-anchored markers and markerLine writes them', () => {
  const fp = 'sf-0123456789ab'
  assert.equal(markerLine(fp), `${SECURITY_MARKER_PREFIX}${fp}`)
  assert.throws(() => markerLine('not-a-fingerprint'), /fingerprint/)
  const description = [
    'Body text mentioning SecurityFinding: sf-ffffffffffff inline',
    markerLine(fp),
    'SecurityFinding: sf-aaaaaaaaaaaa   ',
    'SecurityFinding: sf-0123456789ab',
  ].join('\n')
  assert.deepEqual(parseSecurityMarkers(description), [fp, 'sf-aaaaaaaaaaaa'])
  assert.deepEqual(parseSecurityMarkers(undefined), [])
})

test('classifyAgainstLedger applies the state policy', () => {
  const fps = Array.from({ length: 6 }, (_, i) => `sf-00000000000${i}`)
  const recs = fps.map((fp) => ({ ...record(), fingerprint: fp }))
  const issue = (identifier, fp, type, name = type) => ({
    identifier,
    title: `t ${identifier}`,
    description: `prose\n${markerLine(fp)}\n`,
    state: { type, name },
  })
  const ledger = [
    issue('SYN-1', fps[0], 'started', 'In Progress'),
    issue('SYN-2', fps[1], 'canceled', 'Canceled'),
    issue('SYN-3', fps[2], 'canceled', 'Duplicate'),
    issue('SYN-4', fps[3], 'completed', 'Done'),
    issue('SYN-5', fps[4], 'completed', 'Done'),
    issue('SYN-6', fps[4], 'unstarted', 'Todo'),
  ]
  const out = classifyAgainstLedger(recs, ledger)
  const table = [
    ['started', 'known', ['SYN-1']],
    ['canceled', 'known', ['SYN-2']],
    ['Duplicate', 'known', ['SYN-3']],
    ['completed', 'regression-candidate', ['SYN-4']],
    ['completed plus open', 'known', ['SYN-5', 'SYN-6']],
    ['none', 'new', []],
  ]
  table.forEach(([label, status, issues], i) => {
    assert.equal(out[i].ledger.status, status, label)
    assert.deepEqual(out[i].ledger.issues, issues, label)
    assert.equal(typeof out[i].ledger.reason, 'string', label)
  })
  assert.equal(recs[0].ledger, undefined, 'inputs are not mutated')

  const drifted = [{ identifier: 'SYN-7', title: 'x', description: markerLine(fps[5]) }]
  assert.throws(() => classifyAgainstLedger(recs, drifted), /state/)
})

test('fetchSecurityLedger scans the SecurityFinding marker with the ledger query', async () => {
  const calls = []
  const node = {
    identifier: 'SYN-1',
    title: 't',
    description: markerLine('sf-0123456789ab'),
    state: { type: 'started', name: 'In Progress' },
  }
  const linearRequest = async ({ query, variables }) => {
    calls.push({ query, variables })
    return { issues: { nodes: [node], pageInfo: { hasNextPage: false, endCursor: null } } }
  }
  assert.deepEqual(await fetchSecurityLedger({ apiKey: 'lk', linearRequest }), [node])
  assert.equal(calls[0].query, SECURITY_LEDGER_QUERY)
  assert.deepEqual(calls[0].variables.filter, { description: { contains: SECURITY_MARKER_PREFIX } })

  const stateless = async () => ({
    issues: {
      nodes: [{ identifier: 'SYN-2', title: 't', description: '' }],
      pageInfo: { hasNextPage: false, endCursor: null },
    },
  })
  await assert.rejects(fetchSecurityLedger({ apiKey: 'lk', linearRequest: stateless }), /no state/)
})

test('renderKnownList lists open and canceled issues and omits completed ones', () => {
  const ledger = [
    {
      identifier: 'SYN-10',
      title: 'Gizmo store joins request paths',
      description: `See https://example.invalid/a/b.html and lib/gizmo/store.go:17 for detail.\n${markerLine('sf-111111111111')}`,
      state: { type: 'canceled', name: 'Canceled' },
    },
    {
      identifier: 'SYN-2',
      title: 'Widget handler skips the owner check',
      description: `In \`services/widget/handler.go:42\`.\n${markerLine('sf-222222222222')}`,
      state: { type: 'started', name: 'In Progress' },
    },
    {
      identifier: 'SYN-3',
      title: 'Fixed long ago',
      description: `services/old/file.go:1\n${markerLine('sf-333333333333')}`,
      state: { type: 'completed', name: 'Done' },
    },
  ]
  const extra = [
    { identifier: 'SYN-20', title: 'Epic child without markers', description: 'No paths here.' },
    { identifier: 'SYN-2', title: 'Duplicate of a ledger issue', description: '' },
  ]
  assert.equal(
    renderKnownList(ledger, extra),
    [
      '- SYN-2: Widget handler skips the owner check (services/widget/handler.go:42)',
      '- SYN-10: Gizmo store joins request paths (lib/gizmo/store.go:17) [accepted risk]',
      '- SYN-20: Epic child without markers',
    ].join('\n'),
  )
  assert.equal(renderKnownList([], []), '')
})

function runCliProcess(args, opts = {}) {
  return spawnSync(process.execPath, [SCRIPT, ...args], { encoding: 'utf8', ...opts })
}

test('collect writes merged.json and prints the counts', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-security-findings-'))
  try {
    const root = path.join(tmp, 'repo')
    const runDir = path.join(tmp, 'run')
    fs.mkdirSync(path.join(root, 'services/widget'), { recursive: true })
    fs.writeFileSync(
      path.join(root, 'services/widget/handler.go'),
      FILES[`${ROOT}/services/widget/handler.go`],
    )
    fs.mkdirSync(path.join(runDir, RUN_LAYOUT.findingsDir), { recursive: true })
    fs.mkdirSync(path.join(runDir, 'codex'), { recursive: true })
    // Two lenses of one family report the same defect; a third lens is not in the lenses file.
    fs.writeFileSync(path.join(runDir, 'findings/authz-widgets.md'), CLAUDE_REPORT)
    fs.writeFileSync(
      path.join(runDir, 'findings/authz-routes.md'),
      CLAUDE_REPORT.replace('handler.go:42', 'handler.go:45').replace(
        'Severity: medium',
        'Severity: high',
      ),
    )
    fs.writeFileSync(
      path.join(runDir, 'findings/unlisted.md'),
      `### Gizmo cache leaks a token\n\n- Severity: low\n- Location: lib/gizmo/cache.go:3\n- Issue: Token is logged.\n`,
    )
    fs.writeFileSync(
      path.join(runDir, RUN_LAYOUT.codexFindings),
      JSON.stringify([
        {
          title: 'Widget delete is missing an ownership check',
          severityRaw: 'high',
          // Absolute inside the root: collect must hand the root to normaliseRecord.
          locations: [{ path: `${root}/services/widget/handler.go`, line: 42 }],
          defect: 'No authorization check before delete.',
        },
      ]),
    )
    const lensesFile = path.join(tmp, 'lenses.json')
    fs.writeFileSync(
      lensesFile,
      JSON.stringify({ 'authz-widgets': 'authz', 'authz-routes': 'authz' }),
    )

    // Learn the merged Claude fingerprint, then file it as a completed ledger issue.
    const parsed = runCliProcess([
      'parse',
      path.join(runDir, 'findings/authz-widgets.md'),
      '--source',
      'claude',
      '--lens',
      'authz-widgets',
      '--lens-family',
      'authz',
      '--root',
      root,
    ])
    assert.equal(parsed.status, 0, parsed.stderr)
    const claudeFp = JSON.parse(parsed.stdout).records[0].fingerprint
    const ledgerFile = path.join(tmp, 'ledger.json')
    fs.writeFileSync(
      ledgerFile,
      JSON.stringify([
        {
          identifier: 'SYN-1',
          title: 'Widget owner check',
          description: markerLine(claudeFp),
          state: { type: 'completed', name: 'Done' },
        },
      ]),
    )

    const args = [
      'collect',
      '--run-dir',
      runDir,
      '--lenses',
      lensesFile,
      '--ledger',
      ledgerFile,
      '--root',
      root,
    ]
    const res = runCliProcess(args)
    assert.equal(res.status, 0, res.stderr)
    assert.match(res.stderr, /unlisted/)
    assert.deepEqual(JSON.parse(res.stdout), {
      parsed: 4,
      skippedBlocks: 6,
      merged: 3,
      new: 2,
      known: 0,
      regressionCandidates: 1,
      locationUnverified: 1,
      nearDuplicatePairs: 1,
    })
    const mergedPath = path.join(runDir, RUN_LAYOUT.merged)
    const first = fs.readFileSync(mergedPath, 'utf8')
    const doc = JSON.parse(first)
    assert.equal(doc.schemaVersion, RECORD_SCHEMA_VERSION)
    assert.equal(doc.records.length, 3)
    const claude = doc.records.find((r) => r.fingerprint === claudeFp)
    assert.equal(claude.severity, 'high')
    assert.equal(claude.ledger.status, 'regression-candidate')
    assert.equal(claude.locationStatus, 'location-verified')
    const codex = doc.records.find((r) => r.source === 'codex')
    assert.equal(codex.lensFamily, 'cross-model')
    assert.equal(codex.primaryPath, 'services/widget/handler.go')
    assert.equal(codex.locationStatus, 'location-verified')
    assert.equal(doc.records.find((r) => r.lens === 'unlisted').lensFamily, 'unlisted')

    // Reruns are byte-stable.
    assert.equal(runCliProcess(args).status, 0)
    assert.equal(fs.readFileSync(mergedPath, 'utf8'), first)
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true })
  }
})

test('CLI rejects unknown flags and missing inputs with exit 2', () => {
  const unknown = runCliProcess(['collect', '--run-dir', '/nonexistent', '--bogus', 'x'])
  assert.equal(unknown.status, 2)
  const missing = runCliProcess(['known-list'])
  assert.equal(missing.status, 2)
  assert.match(missing.stderr, /--ledger/)
  const noVerb = runCliProcess([])
  assert.equal(noVerb.status, 2)
  const noKey = runCliProcess(['ledger', '--out', path.join(os.tmpdir(), 'never-written.json')], {
    env: { ...process.env, LINEAR_API_KEY: '' },
  })
  assert.equal(noKey.status, 2)
  assert.match(noKey.stderr, /LINEAR_API_KEY/)
})

test('a physical absolute path under a symlinked root is repo-relative', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-security-findings-'))
  try {
    const real = path.join(tmp, 'real')
    fs.mkdirSync(path.join(real, 'lib'), { recursive: true })
    fs.writeFileSync(path.join(real, 'lib/a.js'), 'x\n')
    const root = path.join(tmp, 'link')
    fs.symlinkSync(real, root)
    const physical = `${fs.realpathSync(real)}/lib/a.js`
    const title = 'Gizmo store joins request paths'
    const fromMarkdown = (loc) =>
      parseFindingsMarkdown(`### ${title}\n\n- Severity: high\n- Location: ${loc}\n`, {
        source: 'codex',
        root,
      }).records[0]
    const fromJson = (p) =>
      normaliseRecord(
        { title, severity: 'high', locations: [{ path: p, line: 1 }] },
        { source: 'codex', root },
      )
    const records = [
      fromMarkdown('lib/a.js:1'),
      fromMarkdown(`${physical}:1`),
      fromJson('lib/a.js'),
      fromJson(physical),
    ]
    const verified = verifyLocations(records, { root })
    assert.deepEqual(
      verified.map((r) => [r.locations[0].path, r.locations[0].outsideRoot, r.locationStatus]),
      Array(4).fill(['lib/a.js', undefined, 'location-verified']),
    )
    assert.equal(new Set(records.map((r) => r.fingerprint)).size, 1)
    // A sibling of the physical root is still outside it.
    const sibling = fromJson(`${fs.realpathSync(tmp)}/other/lib/a.js`)
    assert.equal(sibling.locations[0].outsideRoot, true)
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true })
  }
})

test('verifyLocations never reads a target whose realpath failed', () => {
  let reads = 0
  const [out] = verifyLocations([record({ locations: [{ path: 'lib/a.js', line: 1 }] })], {
    root: ROOT,
    realpath: () => {
      throw new Error('ENOENT')
    },
    readFile: () => {
      reads += 1
      return 'matching content'
    },
  })
  assert.equal(out.locations[0].reason, 'missing-file')
  assert.equal(reads, 0)
})

test('verifyLocations reads the exact resolved path checked for containment', () => {
  const reads = []
  const resolved = `${ROOT}/real/a.js`
  const [out] = verifyLocations(
    [record({ locations: [{ path: 'lib/a.js', line: 1, note: '`matching`' }] })],
    {
      root: ROOT,
      realpath: (p) => (p === `${ROOT}/lib/a.js` ? resolved : p),
      readFile: (p) => {
        reads.push(p)
        return p === resolved ? 'matching content' : 'wrong content'
      },
    },
  )
  assert.equal(out.locationStatus, 'location-verified')
  assert.deepEqual(reads, [resolved])
})

for (const fence of ['```', '~~~']) {
  test(`parseFindingsMarkdown keeps headings inside ${fence} samples in their field`, () => {
    const report = [
      '## Synthetic finding',
      '- Severity: high',
      '- Location: lib/a.js:1',
      '- Fix: Run this:',
      `${fence}sh`,
      '# comment',
      '## step two',
      'echo safe',
      fence,
      '- Confidence: likely',
    ].join('\n')
    const { records, skipped } = parseFindingsMarkdown(report)
    assert.equal(records.length, 1)
    assert.equal(records[0].confidence, 'likely')
    assert.match(records[0].fix, /# comment ## step two/)
    assert.deepEqual(skipped, [])
  })
}

test('parseFindingsMarkdown keeps YAML labels and text after a fenced defect sample', () => {
  const { records } = parseFindingsMarkdown(
    [
      '## Synthetic finding',
      '- Severity: high',
      '- Location: lib/a.js:1',
      '- Defect: Before sample.',
      '```yaml',
      '- key: value',
      '```',
      'After sample.',
    ].join('\n'),
  )
  assert.equal(records[0].defect, 'Before sample. ```yaml - key: value ``` After sample.')
})

test('parseFindingsMarkdown leaves unclosed and severity-bearing fences transparent', () => {
  const finding = (title) => `## ${title}\n- Severity: high\n- Location: lib/a.js:1\n`
  for (const report of [
    `${finding('First')}\n\x60\x60\x60sh\n${finding('Second')}`,
    `\x60\x60\x60markdown\n${finding('First')}\n${finding('Second')}\x60\x60\x60`,
  ]) {
    const { records } = parseFindingsMarkdown(report)
    assert.deepEqual(
      records.map((r) => r.title),
      ['First', 'Second'],
    )
  }
})
