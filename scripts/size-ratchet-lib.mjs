#!/usr/bin/env node

// Two-sided, self-explaining size ratchets for committed artifacts (BOS-768).
//
// A size ratchet written the obvious way — `assert.ok(bytes <= RATCHET, …)` — banks nothing.
// It is one-sided: the artifact may shrink by any amount and the gate stays green, so the
// saving becomes silent headroom the artifact can regrow into without a single red run. Eight
// of this repo's nine JS ratchets were carrying exactly that stale headroom when this landed
// (`boss` 6 B, `boss-build` 8 B, `boss-epic` 67 B, and 83/91/81/311 B across the four sweeps).
// Each of those numbers is a saving somebody earned and nobody kept.
//
// A rounded-up ceiling ("measured + 64", "next KiB") is the same leak spelled as a decision.
// It hands the budget back at the moment it is written.
//
// `assertExactSize` compares `measured === expected` and branches: OVER is a regression,
// UNDER is an unbanked saving, and each gets its own remedy. That is the shape
// `lib/bossalib/bossmcp/manifest_test.go` (TestToolSurfaceSizeRatchet) already proves in Go,
// message text and all; this is its JavaScript implementation, and the two are meant to be
// read together.
//
// WHY THE MESSAGES ARE THIS LONG. A pinned number that reds tells the reader nothing about
// what to do, and the wrong guess is expensive in both directions: repinning UP re-spends a
// banked saving, and "trim prose" is actively wrong when the red artifact is a generated
// mirror. So every failure names the measured value beside the pinned one, the constant and
// the file to edit, the requirement that the repin land in the SAME commit as the artifact
// change, and — in the under case — the possibility that the reader never touched the file at
// all and inherited the red from `origin/main`.
//
// WHY `residual` IS REQUIRED. A gate that does not state its own blind spot gets read as
// coverage it does not have. `assertExactSize` throws when `residual` is omitted, so "state
// your blind spot" cannot be skipped by forgetting it, and every failure message ends with it.
//
// THE SHARE-FLOOR GUARD IS DELIBERATELY NOT HERE. `manifest_test.go` guards measurement
// collapse with a scale-free 30% schema-share floor. The analogous "resident body as a share
// of resident + references" was measured across the eight guarded skills at 25%, 26%, 50%,
// 66%, 76%, 52%, 100% and 100% — there is no defensible single floor, and `bs-sweep-mutation`
// and `bs-sweep-prettify` have no `references/` directory at all, where such a guard is
// identically 100% and therefore vacuous by construction. Shipping it anyway would be the
// vacuity this file exists to refuse. What replaces it is a shape self-check that the
// measurement still points at the artifact: `measureFile` throws rather than returning 0 on a
// missing or empty file, `assertArtifactSet` throws when a guarded list is empty or has
// silently shortened, and each call site keeps its own `frontmatter identifies the skill`
// test. An exact two-sided pin is already collapse-proof for the byte total itself.
//
// RESIDUAL OF THIS WHOLE FILE, stated so it is not mistaken for coverage it does not have: an
// exact byte pin cannot detect a restructure that lands on the identical byte count, and
// nothing here polices the resident-vs-references split — content moved into a reference stops
// being measured at all. Per-call-site residuals are the `residual` parameter's job.
//
// `scripts/check-raw-size-ratchets.mjs` is the gate that stops the open-coded shape coming
// back. Its scan is parser-free, so the prose in this file deliberately never spells the
// forbidden comparison verbatim; that is what lets both files pass the gate with no opt-out.

import fs from 'node:fs'

/** Remedy sentence for an artifact that has somewhere to extract content to. */
export const MOVE_TO_REFERENCE_REMEDY =
  'Move situational content into a reference rather than raising the pin.'

/** Remedy sentence for an artifact with no `references/` directory to extract into. */
export const NO_REFERENCE_REMEDY =
  'This skill has no references/ directory, so there is nowhere to extract to: the saving has ' +
  'to come out of the body itself, or the growth has to be justified and the pin re-measured.'

const UNITS = new Set(['bytes', 'lines'])

function residualSuffix(residual) {
  return ` Not covered by this check: ${residual}.`
}

