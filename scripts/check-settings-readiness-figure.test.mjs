#!/usr/bin/env node

// Coverage for scripts/check-settings-readiness-figure.mjs, plus the line pin on
// the Go doc comment the gate replaced a manual sync instruction in.
//
// Every failure case here is fed the gate the values it exists to reject (see
// docs/solutions/best-practices/feed-a-new-gate-the-values-it-exists-to-reject.md):
// a wrong figure, a wrong default, a second copy of the sentence, no copy at
// all, a moved heading, a moved page, and a reformatted constant. A gate only
// tested on its happy path is a gate whose red branch nobody has run.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  DOCS_SITE_DIR,
  GO_CONSTANT_SPECS,
  REGION_END,
  REGION_START,
  SESSION_START_DECLARATION,
  SETTINGS_PAGE,
  checkSettingsReadinessFigure,
  computeWorstCaseSeconds,
  extractDocCommentLines,
  parseAttempts,
  parseDeadlineSeconds,
  parseGoConstant,
  parseProbeSeconds,
  readGoFigures,
} from './check-settings-readiness-figure.mjs'
import { assertExactSize } from './size-ratchet-lib.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// The pinned line count of the SessionStartReadyDeadline doc comment in
// lib/bossalib/config/config.go.
//
// BOS-949 rewrote that comment from 92 lines of accumulated revision history
// into a present-tense contract. The pin exists because the same accretion had
// already happened twice: five tickets each appended a paragraph correcting the
// previous one, and no gate ever noticed the comment doubling. Two-sided, so a
// later trim has to be banked rather than left as headroom the next ticket
// regrows into.
//
// Re-pinned 69 -> 71 by BOS-948, which is the banked-rewrite case this file's
// GO_DOC_COMMENT_REMEDY allows rather than the regrown-history case it refuses.
// BOS-948 made the switch respawn budget a derivation over this setting instead
// of a compiled constant, so "raising this raises that budget with it, and the
// budget funds one full attempt at every configured value rather than only at
// the default" became live contract for THIS accessor: its unclamped promise
// previously stopped holding on the switch route past roughly 88 configured
// seconds. Two lines, folded into the existing WHAT BOUNDS THE SPENDERS FROM
// ABOVE paragraph rather than appended as a new one, and no revision history
// was restored — the 92-line version BOS-949 removed stays removed.
const SESSION_START_COMMENT_LINES = 71

// Remedy for the over case, written for a Go doc comment: there is no
// references/ directory to extract into, and "trim prose" is the wrong
// instruction for a comment whose job is to state a contract.
const GO_DOC_COMMENT_REMEDY =
  'A doc comment states the contract that is live. History — why the value moved, which race ' +
  'closed, what a previous revision claimed — belongs in docs/plans/ and docs/solutions/, ' +
  'which already carry it; do not raise the pin to make room for another paragraph of it. ' +
  'Banking a rewrite is the OTHER case and is allowed: when live contract material genuinely ' +
  "arrives — a merge folding in a sibling branch's substantive changes, or a paragraph the " +
  'code now requires — re-pin in the same commit and say on the constant what was added. The ' +
  'pin refuses regrown history, not a larger contract.'

// Capture what the gate prints so a test can assert on the failure text a
// contributor actually sees, not just the boolean.
function captureConsole(run) {
  const out = []
  const err = []
  const originalLog = console.log
  const originalError = console.error
  console.log = (...args) => out.push(args.join(' '))
  console.error = (...args) => err.push(args.join(' '))
  try {
    const result = run()
    return { result, out: out.join('\n'), err: err.join('\n') }
  } finally {
    console.log = originalLog
    console.error = originalError
  }
}

function configGo(deadlineSeconds) {
  return [
    'package config',
    '',
    'const (',
    '\tDefaultSessionStartReadyDeadline = ' + deadlineSeconds + ' * time.Second',
    '\tDefaultSendReadyDeadline         = 5 * time.Second',
    '\tSendReadyDeadlineMax             = 20 * time.Second',
    ')',
    '',
    '// A doc comment above the declaration the line pin measures.',
    '// Second line of it.',
    SESSION_START_DECLARATION,
    '\treturn DefaultSessionStartReadyDeadline',
    '}',
    '',
  ].join('\n')
}

