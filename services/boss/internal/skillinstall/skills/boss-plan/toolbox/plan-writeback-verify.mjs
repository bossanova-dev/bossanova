#!/usr/bin/env node

// plan-writeback-verify — the post-save read-back that MEASURES round-trip fidelity of a tracker
// description write instead of assuming it.
//
// Every other gate in the planning skill is pre-write prevention: they read local files and abort
// before the save. Nothing observed what actually landed, so a transcription slip at the save step
// survived the whole gate stack — and the tracker exposes no description history to an agent, so the
// written text is the only surviving copy. This helper closes that half.
//
// Two recorded field beliefs contradict each other. One says the tracker normalizes markdown on
// write, so a description never round-trips byte-identically and a post-save byte diff can only
// false-positive. The other says the normalization belongs to the write TRANSPORT, and a raw write
// does round-trip byte-identically. This helper encodes neither: round-trip fidelity is a property
// of the transport, so it is measured in the run that used it and the strongest check the
// measurement supports is applied.
//
//   tier 1  byte-exact              stored == intended, byte for byte.
//   tier 2  normalized-equivalent   every difference is attributable to a DECLARED transform, AND
//                                   the semantic contract validates against the stored text, AND
//                                   the verbatim block matches under the same normalization with
//                                   its upload identities unchanged. All three, or it is not tier 2.
//   tier 3  unattributed            the three CONTENT conjuncts hold, but some difference is not
//                                   attributable to a declared transform. Advisory by default.
//   tier 4  drift                   a content conjunct failed: a contract section is gone, an
//                                   upload identity is gone, or the verbatim block changed. Exit
//                                   non-zero, name the differing line and column, retain scratch.
//
// Tiers 3 and 4 used to be one tier, and both exited non-zero. Separating them is the whole point
// of this revision. Measured over 60 runs, the merged tier fired on 55% of verifications and every
// retained artifact was a cosmetic bullet-marker rewrite; the run it failed had already created the
// attachment, moved the ticket, and written every label. Exiting non-zero there withholds NOTHING —
// the description is already stored and a corrective rewrite is forbidden — so the only effect was
// to strand finished work. A tier-3 warning keeps every scrap of the triage signal (named cause,
// located difference, retained scratch, a line in the run report) without that cost. Tier 4 keeps
// blocking, because a missing section or a dropped image is exactly the case a human must see
// before the artifact is trusted.
//
// Both branches are NON-DESTRUCTIVE. By the time this runs the description is already stored, so
// the "no write, abort" branch the pre-write gates take is unavailable — and an automatic
// corrective rewrite would be an unattended agent overwriting a description it has just proven it
// cannot reproduce faithfully. This helper never writes anything.
//
// Node builtins only — this runs in dependency-light cron worktrees.

import { readFileSync } from 'node:fs'

import { createGateRecorder } from './gate-outcome.mjs'
import { isMainModule } from './main-module.mjs'
import { findDroppedImages, originalNotesBodies } from './plan-image-guard.mjs'
import {
  DESCRIPTION_NORMALIZATION_TRANSFORMS,
  loadSkillConfig,
  toleratedDescriptionTransforms,
  unattributedDriftSeverity,
  validatePlanDescription,
} from './skill-config.mjs'

/** The four verdicts. Exactly one is emitted per run, or none at all on a refusal. */
export const WRITEBACK_VERDICTS = Object.freeze({
  BYTE_EXACT: 'byte-exact',
  NORMALIZED_EQUIVALENT: 'normalized-equivalent',
  UNATTRIBUTED: 'unattributed',
  DRIFT: 'drift',
})

/**
 * Why a verdict was reached, as a closed slug vocabulary. The verdict alone was not enough to
 * answer "is this gate earning its keep": every failing run recorded the single token `drift`, so
 * telling a lost section from a bullet rewrite meant hand-diffing retained scratch directories.
 * Each cause is recorded as the gate-outcome reason, so the question is answerable from telemetry.
 *
 * Slugs must satisfy gate-outcome's slug pattern; it replaces a non-matching token wholesale.
 */
export const WRITEBACK_CAUSES = Object.freeze({
  EQUAL: 'equal',
  NORMALIZED: 'normalized',
  CONTRACT: 'contract',
  UPLOADS: 'uploads',
  NOTES: 'notes',
  UNATTRIBUTED: 'unattributed',
})