function requireResidual(residual, fn) {
  if (typeof residual !== 'string' || residual.trim() === '') {
    throw new Error(
      `size-ratchet: a gate must state its blind spot — ${fn} requires a non-empty \`residual\` ` +
        'describing what this check does NOT cover. A pinned number with no stated blind spot ' +
        'gets read as coverage it does not have. This is a wiring error in the gate, not a ' +
        'finding about the artifact.',
    )
  }
}

function requireText(name, value, fn) {
  if (typeof value !== 'string' || value.trim() === '') {
    throw new Error(`size-ratchet: ${fn} requires a non-empty \`${name}\`. Wiring error.`)
  }
}

function requireCount(name, value, fn) {
  if (!Number.isInteger(value) || value < 0) {
    throw new Error(
      `size-ratchet: ${fn} requires \`${name}\` to be a non-negative integer, got ` +
        `${String(value)}. A gate that compares against a non-number passes on nothing. ` +
        'Wiring error.',
    )
  }
}

/**
 * Assert a committed artifact measures EXACTLY its pinned size, failing in both directions.
 *
 * Over is a regression; under is an unbanked saving. Both throw, with different remedies, and
 * both messages end with `residual`.
 *
 * @param {object} options
 * @param {string} options.label Human name for what is being gated, used to open the message.
 * @param {string} options.path Repo-relative path of the measured artifact.
 * @param {number} options.measured Freshly measured size (see `measureFile`).
 * @param {number} options.expected The pinned constant's value.
 * @param {{value: number, delta?: number, label?: string}} [options.previous] Previous pinned
 *   value plus the recorded delta. When supplied, the delta is derived and checked.
 * @param {string} options.constName Identifier of the pinned constant, so the fix names itself.
 * @param {string} options.constFile Repo-relative file the constant lives in.
 * @param {'bytes'|'lines'} [options.unit] What is being counted. Default `bytes`.
 * @param {{name: string, value: number}} [options.below] Second, opposing threshold the pin
 *   must sit under (a pre-extraction/pre-split baseline). Its failure names BOTH readings.
 * @param {string} [options.remedy] Over-case remedy sentence. Default
 *   `MOVE_TO_REFERENCE_REMEDY`; pass `NO_REFERENCE_REMEDY` where there is no reference to
 *   move content into.
 * @param {string} options.residual REQUIRED. What this check does not cover.
 * @returns {void}
 */
export function assertExactSize(options) {
  const {
    label,
    path: artifactPath,
    measured,
    expected,
    previous,
    constName,
    constFile,
    unit = 'bytes',
    below,
    remedy = MOVE_TO_REFERENCE_REMEDY,
    residual,
  } = options ?? {}

  // Residual is validated FIRST, before any measurement is even looked at, so a call site that
  // forgot it fails on every run rather than only on the run where the artifact happens to move.
  requireResidual(residual, 'assertExactSize')
  requireText('label', label, 'assertExactSize')
  requireText('path', artifactPath, 'assertExactSize')
  requireText('constName', constName, 'assertExactSize')
  requireText('constFile', constFile, 'assertExactSize')
  requireCount('measured', measured, 'assertExactSize')
  requireCount('expected', expected, 'assertExactSize')
  if (previous !== undefined) {
    requireCount('previous.value', previous?.value, 'assertExactSize')
    if (!Number.isInteger(previous?.delta)) {
      throw new Error(
        'size-ratchet: assertExactSize `previous.delta` must be an integer when `previous` is supplied. Wiring error.',
      )
    }
    const derivedDelta = expected - previous.value
    if (previous.delta !== derivedDelta) {
      throw new Error(
        `${label}: recorded ${previous.label ?? 'ratchet'} delta is ${previous.delta}, but ` +
          `${expected} - ${previous.value} derives ${derivedDelta}. Fix the bookkeeping line ` +
          `beside ${constName} in ${constFile}; a lying re-baseline comment misleads the next repin.` +
          residualSuffix(residual),
      )
    }
  }
  if (!UNITS.has(unit)) {
    throw new Error(
      `size-ratchet: assertExactSize \`unit\` must be one of ${[...UNITS].join(', ')}, got ` +
        `${String(unit)}. Wiring error.`,
    )
  }

  const tail = residualSuffix(residual)

  if (below !== undefined) {
    requireText('below.name', below?.name, 'assertExactSize')
    requireCount('below.value', below?.value, 'assertExactSize')
    if (expected >= below.value) {
      // Two thresholds, one artifact, opposing directions. Prescribing a single cause here
      // ("trim resident prose") is what the old message did, and it is wrong half the time:
      // the baseline is a historical measurement that can itself have stopped describing
      // anything real. Name both readings and let the reader decide which one they are in.
      throw new Error(
        `${label}: the pin ${constName} = ${expected} ${unit} no longer sits below the ` +
          `baseline ${below.name} = ${below.value} ${unit}. Read this BOTH ways before ` +
          'touching either number: either the pin was raised toward the baseline, in which ' +
          'case the growth is what wants undoing — or the baseline needs re-deriving, ' +
          'because it records a measurement that no longer describes anything real. What is ' +
          'never right is sliding both up together, which is how a bound stops being a bound.' +
          tail,
      )
    }
  }

  if (measured === expected) return

  if (measured > expected) {
    throw new Error(
      `${label}: ${artifactPath} measured ${measured} ${unit}, pinned at ${expected} ${unit} ` +
        `(${constName} in ${constFile}) — over by ${measured - expected}. This ratchet only ` +
        `moves DOWN. ${remedy} If the growth is genuinely necessary, repin ${constName} to ` +
        `${measured} in ${constFile}, in the SAME commit that changed ${artifactPath}, and ` +
        'record why on the constant.' +
        tail,
    )
  }

  throw new Error(
    `${label}: ${artifactPath} measured ${measured} ${unit}, pinned at ${expected} ${unit} ` +
      `(${constName} in ${constFile}) — under by ${expected - measured}. The artifact shrank ` +
      'but the reduction was never banked: left unpinned it is silent headroom the artifact ' +
      `can regrow into. Repin ${constName} to ${measured} in ${constFile}, in the SAME commit ` +
      `that changed ${artifactPath}. If you did not touch ${artifactPath}, the constant moved ` +
      'without you: check it as it exists at origin/main before assuming this branch caused ' +
      'it.' +
      tail,
  )
}