// Carries all three spellings of the identifier on purpose: the const
// declaration the parser must read, the struct field, and the option setter's
// qualified assignment — with a NUMERIC right-hand side, so the digit
// requirement alone cannot be what excludes it.
function tmuxGo(attempts) {
  return [
    'package tmux',
    '',
    'const (',
    '\tsendPlanDefaultReadyAttempts = 1',
    '\tsessionStartReadyAttempts = ' + attempts,
    '\tsendReadyAttempts = 1',
    ')',
    '',
    'type Client struct {',
    '\tsessionStartReadyAttempts int',
    '}',
    '',
    'func WithReadyAttempts() Option {',
    '\treturn func(c *Client) {',
    '\t\tc.sessionStartReadyAttempts = 7',
    '\t}',
    '}',
    '',
  ].join('\n')
}

function tmuxModalGo(probeSeconds) {
  return [
    'package tmux',
    '',
    'const modalProbeTimeout = ' + probeSeconds + ' * time.Second',
    '',
  ].join('\n')
}

// The distractors that legitimately share the tmux_delivery section: the 20s
// send clamp, the 30s relay bound, the 90-second switch budget, the "twelve
// seconds" shell-init measurement, and the `shortened from 45s` timeout message.
// A bare seconds scan would red on every one of them.
const REGION_DISTRACTORS = [
  'Raise the deadline when sessions fail to start on a host with a slow shell',
  'profile — measured login-shell init alone has ranged from under a second to',
  'twelve seconds on affected machines.',
  '',
  'The send deadline is **clamped to 20 seconds**, because that delivery runs',
  'inside a request the cloud relay bounds at 30 seconds.',
  '',
  'A switch respawns the pane and bossanova gives that whole operation a fixed',
  '90-second budget of its own, sized to fund one full attempt at the',
  '45-second default.',
  '',
  'The timeout message says so: _shortened from 45s to stay inside the',
  "caller's context_.",
].join('\n')

function worstCaseSentence(worstCase, deadline) {
  return (
    'It is a **per-attempt** budget, not a total, so a start that is genuinely ' +
    'going to fail spends about ' +
    worstCase +
    ' seconds at the default of ' +
    deadline +
    ' before it reports failure.'
  )
}

function settingsPage(regionBody) {
  return [
    '# Settings File',
    '',
    '## `repair` fields',
    '',
    'An unrelated earlier section.',
    '',
    REGION_START,
    '',
    regionBody,
    '',
    REGION_END,
    '',
    'An unrelated later section.',
    '',
  ].join('\n')
}

function writeInto(root, relativePath, contents) {
  const target = path.join(root, relativePath)
  fs.mkdirSync(path.dirname(target), { recursive: true })
  fs.writeFileSync(target, contents)
}

function makeRepo(options = {}) {
  const {
    deadline = 45,
    attempts = 2,
    probe = 2,
    docsSite = true,
    page = settingsPage([REGION_DISTRACTORS, '', worstCaseSentence(94, 45)].join('\n')),
  } = options

  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'settings-readiness-'))
  writeInto(root, GO_CONSTANT_SPECS.deadlineSeconds.file, configGo(deadline))
  writeInto(root, GO_CONSTANT_SPECS.attempts.file, tmuxGo(attempts))
  writeInto(root, GO_CONSTANT_SPECS.probeSeconds.file, tmuxModalGo(probe))
  if (docsSite) {
    fs.mkdirSync(path.join(root, DOCS_SITE_DIR), { recursive: true })
    if (page !== null) writeInto(root, SETTINGS_PAGE, page)
  }
  return root
}

function withRepo(t, options) {
  const root = makeRepo(options)
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  return root
}

test('parseDeadlineSeconds reads the default from fabricated config.go source', () => {
  assert.equal(parseDeadlineSeconds(configGo(45)), 45)
  assert.equal(parseDeadlineSeconds(configGo(120)), 120)
})

test('parseAttempts reads the const declaration from fabricated tmux.go source', () => {
  assert.equal(parseAttempts(tmuxGo(2)), 2)
  assert.equal(parseAttempts(tmuxGo(5)), 5)
})

test('parseProbeSeconds reads the modal probe timeout from fabricated source', () => {
  assert.equal(parseProbeSeconds(tmuxModalGo(2)), 2)
  assert.equal(parseProbeSeconds(tmuxModalGo(9)), 9)
})

test('parseAttempts ignores the struct field and the qualified assignment target', () => {
  // The fixture's setter assigns 7 to `c.sessionStartReadyAttempts`, so a parser
  // that only required digits would find two matches and report the wrong one.
  const source = tmuxGo(2)
  assert.ok(source.includes('c.sessionStartReadyAttempts = 7'))
  assert.ok(source.includes('\tsessionStartReadyAttempts int'))
  assert.equal(parseAttempts(source), 2)
})

