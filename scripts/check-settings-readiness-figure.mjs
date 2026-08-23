#!/usr/bin/env node

// Guardrail: the worst-case session-start readiness figure printed on the docs
// Settings reference page must equal the figure the Go constants actually
// compose. Three constants own that number and no two of them live in the same
// module:
//
//   DefaultSessionStartReadyDeadline  lib/bossalib/config/config.go
//   sessionStartReadyAttempts         services/bossd/internal/tmux/tmux.go
//   modalProbeTimeout                 services/bossd/internal/tmux/tmux_modal.go
//
// The readiness wait is re-run `sessionStartReadyAttempts` times and each
// attempt reserves `modalProbeTimeout` beyond the deadline for its ModalDetector
// call, so a doomed start costs attempts x (deadline + probe) of wall clock.
// The docs page quotes that product to operators.
//
// WHY A GATE AND NOT AN INSTRUCTION. The Go doc comment on
// SessionStartReadyDeadline used to close with a manual "keep this in step with
// settings.md and the attempt count" note, and the docs page drifted underneath
// it anyway: it sat four seconds below what config.go stated, and stayed wrong
// until BOS-897 hand-corrected it. A three-way sync instruction with no guard on
// one leg is a documented intention, not a check.
//
// DERIVED, NEVER PINNED. This file states no worst-case figure anywhere, in code
// or in prose. Writing today's number here — even in a comment — would make a
// fourth copy of the very value the gate exists to bind, and the fourth copy
// would go stale exactly like the third did. Run the gate to see the figure; its
// success line prints the whole composition.
//
// WHAT THIS DOES NOT PROVE.
//
//   1. It does not prove the composition rule is right. `attempts x (deadline +
//      probe)` is this gate's MODEL of waitForReadyMarkerWithAttempts
//      (services/bossd/internal/tmux/ready_marker.go), and that model has no
//      executable Go twin: tmux_modal_test.go bounds one probe,
//      tmux_ready_retry_test.go pins the attempt count, and
//      TestDeliveryDeadline_MatchesConfigDefaults pins the duplicated default,
//      but nothing in Go composes the three. Change the retry's SHAPE — add
//      inter-attempt backoff, move the probe out of the poll loop, make the
//      reservation conditional — and this gate stays green while the docs page
//      silently goes wrong. Binding three constant VALUES is all it does.
//   2. It does not prove the rest of the tmux_delivery section is accurate. One
//      sentence shape is matched; the surrounding prose about clamps, relays and
//      switch budgets is unchecked.
//   3. It does not prove the figure is quoted anywhere ELSE correctly. Only the
//      region between the two headings named below is read, so the same number
//      written on another page, in a changelog, or in a Go comment is invisible
//      here.
//
// Exercised by scripts/check-settings-readiness-figure.test.mjs and runnable via
// `node scripts/check-settings-readiness-figure.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { regionUntilNext } from './gate-region-lib.mjs'

// The repo root relative to this file, so the gate works from any cwd — it runs
// from the repo root via `node scripts/check-settings-readiness-figure.mjs` and
// from `scripts/` via the scripts Makefile `lint` target.
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// Derived as a pair, exactly as check-docs-mcp-props.mjs derives its two, so the
// "no docs site in this checkout" and "the docs site is here but the page moved"
// cases can never be told apart by two literals that have drifted.
export const DOCS_SITE_DIR = path.join('services', 'docs')
export const SETTINGS_PAGE = path.join(DOCS_SITE_DIR, 'docs', 'reference', 'settings.md')

// The section of the Settings page that quotes the figure. The end marker is the
// LITERAL next heading rather than a generic heading prefix: regionUntilNext
// searches forward from the start marker, so a generic prefix would stop at the
// first later heading of any kind and a new section inserted between these two
// would silently narrow the region instead of throwing.
export const REGION_START = '## `tmux_delivery` fields'
export const REGION_END = '## `subagent_dispatch_grant`'