/**
 * Measure a file fail-closed: throw on missing, unreadable, non-regular or empty.
 *
 * A ratchet whose measurement collapses to 0 passes trivially against any ceiling, and a
 * measurement of a file that moved away is a gate that guards nothing. So this never returns 0
 * and never returns a value for a path that is not a readable regular file.
 *
 * @param {string} absPath Absolute path to the artifact.
 * @param {object} [options]
 * @param {'bytes'|'lines'} [options.unit] Default `bytes`. `lines` counts newline-separated
 *   lines, not counting a trailing newline as an extra empty line — the metric the converted
 *   `CLAUDE.md` gate already used.
 * @returns {number} The measurement, always > 0.
 */
export function measureFile(absPath, options = {}) {
  const { unit = 'bytes' } = options
  requireText('absPath', absPath, 'measureFile')
  if (!UNITS.has(unit)) {
    throw new Error(
      `size-ratchet: measureFile \`unit\` must be one of ${[...UNITS].join(', ')}, got ` +
        `${String(unit)}. Wiring error.`,
    )
  }

  let stat
  try {
    stat = fs.statSync(absPath)
  } catch (cause) {
    throw new Error(
      `size-ratchet: cannot measure ${absPath} — it does not exist or is unreadable. The ` +
        'guarded artifact moved or was deleted; a gate that measured it as 0 would pass on ' +
        'nothing at all.',
      { cause },
    )
  }
  if (!stat.isFile()) {
    throw new Error(
      `size-ratchet: ${absPath} is not a regular file, so there is nothing to measure. The ` +
        'guarded artifact moved; fix the path rather than the pin.',
    )
  }

  const buffer = fs.readFileSync(absPath)
  if (buffer.length === 0) {
    throw new Error(
      `size-ratchet: ${absPath} is empty (0 bytes). That is a collapsed measurement, not a ` +
        'small artifact — it is refused rather than returned, because 0 satisfies every ' +
        'ceiling ever written.',
    )
  }
  if (unit === 'bytes') return buffer.length

  const text = buffer.toString('utf8')
  return text.split('\n').length - (text.endsWith('\n') ? 1 : 0)
}

/**
 * Assert a guarded artifact list is non-empty and still the length it is supposed to be.
 *
 * A `for` loop over a list that silently shortened runs fewer assertions and stays green: the
 * dropped mirror is simply no longer gated, with nothing red to say so.
 *
 * @param {unknown} list The list the gate iterates.
 * @param {number} expectedLength How many entries it must have.
 * @param {string} [label] What the list is, used in the failure.
 * @returns {void}
 */