test('parseDeadlineSeconds fails loudly naming file and symbol when the pattern misses', () => {
  const reformatted = 'package config\n\nconst DefaultSessionStartReadyDeadline = time.Minute\n'
  assert.throws(
    () => parseDeadlineSeconds(reformatted),
    (error) => {
      assert.match(error.message, /DefaultSessionStartReadyDeadline/)
      assert.match(error.message, /config\.go/)
      return true
    },
  )
})

test('parseAttempts fails loudly naming file and symbol when the pattern misses', () => {
  const renamed = 'package tmux\n\nconst startReadyAttempts = 2\n'
  assert.throws(
    () => parseAttempts(renamed),
    (error) => {
      assert.match(error.message, /sessionStartReadyAttempts/)
      assert.match(error.message, /tmux\.go/)
      return true
    },
  )
})

test('parseProbeSeconds fails loudly naming file and symbol when the pattern misses', () => {
  const reformatted = 'package tmux\n\nconst modalProbeTimeout = 2000 * time.Millisecond\n'
  assert.throws(
    () => parseProbeSeconds(reformatted),
    (error) => {
      assert.match(error.message, /modalProbeTimeout/)
      assert.match(error.message, /tmux_modal\.go/)
      return true
    },
  )
})

test('parseGoConstant refuses an ambiguous second declaration', () => {
  const doubled = tmuxModalGo(2) + tmuxModalGo(3)
  assert.throws(
    () => parseGoConstant(doubled, GO_CONSTANT_SPECS.probeSeconds),
    /Ambiguous modalProbeTimeout/,
  )
  // The exported wrapper is the same refusal, so a caller cannot reach the
  // first-match-wins behaviour by going through it instead.
  assert.throws(() => parseProbeSeconds(doubled), /Ambiguous modalProbeTimeout/)
})

test('computeWorstCaseSeconds multiplies attempts over deadline plus probe', () => {
  assert.equal(computeWorstCaseSeconds({ attempts: 2, deadlineSeconds: 45, probeSeconds: 2 }), 94)
  assert.equal(computeWorstCaseSeconds({ attempts: 3, deadlineSeconds: 10, probeSeconds: 1 }), 33)
})

test('readGoFigures fails loudly naming file and symbol when a Go source is missing', (t) => {
  const root = withRepo(t)
  fs.rmSync(path.join(root, GO_CONSTANT_SPECS.attempts.file))
  assert.throws(
    () => readGoFigures(root),
    (error) => {
      assert.match(error.message, /sessionStartReadyAttempts/)
      assert.match(error.message, /tmux\.go/)
      return true
    },
  )
})

test('extractDocCommentLines returns the comment above the declaration', () => {
  const lines = extractDocCommentLines(configGo(45), SESSION_START_DECLARATION)
  assert.equal(lines.length, 2)
  assert.match(lines[0], /A doc comment above the declaration/)
})

test('extractDocCommentLines throws when the declaration marker moves', () => {
  assert.throws(
    () => extractDocCommentLines(configGo(45), 'func (c TmuxDeliveryConfig) Renamed() {'),
    /Declaration not found/,
  )
})

test('a checkout with no services/docs passes with nothing to check', (t) => {
  const root = withRepo(t, { docsSite: false })
  const { result, out } = captureConsole(() => checkSettingsReadinessFigure(root))
  assert.equal(result.ok, true)
  assert.equal(result.counts.goConstantsParsed, 3)
  assert.equal(result.counts.docPagesChecked, 0)
  assert.match(out, /nothing to check/)
})

test('services/docs present but settings.md missing fails with a page-has-moved message', (t) => {
  const root = withRepo(t, { page: null })
  const { result, err } = captureConsole(() => checkSettingsReadinessFigure(root))
  assert.equal(result.ok, false)
  assert.match(err, /has moved/)
  assert.match(err, /SETTINGS_PAGE/)
})

test('a correct page passes and reports non-zero counts', (t) => {
  const root = withRepo(t)
  const { result, out } = captureConsole(() => checkSettingsReadinessFigure(root))
  assert.equal(result.ok, true, out)
  assert.deepEqual(result.counts, {
    goConstantsParsed: 3,
    docPagesChecked: 1,
    worstCaseSentencesChecked: 1,
  })
  assert.equal(result.figures.worstCaseSeconds, 94)
})

test('a wrong worst-case figure fails naming expected and found', (t) => {
  for (const documented of [93, 95]) {
    const root = withRepo(t, {
      page: settingsPage(worstCaseSentence(documented, 45)),
    })
    const { result, err } = captureConsole(() => checkSettingsReadinessFigure(root))
    assert.equal(result.ok, false)
    assert.match(err, new RegExp('page says ' + documented + 's'))
    assert.match(err, /compose 94s/)
  }
})