// The sentence shape that carries the figure — the page renders it as "about
// <worst case> seconds at the default of <deadline>". Both numbers are captured:
// the first must equal the computed product, the second the parsed deadline, so
// a page that updates one and not the other is still caught.
//
// Written with `\s+` between words, never literal spaces: the root .prettierrc
// leaves proseWrap at preserve, so an author rewrapping a neighbouring sentence
// can push a line break through the middle of this phrase without changing a
// word of it. A literal-space pin would go red naming no real defect.
export const WORST_CASE_SENTENCE = /about\s+(\d+)\s+seconds\s+at\s+the\s+default\s+of\s+(\d+)/g

// The Go constants the figure is composed from.
//
// Each pattern anchors on an ASSIGNMENT of a numeric literal and carries a
// `(?<![\w.$])` lookbehind. The lookbehind is load-bearing for
// sessionStartReadyAttempts specifically: that identifier appears three times in
// tmux.go — the const declaration, an identically-named struct field on the
// client, and `c.sessionStartReadyAttempts = n` in the option setter. Excluding a
// preceding `.` is what keeps the qualified field assignment out of the match,
// and the `\d` requirement keeps the bare field declaration out.
export const GO_CONSTANT_SPECS = {
  deadlineSeconds: {
    symbol: 'DefaultSessionStartReadyDeadline',
    file: path.join('lib', 'bossalib', 'config', 'config.go'),
    pattern: /(?<![\w.$])DefaultSessionStartReadyDeadline\s*=\s*(\d+)\s*\*\s*time\.Second/g,
    shape: 'NAME = <seconds> * time.Second',
  },
  attempts: {
    symbol: 'sessionStartReadyAttempts',
    file: path.join('services', 'bossd', 'internal', 'tmux', 'tmux.go'),
    pattern: /(?<![\w.$])sessionStartReadyAttempts\s*=\s*(\d+)(?![\w.])/g,
    shape: 'NAME = <count>',
  },
  probeSeconds: {
    symbol: 'modalProbeTimeout',
    file: path.join('services', 'bossd', 'internal', 'tmux', 'tmux_modal.go'),
    pattern: /(?<![\w.$])modalProbeTimeout\s*=\s*(\d+)\s*\*\s*time\.Second/g,
    shape: 'NAME = <seconds> * time.Second',
  },
}

/** Marker for the declaration whose doc comment the size pin measures. */
export const SESSION_START_DECLARATION =
  'func (c TmuxDeliveryConfig) SessionStartReadyDeadline() time.Duration {'

/**
 * Read one numeric Go constant out of `goSource`, fail-closed.
 *
 * Throws — naming BOTH the file and the symbol — when the pattern matches no
 * times or more than once. A regex over Go source stops matching silently when
 * the declaration is reformatted, and a gate whose input collapsed to nothing
 * passes on everything; naming the symbol is what turns that into a fixable
 * message rather than a puzzle.
 *
 * @param {string} goSource Whole Go file text.
 * @param {{symbol: string, file: string, pattern: RegExp, shape: string}} spec
 * @returns {number} The parsed literal.
 */
export function parseGoConstant(goSource, spec) {
  const { symbol, file, pattern, shape } = spec
  const matches = [...String(goSource).matchAll(pattern)]

  if (matches.length === 0) {
    throw new Error(
      `Cannot parse ${symbol} from ${file}: no declaration of the form "${shape}" matched. ` +
        'Either the constant was renamed or removed, or its declaration was reformatted past ' +
        'the pattern in GO_CONSTANT_SPECS (scripts/check-settings-readiness-figure.mjs). ' +
        'Fix the pattern rather than deleting the entry — an unparsed constant leaves the ' +
        'documented worst-case figure with nothing binding it.',
    )
  }
  if (matches.length > 1) {
    throw new Error(
      `Ambiguous ${symbol} in ${file}: ${matches.length} declarations of the form "${shape}" ` +
        'matched, so there is no single value to compose the worst-case figure from. Narrow ' +
        'the pattern in GO_CONSTANT_SPECS (scripts/check-settings-readiness-figure.mjs).',
    )
  }

  return Number(matches[0][1])
}

/** Parse `DefaultSessionStartReadyDeadline`, in seconds. */
export function parseDeadlineSeconds(goSource) {
  return parseGoConstant(goSource, GO_CONSTANT_SPECS.deadlineSeconds)
}

/** Parse `sessionStartReadyAttempts`, ignoring the struct field and the setter. */
export function parseAttempts(goSource) {
  return parseGoConstant(goSource, GO_CONSTANT_SPECS.attempts)
}