export function assertArtifactSet(list, expectedLength, label = 'guarded artifact set') {
  requireCount('expectedLength', expectedLength, 'assertArtifactSet')
  if (!Array.isArray(list)) {
    throw new Error(
      `size-ratchet: ${label} is ${typeof list}, not an array — a gate that iterates it runs ` +
        'zero assertions. Wiring error.',
    )
  }
  if (list.length === 0) {
    throw new Error(
      `size-ratchet: ${label} is empty, so every assertion in the loop over it runs zero ` +
        'times and the ratchet passes vacuously.',
    )
  }
  if (list.length !== expectedLength) {
    throw new Error(
      `size-ratchet: ${label} has ${list.length} entries, expected ${expectedLength}. A list ` +
        'that shortens drops a guarded artifact with nothing going red to say so; a list that ' +
        'grows added one nobody pinned. Update the expected length in the same commit that ' +
        'changed the list.',
    )
  }
}

/**
 * Assert a generated mirror is byte-identical to regenerating it from its source.
 *
 * SIZE IS NOT THE DISCRIMINATOR HERE, and the intuitive rule is wrong. `rewriteClaudeSkillMarkdown`
 * unconditionally prepends a generated-by header, so a HEALTHY `.codex` mirror is always LARGER
 * than its `.claude` source — by 81, 80, 80 and 82 bytes for the four sweep skills measured at
 * implementation time. A rule of "a mirror larger than its source is the tell" would therefore
 * ship a permanently-red gate. Exact in-memory regeneration equality is the correct
 * discriminator, and it makes a byte constant on the mirror unnecessary: one exact pin on the
 * source plus regeneration equality fully determines the mirror.
 *
 * The remedy for a red here is `make codex-skills`, never a prose edit — the message says so,
 * and deliberately does NOT repeat the extract-into-a-reference remedy, which would send a
 * reader to trim a file that is regenerated wholesale.
 *
 * @param {object} options
 * @param {string} options.sourcePath Path of the authored source.
 * @param {string} options.mirrorPath Path of the generated mirror.
 * @param {(source: string, sourcePath: string) => string} options.regenerate The generator.
 * @param {(p: string) => string} [options.read] Reader, injectable for tests.
 * @param {string} [options.residual] What this check does not cover. Defaults to the check's
 *   own intrinsic blind spot — unlike `assertExactSize`, whose blind spot varies per artifact,
 *   this one is a property of the check itself, so it has a defensible built-in.
 * @returns {void}
 */
export function assertMirrorRegenerated(options) {
  const {
    sourcePath,
    mirrorPath,
    regenerate,
    read = (p) => fs.readFileSync(p, 'utf8'),
    residual = 'that the SOURCE is correct — this proves only that the mirror is what the ' +
      'generator produces from it, so a wrong source produces a faithful wrong mirror',
  } = options ?? {}

  requireResidual(residual, 'assertMirrorRegenerated')
  requireText('sourcePath', sourcePath, 'assertMirrorRegenerated')
  requireText('mirrorPath', mirrorPath, 'assertMirrorRegenerated')
  if (typeof regenerate !== 'function') {
    throw new Error(
      'size-ratchet: assertMirrorRegenerated requires a `regenerate` function. Wiring error.',
    )
  }

  const source = read(sourcePath)
  const mirror = read(mirrorPath)
  if (source === '') {
    throw new Error(`size-ratchet: ${sourcePath} is empty, so regeneration proves nothing.`)
  }
  const regenerated = regenerate(source, sourcePath)
  if (regenerated === mirror) return

  throw new Error(
    `${mirrorPath} is not byte-identical to regenerating it from ${sourcePath}. It is a ` +
      'GENERATED mirror, so this is a sync failure, not a size problem: run `make ' +
      'codex-skills` to regenerate it, and if the change you wanted belongs in the mirror, ' +
      `make it in ${sourcePath} first. Do not hand-edit the mirror. Note that size is NOT the ` +
      'discriminator — the generator prepends a header, so a healthy mirror is always larger ' +
      'than its source; only exact regeneration equality distinguishes the two cases.' +
      residualSuffix(residual),
  )
}