test('a wrong documented default deadline fails naming both readings', (t) => {
  const root = withRepo(t, { page: settingsPage(worstCaseSentence(94, 30)) })
  const { result, err } = captureConsole(() => checkSettingsReadinessFigure(root))
  assert.equal(result.ok, false)
  assert.match(err, /page says 30s/)
  assert.match(err, /DefaultSessionStartReadyDeadline is 45s/)
})

test('two worst-case sentences fail', (t) => {
  const root = withRepo(t, {
    page: settingsPage([worstCaseSentence(94, 45), '', worstCaseSentence(94, 45)].join('\n')),
  })
  const { result, err } = captureConsole(() => checkSettingsReadinessFigure(root))
  assert.equal(result.ok, false)
  assert.match(err, /found 2/)
  assert.match(err, /second copy/)
})

test('no worst-case sentence fails rather than passing over an empty match set', (t) => {
  const root = withRepo(t, { page: settingsPage(REGION_DISTRACTORS) })
  const { result, err } = captureConsole(() => checkSettingsReadinessFigure(root))
  assert.equal(result.ok, false)
  assert.match(err, /found 0/)
  assert.match(err, /unguarded number/)
})

test("the region's other second figures never trip the gate", (t) => {
  // The distractors are present in the default fixture, so this asserts the
  // discriminating property directly: remove the one real sentence and the gate
  // finds ZERO matches, which proves none of the 20/30/90/twelve/45 figures was
  // ever being counted as the worst-case sentence.
  const root = withRepo(t, { page: settingsPage(REGION_DISTRACTORS) })
  const { result } = captureConsole(() => checkSettingsReadinessFigure(root))
  assert.equal(result.ok, false)
  assert.equal(result.counts.worstCaseSentencesChecked, 0)

  const withSentence = withRepo(t, {
    page: settingsPage([REGION_DISTRACTORS, '', worstCaseSentence(94, 45)].join('\n')),
  })
  const passing = captureConsole(() => checkSettingsReadinessFigure(withSentence))
  assert.equal(passing.result.ok, true, passing.err)
  assert.equal(passing.result.counts.worstCaseSentencesChecked, 1)
})

test('a moved region heading fails naming the marker', (t) => {
  const page = settingsPage(worstCaseSentence(94, 45)).replace(
    REGION_END,
    '## `subagent_dispatch_policy`',
  )
  const root = withRepo(t, { page })
  const { result, err } = captureConsole(() => checkSettingsReadinessFigure(root))
  assert.equal(result.ok, false)
  assert.match(err, /end marker not found/)
  assert.match(err, /REGION_START\/REGION_END/)
})

test('the real repo passes with every success-line count above zero', () => {
  const { result, out, err } = captureConsole(() => checkSettingsReadinessFigure(REPO_ROOT))
  assert.equal(result.ok, true, err)

  // A gate that finds nothing looks identical to a gate that looked at nothing,
  // and the success line is where that ambiguity is read. Every count it prints
  // has to be a real number of things examined.
  const counts = Object.entries(result.counts)
  assert.ok(counts.length > 0, 'the gate reported no counts at all')
  for (const [name, value] of counts) {
    assert.ok(value > 0, `${name} is ${value} on the real repo, so that leg checked nothing`)
  }
  assert.match(out, /Settings readiness figure OK/)
})

test('the SessionStartReadyDeadline doc comment stays at its pinned line count', () => {
  const configPath = path.join(REPO_ROOT, GO_CONSTANT_SPECS.deadlineSeconds.file)
  const comment = extractDocCommentLines(
    fs.readFileSync(configPath, 'utf8'),
    SESSION_START_DECLARATION,
  )

  assertExactSize({
    constFile: 'scripts/check-settings-readiness-figure.test.mjs',
    constName: 'SESSION_START_COMMENT_LINES',
    expected: SESSION_START_COMMENT_LINES,
    label: 'SessionStartReadyDeadline doc comment',
    measured: comment.length,
    path: GO_CONSTANT_SPECS.deadlineSeconds.file,
    remedy: GO_DOC_COMMENT_REMEDY,
    residual:
      'what the lines SAY — a rewrite that reintroduces revision-history narration, or ' +
      'restates the worst-case figure as a literal, lands on the same line count and passes ' +
      'this pin untouched; the greps in the BOS-949 acceptance criteria are what cover that',
    unit: 'lines',
  })
})