/** Parse `modalProbeTimeout`, in seconds. */
export function parseProbeSeconds(goSource) {
  return parseGoConstant(goSource, GO_CONSTANT_SPECS.probeSeconds)
}

/**
 * The worst-case wall clock a doomed session start costs, in seconds.
 *
 * This is the model named under "what this does not prove" above; it is derived
 * here and never written down as a literal.
 *
 * @param {{attempts: number, deadlineSeconds: number, probeSeconds: number}} figures
 * @returns {number}
 */
export function computeWorstCaseSeconds(figures) {
  const { attempts, deadlineSeconds, probeSeconds } = figures
  return attempts * (deadlineSeconds + probeSeconds)
}

/**
 * Read all three constants from a checkout, fail-closed on a missing file.
 *
 * @param {string} repoRoot Absolute repo root.
 * @returns {{attempts: number, deadlineSeconds: number, probeSeconds: number}}
 */
export function readGoFigures(repoRoot) {
  const figures = {}
  for (const [key, spec] of Object.entries(GO_CONSTANT_SPECS)) {
    const sourcePath = path.join(repoRoot, spec.file)
    if (!fs.existsSync(sourcePath)) {
      throw new Error(
        `Cannot read ${spec.symbol}: ${spec.file} does not exist. The constant moved, so the ` +
          'documented worst-case figure has nothing binding it. Update GO_CONSTANT_SPECS in ' +
          'scripts/check-settings-readiness-figure.mjs to the new location.',
      )
    }
    figures[key] = parseGoConstant(fs.readFileSync(sourcePath, 'utf8'), spec)
  }
  return figures
}

/**
 * Extract the `//` doc comment lines immediately above `declaration`.
 *
 * Used by the size pin in the test sibling, which is why it lives here rather
 * than there: the declaration marker and the parsing rule belong beside the
 * constants they describe.
 *
 * @param {string} goSource Whole Go file text.
 * @param {string} declaration Literal first line of the declaration.
 * @returns {string[]} The comment lines, in source order.
 */
export function extractDocCommentLines(goSource, declaration) {
  const lines = String(goSource).split('\n')
  const declarationIndex = lines.findIndex((line) => line.trim().startsWith(declaration))
  if (declarationIndex === -1) {
    throw new Error(
      `Declaration not found: ${JSON.stringify(declaration)}. The symbol was renamed or its ` +
        'signature changed; update SESSION_START_DECLARATION in ' +
        'scripts/check-settings-readiness-figure.mjs.',
    )
  }

  const comment = []
  for (let index = declarationIndex - 1; index >= 0; index -= 1) {
    if (!lines[index].trim().startsWith('//')) break
    comment.unshift(lines[index])
  }
  if (comment.length === 0) {
    throw new Error(
      `No doc comment above ${JSON.stringify(declaration)} — a line pin over zero lines pins ` +
        'nothing. Either the comment was deleted or the declaration marker matched the wrong ' +
        'line.',
    )
  }
  return comment
}

/**
 * Check the documented figure against the composed one.
 *
 * @param {string} [repoRoot] Absolute repo root.
 * @returns {{ok: boolean, counts: {goConstantsParsed: number, docPagesChecked: number,
 *   worstCaseSentencesChecked: number}, figures: object|null}}
 */