// One canonicalizer per transform id in skill-config's closed vocabulary. Each maps BOTH spellings
// of its transform onto one canonical form, so it can be applied to the intended and the stored side
// alike without knowing which one the tracker reshaped. Each is idempotent.
export const DESCRIPTION_TRANSFORM_NORMALIZERS = Object.freeze({
  // `- item` / `+ item` / `* item` -> one canonical marker. Byte-count neutral, which is why a size
  // comparison cannot stand in for the byte comparison.
  'unordered-list-marker-substitution': (text) => text.replace(/^([ \t]*)[-+*]([ \t])/gm, '$1*$2'),
  // `**1.** ` stored as `**1. **`: whitespace migrating across a delimiter run. Canonical form puts
  // the whitespace outside. The delimiter must be FOLLOWED by whitespace or end-of-line, which is
  // what keeps an ordinary opening delimiter (`a *b* c`) out of the rule.
  'emphasis-delimiter-whitespace-migration': (text) =>
    text.replace(/[ \t]+(\*{1,3}|_{1,3})(?=[ \t]|$)/gm, '$1'),
  // `**a `b` c**` stored as `**a **`b`** c**`: one emphasis span containing an inline code span
  // rewritten as emphasis + code + emphasis. Canonical form MERGES the split back into the single
  // span; it does NOT strip delimiter runs that merely sit against a code-span boundary.
  //
  // Stripping was lossy in the one direction that matters. `The **`foo`** helper.` and
  // `The `foo` helper.` — a stored description that genuinely LOST the emphasis around an inline
  // code span — both reduced to `The `foo` helper.`, so the fidelity gate certified real content
  // loss as normalized-equivalent and exited zero. A canonicalizer for a RESTRUCTURING must
  // preserve the presence or absence of the emphasis and normalize only its position.
  //
  // The merge is anchored on the OUTER delimiter pair: the recognised shape is an opening run, then
  // one or more JOINTS — a closing run, an inline code span, an opening run — then a closing run,
  // every run the SAME delimiter. Only the interior runs are dropped. A run that IS the emphasis
  // (`**`x`**`, two runs) has no outer pair to anchor on and survives untouched, so it still
  // differs from the un-emphasised `` `x` ``. Deliberately conservative: a shape this pattern does
  // not recognise stays a byte difference and is reported as drift, which is the safe direction.
  //
  // The joint tolerates whitespace at the split point and repeats, because that is the shape the
  // transport was MEASURED emitting: `**a `c` b**` is stored as `**a** `c` **b**`, and a span with
  // two code spans is stored with a joint at each. Anchoring on the contiguous one-span form alone
  // left both measured pairs reading as drift. Widening WHICH shape is recognised does not widen
  // what survives it: the whitespace stays in the output as the text it is, and presence and
  // delimiter-run length are untouched, so emphasis DELETED and emphasis DEMOTED still differ.
  'emphasis-span-restructuring': (text) => {
    // The pattern is built and run ONCE PER DELIMITER CHARACTER — `*`, then `_` — because the bound
    // it needs cannot be written with the delimiter known only as a backreference. In each pass the
    // segments exclude THAT pass's delimiter as well as backticks and newlines, so the only runs of
    // that delimiter inside a match are the match's own joints. Two consequences, and they are the
    // whole point: an outer match can never reach across an independently emphasised span of the
    // same delimiter, and the global inner replace below therefore only ever deletes runs that
    // genuinely are joints. Excluding BOTH delimiters from BOTH passes would bound the match too,
    // but it would stop `snake_case` text inside `**bold**` from merging — the very drift this
    // transform exists to remove — so each pass excludes only its own. A run of the OTHER delimiter
    // may still sit inside a segment; it is carried through the merge as the text it is.
    //
    // The outer pair is additionally required to FLANK, the way CommonMark requires of a real
    // emphasis pair: an opening run is followed by non-whitespace, a closing run is preceded by it.
    // Without that the closing run of an already-merged span reads as an opening run for the span
    // AFTER it, and `**a `c` b** then **`z`** and ...` merges a second time and deletes the bold
    // around `` `z` `` — a false pass, and one the bounded fixpoint loop below would reach on its
    // own even from an input that needed no merge at all.
    //
    // What a match is bounded to, exactly: one line (no segment or code span may contain a newline)
    // and one emphasis span of the delimiter being matched.
    //
    // Every delimiter run is additionally fenced by `(?<![*_])` / `(?![*_])` so it can only match a
    // MAXIMAL run: without that fence the engine backtracks `**` down to `*` and rewrites
    // `**`x`**` as `*`x`*`, which loses a delimiter and collides bold with italic — the same class
    // of loss this rewrite exists to end.
    const CODE = String.raw`\`[^\`\n]*\``
    const splitFor = (delim) => {
      const q = delim === '*' ? String.raw`\*` : delim
      const OPEN = String.raw`(?<![*_])(${q}{1,3})(?![*_])(?=\S)`
      const RUN = String.raw`(?<![*_])\1(?![*_])`
      const SEG = String.raw`[^\`\n${q}]*?`
      const JOINT = String.raw`${RUN}[ \t]*${CODE}[ \t]*${RUN}`
      return new RegExp(`${OPEN}(?:${SEG}${JOINT})+${SEG}(?<=\\S)${RUN}`, 'g')
    }
    // Rebuild the matched span by dropping ONLY its interior joint runs. The delimiter is known by
    // then, so the inner pattern is unambiguous — and rebuilding this way, rather than with numbered
    // groups, is what lets the joint repeat an unbounded number of times.
    const merge = (match, delim) => {
      const quoted = delim.replace(/\*/g, String.raw`\*`)
      const joint = new RegExp(
        String.raw`(?<![*_])${quoted}(?![*_])([ \t]*)(${CODE})([ \t]*)(?<![*_])${quoted}(?![*_])`,
        'g',
      )
      const inner = match.slice(delim.length, match.length - delim.length)
      return delim + inner.replace(joint, '$1$2$3') + delim
    }
    const splits = ['*', '_'].map(splitFor)
    let out = String(text)
    // Run to a fixpoint (bounded) so the result is idempotent and order-independent.
    for (let pass = 0; pass < 10; pass += 1) {
      let next = out
      for (const split of splits) next = next.replace(split, merge)
      if (next === out) break
      out = next
    }
    return out
  },
  'trailing-whitespace-trimming': (text) => text.replace(/[ \t]+$/gm, ''),
  'terminal-newline-trimming': (text) => text.replace(/\n+$/, ''),
  // `| --- | --- |` stored as `| -- | -- |`: the dash run inside each cell of a table delimiter row
  // rewritten to a different length. Per the GFM tables extension a delimiter cell is hyphens with
  // an optional leading or trailing colon — the colons carry alignment, the dash count carries
  // nothing — so canonicalizing the run length is meaning-preserving where dropping a colon is not.
  // Only the dash runs are rewritten, and only on a line that is a delimiter row in full: colons,
  // pipes and surrounding whitespace are left exactly as they are, so a row that LOST a colon or a
  // cell still differs from the one it came from.
  //
  // It does NOT rewrite a dash line that is not a delimiter row. In GFM a delimiter row is defined
  // POSITIONALLY — it is the row immediately after a table's header row — so the shape alone is not
  // enough: `| - | - |` is an ordinary BODY row meaning "none" in the tables this gate verifies, and
  // rewriting it would canonicalize CONTENT and make two genuinely different tables compare equal.
  // The rewrite therefore fires only where the PRECEDING line is a plausible header row: it carries
  // a pipe and is not itself delimiter-shaped. A pipe is REQUIRED on the delimiter row too — a
  // thematic break, a setext underline and a prose dash run carry none, and a single-column row
  // written without pipes is indistinguishable from a thematic break, so the safe reading of an
  // ambiguous line is "not a table".
  //
  // FENCED content is skipped: it is literal text, and rewriting it would change what the block
  // shows rather than how the document renders. Only fenced blocks are recognised as literal — a
  // four-space-INDENTED code block is literal by the same argument but is not skipped, because
  // widening the skip to indented lines would also skip the tables this transform legitimately has
  // to canonicalize inside list items. A delimiter-shaped row inside an indented code block is a
  // known and accepted limitation of the rule, not a claim it handles.
  'table-delimiter-row-normalization': (text) => {
    // Optional leading pipe, then cells of optional-colon + dashes + optional-colon separated by
    // pipes, then an optional trailing pipe. Every repetition must consume a `|`, so the quantifier
    // cannot backtrack quadratically on a long line.
    const DELIMITER_ROW = /^[ \t]*\|?(?:[ \t]*:?-+:?[ \t]*\|)*[ \t]*:?-+:?[ \t]*\|?[ \t]*$/
    const FENCE_OPEN = /^[ \t]{0,3}(`{3,}|~{3,})/
    // CommonMark closes a fence only with the SAME character at >= the opening length, and a closing
    // fence carries no info string. A bare boolean toggle got both wrong: a `~~~` line closed a
    // ``` block, and the four-backtick wrapper this repo's docs use to quote a markdown block that
    // itself contains a fence was closed by the inner ``` — exposing the quoted content to rewrite.
    const FENCE_CLOSE = /^[ \t]{0,3}(`{3,}|~{3,})[ \t]*$/
    const delimiterShaped = (line) => line.includes('|') && DELIMITER_ROW.test(line)
    let fence = null
    // The line before this one, when it was ordinary text outside a fence; null otherwise.
    let previous = null
    return String(text)
      .split('\n')
      .map((line) => {
        if (fence) {
          const close = FENCE_CLOSE.exec(line)
          if (close && close[1][0] === fence.char && close[1].length >= fence.length) fence = null
          previous = null
          return line
        }
        const open = FENCE_OPEN.exec(line)
        if (open) {
          fence = { char: open[1][0], length: open[1].length }
          previous = null
          return line
        }
        const headed = previous !== null && previous.includes('|') && !delimiterShaped(previous)
        const rewrite = headed && delimiterShaped(line)
        previous = line
        return rewrite ? line.replace(/-+/g, '---') : line
      })
      .join('\n')
  },
})

/**
 * The vocabulary and the normalizer table are two hand-written restatements of ONE list, in two
 * files. Cross-check them at load, bidirectionally, so they cannot drift silently:
 *
 *   - a vocabulary id with no normalizer would validate in `.boss-skills.json`, be declarable as
 *     tolerated, and then be INERT at normalization time — a config knob that quietly does nothing;
 *   - a normalizer with no vocabulary id can never be reached, because validation rejects the id.
 *
 * This throws rather than warning. An unverifiable fidelity gate must fail loudly: the whole point
 * of the helper is that nothing else observes what landed on the tracker.
 */
const missingNormalizers = DESCRIPTION_NORMALIZATION_TRANSFORMS.filter(
  (id) => typeof DESCRIPTION_TRANSFORM_NORMALIZERS[id] !== 'function',
)
const orphanNormalizers = Object.keys(DESCRIPTION_TRANSFORM_NORMALIZERS).filter(
  (id) => !DESCRIPTION_NORMALIZATION_TRANSFORMS.includes(id),
)
if (missingNormalizers.length > 0 || orphanNormalizers.length > 0) {
  throw new Error(
    'plan-writeback-verify: DESCRIPTION_TRANSFORM_NORMALIZERS is out of step with skill-config ' +
      `DESCRIPTION_NORMALIZATION_TRANSFORMS — declarable but inert: [${missingNormalizers.join(', ')}]; ` +
      `unreachable normalizers: [${orphanNormalizers.join(', ')}]`,
  )
}

/**
 * Apply ONLY the declared transforms, in a fixed order so both sides canonicalize identically.
 * An empty tolerated set is the identity function — the strictest possible comparison.
 */
export function normalizeDescription(text, tolerated) {
  let out = String(text ?? '')
  for (const [id, normalize] of Object.entries(DESCRIPTION_TRANSFORM_NORMALIZERS)) {
    if (tolerated.has(id)) out = normalize(out)
  }
  return out
}

/**
 * The description-contract modes this helper can verify against, mirroring the sibling pre-write
 * lint. `child-plan` is the implementation-plan contract; `epic-parent` is the parent-overview
 * shape an epic decomposition stores. The seam exists because the two contracts require DIFFERENT
 * sections: verifying a correctly stored parent overview against the child contract reports `drift`
 * at the worst possible moment — already stored, non-zero exit, scratch retained, and no corrective
 * rewrite permitted. The default keeps every existing caller on `child-plan`.
 */
export const WRITEBACK_DESCRIPTION_MODES = Object.freeze(['child-plan', 'epic-parent'])

const WRITEBACK_DESCRIPTION_MODE_SET = new Set(WRITEBACK_DESCRIPTION_MODES)

/** Reject an unknown mode loudly rather than silently falling back to the child contract. */
function assertWritebackMode(mode) {
  if (!WRITEBACK_DESCRIPTION_MODE_SET.has(mode)) {
    throw new Error(
      `--mode must be one of ${WRITEBACK_DESCRIPTION_MODES.join(', ')}; got ${JSON.stringify(mode)}`,
    )
  }
  return mode
}

/** The 1-based line and column of the first byte at which `a` and `b` differ, or null if equal. */
function firstDifference(a, b) {
  if (a === b) return null
  const limit = Math.max(a.length, b.length)
  let index = 0
  for (; index < limit; index += 1) {
    if (a.charAt(index) !== b.charAt(index)) break
  }
  const before = a.slice(0, index)
  const line = before.split('\n').length
  const column = index - (before.lastIndexOf('\n') + 1) + 1
  return { line, column }
}

function locate(intendedText, storedText) {
  return firstDifference(intendedText, storedText) ?? { line: 1, column: 1 }
}

/**
 * Compute the read-back verdict.
 *
 * Returns `{ verdict, exitCode, reason, line, column, tolerated }`. A `verdict` of `null` is a
 * REFUSAL, not a tier: the comparison could not be performed at all, which is neither a pass nor
 * drift, and callers must not report it as either.
 */
export function verifyWriteback({
  config,
  intendedText,
  storedText,
  tolerated,
  onUnattributed,
  mode = 'child-plan',
} = {}) {
  const declared = tolerated ?? toleratedDescriptionTransforms(config)
  const severity = onUnattributed ?? unattributedDriftSeverity(config)
  const resolvedMode = assertWritebackMode(mode)

  // Refuse to CERTIFY a comparison we could not meaningfully perform. An empty stored description is
  // the exact input a broken read-back produces, and it is also the input a vacuous gate is most
  // confident about — byte-equal against an empty intended text would otherwise read as tier 1.
  if (String(storedText ?? '').trim() === '') {
    return {
      verdict: null,
      cause: null,
      exitCode: 1,
      reason: 'stored description is empty — nothing was verified',
      line: null,
      column: null,
      tolerated: declared,
    }
  }

  if (intendedText === storedText) {
    return {
      verdict: WRITEBACK_VERDICTS.BYTE_EXACT,
      cause: WRITEBACK_CAUSES.EQUAL,
      exitCode: 0,
      reason:
        'stored description is byte-identical to the intended bytes; the transport round-trips',
      line: null,
      column: null,
      tolerated: declared,
    }
  }

  const { line, column } = locate(intendedText, storedText)
  const at = `line ${line}, column ${column}`
  // A CONTENT-LOSS verdict. Always fatal, and never governed by `onUnattributedDrift`: each of the
  // three callers below has positively identified something that is gone from the stored text.
  const drift = (cause, reason) => ({
    verdict: WRITEBACK_VERDICTS.DRIFT,
    cause,
    exitCode: 1,
    reason: `${reason} (first difference at ${at})`,
    line,
    column,
    tolerated: declared,
  })

  // KTD3 — tier 2 is a CONJUNCTION, not a fuzzy match: EVERY byte difference is attributable to a
  // declared transform, AND the semantic contract validates against the stored text, AND the
  // verbatim block matches under the same normalization with its upload identities unchanged. Any
  // one failing drops the verdict to tier 3, because the semantic validator alone cannot see a
  // dropped image and the image check alone cannot see a dropped section.
  //
  // All four are evaluated, and the REASON names the most specific failure rather than whichever
  // one the flowchart draws first. The verdict is identical either way — a failing conjunct is
  // tier 3 however it is discovered — but "the stored text is missing ## Required proof" sends
  // triage somewhere, while "differs outside the declared transform set" on the same input does
  // not. A conjunct failure is also the strictly more alarming reading of the same bytes.
  const contract = validatePlanDescription(config, storedText, { mode: resolvedMode })
  if (!contract.ok) {
    const detail = contract.unsupportedVersion
      ? `stamped Contract: v${contract.version}, newer than this contract`
      : `missing ${contract.missing.join(', ')}`
    return drift(
      WRITEBACK_CAUSES.CONTRACT,
      `stored description fails the semantic description contract: ${detail}`,
    )
  }

  const droppedUploads = findDroppedImages(intendedText, storedText)
  if (droppedUploads.length > 0) {
    return drift(
      WRITEBACK_CAUSES.UPLOADS,
      `stored description lost ${droppedUploads.length} upload identit${
        droppedUploads.length === 1 ? 'y' : 'ies'
      }: ${droppedUploads.join(', ')}`,
    )
  }

  const intendedNotes = originalNotesBodies(intendedText).map((body) =>
    normalizeDescription(body, declared),
  )
  const storedNotes = originalNotesBodies(storedText).map((body) =>
    normalizeDescription(body, declared),
  )
  if (
    intendedNotes.length !== storedNotes.length ||
    intendedNotes.some((body, index) => body !== storedNotes[index])
  ) {
    return drift(
      WRITEBACK_CAUSES.NOTES,
      'stored `## Original notes` block differs from the intended block under the same normalization',
    )
  }

  const normalizedIntended = normalizeDescription(intendedText, declared)
  const normalizedStored = normalizeDescription(storedText, declared)
  if (normalizedIntended !== normalizedStored) {
    // The only conjunct that cannot tell a cosmetic reshape from a loss. Every CONTENT check above
    // has already passed on these same bytes: the contract's sections are all present, every upload
    // identity survived, and the verbatim block matches. What is left is a difference in the
    // drafter's own prose that this copy's transform vocabulary does not have a name for — which is
    // as likely to be a tracker normalization nobody has catalogued yet as it is to be a defect.
    //
    // Advisory by default, and `block` is available for a repo that wants the old behaviour. The
    // verdict is UNATTRIBUTED either way, so a reader is never told "drift" about bytes whose
    // content checks all passed.
    const declaredList = declared.size === 0 ? 'none declared' : [...declared].sort().join(', ')
    const blocking = severity === 'block'
    return {
      verdict: WRITEBACK_VERDICTS.UNATTRIBUTED,
      cause: WRITEBACK_CAUSES.UNATTRIBUTED,
      exitCode: blocking ? 1 : 0,
      // Word this as what was CHECKED, not as what is true. "No content loss was detected" reads as
      // "the content is intact", and it is not the same claim: the three conjuncts cover the
      // contract's section set, the verbatim block and the upload identities, so a dropped line of
      // the drafter's own body prose passes all three and lands here. Saying which checks passed
      // lets a reader see the gap; asserting a clean bill of health hides it.
      reason:
        `stored description differs from the intended bytes outside the declared transform set ` +
        `(${declaredList}) (first difference at ${at}); the semantic contract, the verbatim block ` +
        `and every upload identity are intact, so no content-loss check fired — but body prose is ` +
        `not compared line-by-line, so read the diff at that location` +
        (blocking ? '' : ' — reported, not fatal'),
      line,
      column,
      tolerated: declared,
    }
  }

  return {
    verdict: WRITEBACK_VERDICTS.NORMALIZED_EQUIVALENT,
    cause: WRITEBACK_CAUSES.NORMALIZED,
    exitCode: 0,
    reason:
      `stored description differs only by declared transforms (${[...declared].sort().join(', ')}); ` +
      'the semantic contract, the verbatim block and every upload identity survived the write',
    line,
    column,
    tolerated: declared,
  }
}

export function parseWritebackVerifyArgs(argv) {
  const args = { intended: null, stored: null, mode: 'child-plan' }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    if (flag === '--intended') {
      args.intended = argv[(i += 1)]
    } else if (flag === '--stored') {
      args.stored = argv[(i += 1)]
    } else if (flag === '--mode') {
      args.mode = assertWritebackMode(argv[(i += 1)])
    } else {
      throw new Error(`unknown argument: ${flag}`)
    }
  }
  if (!args.intended) throw new Error('--intended <path> is required')
  if (!args.stored) throw new Error('--stored <path> is required')
  return args
}

function main() {
  const { intended, stored, mode } = parseWritebackVerifyArgs(process.argv.slice(2))

  // A read failure is NOT drift and is never a pass. Keep the two distinguishable in the message and
  // in the output shape: a refusal emits no verdict line at all, so a reader (and a grep) cannot
  // mistake an unverifiable run for a measured one.
  let intendedText
  try {
    intendedText = readFileSync(intended, 'utf8')
  } catch (error) {
    console.error(
      `plan-writeback-verify: cannot read intended description ${intended}: ${error.message}`,
    )
    process.exitCode = 1
    gateRecorder.record('fire', 'unreadable-intended')
    return
  }
  let storedText
  try {
    storedText = readFileSync(stored, 'utf8')
  } catch (error) {
    console.error(
      `plan-writeback-verify: cannot read stored description ${stored}: ${error.message}`,
    )
    process.exitCode = 1
    gateRecorder.record('fire', 'unreadable-stored')
    return
  }

  const config = loadSkillConfig({ cwd: process.cwd() })
  const result = verifyWriteback({ config, intendedText, storedText, mode })
  if (result.verdict === null) {
    console.error(`plan-writeback-verify: ${result.reason}`)
    process.exitCode = result.exitCode
    gateRecorder.record('fire', 'unverifiable')
    return
  }

  // Record the CAUSE, not the verdict. The verdict is already on stdout for the caller; the
  // telemetry line's job is to answer "which conjunct fired, and how often" later, and a bare
  // `drift` could not. Verdict and cause are 1:1 except across `drift`'s three causes, which is
  // exactly the distinction that was missing.
  gateRecorder.record(result.exitCode === 0 ? 'pass' : 'fire', result.cause ?? result.verdict)

  console.log(`writeback-verdict: ${result.verdict}`)
  console.log(`plan-writeback-verify: ${result.reason}`)

  // An advisory verdict is a PASS that must still be seen. Put it on stderr too, and say plainly
  // what the reader is expected to do, so a warning that scrolls past in a headless log is not
  // mistaken for silence.
  if (result.exitCode === 0 && result.verdict === WRITEBACK_VERDICTS.UNATTRIBUTED) {
    console.error(`writeback-verdict: ${result.verdict}`)
    console.error(`plan-writeback-verify: ${result.reason}`)
    console.error(
      'plan-writeback-verify: retain the run scratch and name this verdict in the run report; ' +
        'do NOT attempt a corrective rewrite — the description is already stored',
    )
  }

  if (result.exitCode !== 0) {
    // The verdict is the thing the caller defers to, so a FAILING run must not leave it on stdout
    // alone. A caller that captured only stderr would otherwise have the diagnosis without the
    // machine-readable word for it, and would have to infer `drift` from the exit status. No
    // caller does that today — the one skill-body call site captures neither stream — so this is a
    // guarantee for the next caller, not a fix for an observed one. Repeat it rather than move it: a
    // caller already grepping stdout for `writeback-verdict:` keeps working unchanged, and a
    // REFUSAL still emits no verdict line on either stream, which is what keeps an unverifiable
    // run distinguishable from a measured one.
    console.error(`writeback-verdict: ${result.verdict}`)
    console.error(`plan-writeback-verify: ${result.reason}`)
    console.error(
      'plan-writeback-verify: the description is ALREADY stored — do not attempt a corrective rewrite; ' +
        'retain the scratch and triage the diff',
    )
    process.exitCode = result.exitCode
  }
}

// One gate-outcome line per invocation. The recorder LATCHES, so the branch that names
// a verdict or a read failure records it precisely and this wrapper's exit-code-derived call is a
// no-op for it — while an argument-parse throw, which never reaches main's body, still records.
const gateRecorder = createGateRecorder('plan-writeback-verify')

// isMainModule resolves both paths through symlinks so this fail-closed CLI gate cannot be skipped.
const invokedDirectly = isMainModule(import.meta.url)
if (invokedDirectly) {
  try {
    main()
    gateRecorder.record(process.exitCode ? 'fire' : 'pass', process.exitCode ? 'violations' : 'ok')
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    // A throw stays FATAL, unlike the advisory `unattributed` verdict, and the difference is not an
    // inconsistency. `unattributed` is a MEASURED result: all three content conjuncts ran and
    // passed, so content loss is positively ruled out and only an unnamed cosmetic difference is
    // left. A throw measured nothing, so it rules out nothing — it is the same category as the
    // empty-read-back refusal above, which this file already fails by explicit design. Passing here
    // would mean a broken verifier silently certifies every write, which is the one failure mode a
    // fidelity gate must not have. Crashes were ~1% of recorded invocations; the false-BLOCKED
    // problem this split addresses was 55%, and it is entirely in the verdict path.
    console.error(
      'plan-writeback-verify: the gate itself failed, so the write is UNVERIFIED — this is not a ' +
        'drift finding. The description is already stored; do NOT attempt a corrective rewrite. ' +
        'Retain the scratch and compare the two files by hand.',
    )
    process.exitCode = 1
    gateRecorder.record('fire', 'guard-threw')
  }
}