export function checkSettingsReadinessFigure(repoRoot = REPO_ROOT) {
  const counts = { goConstantsParsed: 0, docPagesChecked: 0, worstCaseSentencesChecked: 0 }

  let figures
  try {
    figures = readGoFigures(repoRoot)
  } catch (error) {
    console.error(error.message)
    return { ok: false, counts, figures: null }
  }
  counts.goConstantsParsed = Object.keys(GO_CONSTANT_SPECS).length

  const worstCaseSeconds = computeWorstCaseSeconds(figures)
  const composed = { ...figures, worstCaseSeconds }

  // First half of the narrowing tripwire, told apart by the docs SITE rather
  // than the page: a checkout with no services/docs at all has nothing to check
  // and is a legitimate pass, while a site that is present with the page missing
  // means this gate has gone stale and must say so instead of printing a green
  // line over zero pages.
  const docsSite = path.join(repoRoot, DOCS_SITE_DIR)
  if (!fs.existsSync(docsSite)) {
    console.log(
      `Settings readiness figure: no ${DOCS_SITE_DIR} in this checkout, nothing to check ` +
        `(worst case composes to ${worstCaseSeconds}s from ${counts.goConstantsParsed} Go ` +
        'constants)',
    )
    return { ok: true, counts, figures: composed }
  }

  const pagePath = path.join(repoRoot, SETTINGS_PAGE)
  if (!fs.existsSync(pagePath)) {
    console.error(`Found ${DOCS_SITE_DIR} but no ${SETTINGS_PAGE}.`)
    console.error(
      'The Settings reference page has moved; update SETTINGS_PAGE in ' +
        'scripts/check-settings-readiness-figure.mjs so this gate stops passing without ' +
        'checking anything.',
    )
    return { ok: false, counts, figures: composed }
  }
  counts.docPagesChecked = 1

  let section
  try {
    section = regionUntilNext(
      fs.readFileSync(pagePath, 'utf8'),
      REGION_START,
      REGION_END,
      SETTINGS_PAGE,
    )
  } catch (error) {
    console.error(error.message)
    console.error(
      'The tmux_delivery section markers moved; update REGION_START/REGION_END in ' +
        'scripts/check-settings-readiness-figure.mjs.',
    )
    return { ok: false, counts, figures: composed }
  }

  const matches = [...section.matchAll(WORST_CASE_SENTENCE)]

  // Second half of the narrowing tripwire. The page check above bounds the FILE
  // count; this bounds the MATCH count, because the same empty success reappears
  // one layer up when the page is found and the sentence is not. Requiring
  // EXACTLY one match makes presence and absence a single assertion: zero means
  // the figure stopped being stated (or was rephrased past this pattern), and two
  // means a second copy was added that nothing keeps in step with the first.
  if (matches.length !== 1) {
    console.error(
      `Expected exactly one worst-case sentence in the ${REGION_START} section of ` +
        `${SETTINGS_PAGE}, found ${matches.length}.`,
    )
    console.error(
      matches.length === 0
        ? 'The figure is no longer stated in the shape this gate matches. Restore it, or ' +
            'update WORST_CASE_SENTENCE in scripts/check-settings-readiness-figure.mjs — an ' +
            'unmatched sentence is an unguarded number.'
        : 'A second copy of the figure was added. Two copies drift; state it once and refer ' +
            'to it from anywhere else that needs it.',
    )
    return { ok: false, counts, figures: composed }
  }
  counts.worstCaseSentencesChecked = 1

  const documentedWorstCase = Number(matches[0][1])
  const documentedDeadline = Number(matches[0][2])
  const problems = []
  if (documentedWorstCase !== worstCaseSeconds) {
    problems.push(
      `worst case: page says ${documentedWorstCase}s, the Go constants compose ` +
        `${worstCaseSeconds}s (${figures.attempts} attempts x (${figures.deadlineSeconds}s ` +
        `deadline + ${figures.probeSeconds}s probe))`,
    )
  }
  if (documentedDeadline !== figures.deadlineSeconds) {
    problems.push(
      `default deadline: page says ${documentedDeadline}s, ` +
        `${GO_CONSTANT_SPECS.deadlineSeconds.symbol} is ${figures.deadlineSeconds}s`,
    )
  }

  if (problems.length > 0) {
    console.error(`${SETTINGS_PAGE} disagrees with the Go constants:`)
    for (const problem of problems) {
      console.error(`  ${problem}`)
    }
    console.error(
      'Correct the page, or — if a constant moved on purpose — correct it and the page in the ' +
        'same commit.',
    )
    return { ok: false, counts, figures: composed }
  }

  console.log(
    `Settings readiness figure OK (${counts.worstCaseSentencesChecked} sentence checked in ` +
      `${counts.docPagesChecked} page against ${counts.goConstantsParsed} Go constants: ` +
      `${figures.attempts} x (${figures.deadlineSeconds}s + ${figures.probeSeconds}s) = ` +
      `${worstCaseSeconds}s)`,
  )
  return { ok: true, counts, figures: composed }
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) {
  if (!checkSettingsReadinessFigure().ok) process.exit(1)
}
