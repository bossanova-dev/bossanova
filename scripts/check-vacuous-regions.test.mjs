#!/usr/bin/env node

// Falsification suite for the raw-region-slice gate (BOS-737).
//
// Discipline and rationale: docs/solutions/best-practices/feed-a-new-gate-the-values-it-exists-to-reject.md
// That doc's step 1 — enumerate the bypass shapes the gate's own input grammar permits,
// from the grammar rather than from imagination — is the work this file records. The two
// grammars are `RAW_REGION_SLICE` and `OPT_OUT` in scripts/check-vacuous-regions.mjs; every
// shape below was read out of one of them and then fed to the detector to see what it did.
//
// Both directions are pinned. A gate that fires on everything is as useless as one that
// fires on nothing, so the shapes the detector must NOT flag — including the converted
// `region(...)` form this ticket exists to produce — are asserted just as explicitly as the
// shapes it must.
//
// SCOPE, ASSERTED RATHER THAN COMMENTED: the detector is deliberately parser-free, so it has
// exact, knowable limits. The cases named "documented scope limit" pin those limits so they
// are visible to a reader and so a future widening of the pattern shows up here as a
// deliberate edit rather than as a silent change in what the repo is protected against.
//
// SAFETY: every probe below runs against an in-memory fixture string, and the two repo-scan
// cases inject a stub filesystem. Nothing here reads, writes, or mutates a real source file,
// and no probe touches the working tree — that destructive shape is exactly what BOS-770
// exists to replace.
//
// This file is the one entry in SCAN_EXCLUSIONS, because it must carry the forbidden shape
// verbatim as fixture text. That exclusion is itself pinned below.

import assert from 'node:assert/strict'
import path from 'node:path'
import test from 'node:test'

import {
  SCANNED_ROOTS,
  SCAN_EXCLUSIONS,
  UNFALSIFIABLE_PROSE_PIN_FILES,
  findVacuousRegions,
  findVacuousRegionsInRepo,
} from './check-vacuous-regions.mjs'

// --- The shape the gate exists to find --------------------------------------------------

test('detects the single-line raw region slice', () => {
  const source = [
    'const skill = read()',
    "const preflight = skill.slice(skill.indexOf('## Preflight'), skill.indexOf('## Step 1:'))",
    'assert.doesNotMatch(preflight, /~45 min/i)',
  ].join('\n')

  const offenders = findVacuousRegions(source)
  assert.equal(offenders.length, 1, 'exactly one offender, not zero and not a double count')
  assert.equal(offenders[0].line, 2)
  assert.equal(offenders[0].receiver, 'skill')
})

test('detects the multi-line form Prettier produces', () => {
  // Verbatim shape from scripts/proof-agent-finalize.golden.test.mjs:498 as it stood before
  // this ticket converted it (commit 1b3f00a72). A per-line regex misses this entirely, so a
  // line-anchored detector would have shipped blind to part of the very population it exists
  // to find — see the mutation case below, which proves this assertion is load-bearing.
  const source = [
    'test(...)',
    '  const tuiSection = commentBody.slice(',
    "    commentBody.indexOf('#### TUI'),",
    "    commentBody.indexOf('#### Web'),",
    '  )',
    "  assert.ok(!tuiSection.includes('scene-web'))",
  ].join('\n')

  const offenders = findVacuousRegions(source)
  assert.equal(offenders.length, 1)
  assert.equal(offenders[0].receiver, 'commentBody')
  // Reported at the receiver's line, which is the line a reader jumps to.
  assert.equal(offenders[0].line, 2)
})

test('a line-anchored detector would miss the multi-line form — this gate does not', () => {
  // Step 4 of the falsification recipe: mutate and confirm the test dies. Rather than
  // duplicate the pattern (a copy would drift, which is its own vacuity), feed the REAL
  // detector one line at a time. That is exactly what a line-anchored implementation sees.
  const multiLine = [
    '  const tuiSection = commentBody.slice(',
    "    commentBody.indexOf('#### TUI'),",
    "    commentBody.indexOf('#### Web'),",
    '  )',
  ].join('\n')

  const perLine = multiLine.split('\n').flatMap((line) => findVacuousRegions(line))
  assert.equal(perLine.length, 0, 'the line-anchored view finds nothing — the mutant is blind')
  assert.equal(findVacuousRegions(multiLine).length, 1, 'the whole-file view finds it')
})

test('detects the shape through arbitrary interior whitespace', () => {
  // `\s*` sits between every token of the grammar, so these are all the same shape. Pinned
  // because "reformat it until the gate stops seeing it" is the cheapest possible bypass.
  const spaced = 'const a = body . slice ( body . indexOf ( x ) )'
  const wrapped = 'const b = body\n  .slice(\n    body\n      .indexOf(x),\n  )'

  assert.equal(findVacuousRegions(spaced).length, 1)
  assert.equal(findVacuousRegions(wrapped).length, 1)
})

test('detects a one-argument slice and an underscore/dollar receiver', () => {
  assert.equal(findVacuousRegions("const b = prompt.slice(prompt.indexOf('## Diff'))").length, 1)
  assert.equal(findVacuousRegions('const c = _src.slice(_src.indexOf(m))').length, 1)
  assert.equal(findVacuousRegions('const d = $x.slice($x.indexOf(m))').length, 1)
})

test('raw-section-window detects fixed-distance section slices', () => {
  const source = [
    'const start = skill.indexOf("## Step 6")',
    'const window = skill.slice(start, start + 80)',
    'const head = body.slice(0, 1200)',
  ].join('\n')

  const offenders = findVacuousRegions(source).filter((o) => o.rule === 'raw-section-window')
  assert.deepEqual(
    offenders.map((o) => ({ line: o.line, receiver: o.receiver })),
    [
      { line: 2, receiver: 'skill' },
      { line: 3, receiver: 'body' },
    ],
  )
})

test('raw-section-window detects hand-rolled next-heading terminators', () => {
  const source = [
    'const end = rest.search(/\\n#{1,3} /)',
    'const alsoEnd = rest.search(/\\n#{2,3}\\s/)',
    "const next = skill.indexOf('\\n## ', start + 1)",
    'const backtick = skill.indexOf(`\\n## `, start + 1)',
  ].join('\n')

  const offenders = findVacuousRegions(source).filter((o) => o.rule === 'raw-section-window')
  assert.deepEqual(
    offenders.map((o) => o.line),
    [1, 2, 3, 4],
  )
})

test('unfalsifiable-prose-pin detects whole-document flattened prohibitions', () => {
  const source = [
    "const flat = skill.replace(/\\s+/g, ' ')",
    'assert.doesNotMatch(flat, /git\\s+push/)',
  ].join('\n')

  const offenders = findVacuousRegions(source).filter((o) => o.rule === 'unfalsifiable-prose-pin')
  assert.deepEqual(
    offenders.map((o) => ({ line: o.line, receiver: o.receiver })),
    [{ line: 2, receiver: 'flat' }],
  )
})

test('unfalsifiable-prose-pin detects unbounded whole-file prohibitions', () => {
  const offenders = findVacuousRegions('assert.doesNotMatch(source, /git\\s+push/)').filter(
    (o) => o.rule === 'unfalsifiable-prose-pin',
  )
  assert.deepEqual(
    offenders.map((o) => ({ line: o.line, receiver: o.receiver })),
    [{ line: 1, receiver: 'source' }],
  )
})

test('unfalsifiable-prose-pin allows bounded prohibitions and reasoned opt-outs', () => {
  const bounded = [
    "const section = sectionRegion(skill, '## Rule')",
    'assert.doesNotMatch(section, /git\\s+push/)',
  ].join('\n')
  const optedOut = [
    "const flat = skill.replace(/\\s+/g, ' ')",
    '// gate-region-ok: flattened whole-doc check is a historical corpus audit',
    'assert.doesNotMatch(flat, /git\\s+push/)',
  ].join('\n')

  assert.deepEqual(
    findVacuousRegions(bounded).filter((o) => o.rule === 'unfalsifiable-prose-pin'),
    [],
  )
  assert.deepEqual(
    findVacuousRegions(optedOut).filter((o) => o.rule === 'unfalsifiable-prose-pin'),
    [],
  )
})

// --- The opt-out grammar ----------------------------------------------------------------

const OFFENCE = "  const p = skill.slice(skill.indexOf('a'), skill.indexOf('b'))"

test('an opt-out with a non-empty reason suppresses the offence', () => {
  const sameLine = `${OFFENCE} // gate-region-ok: array slice, not a source region`
  const lineBefore = ['  // gate-region-ok: array slice, not a source region', OFFENCE].join('\n')
  const sectionWindow = [
    '  // gate-region-ok: reviewed parser slice, not a section gate',
    '  const excerpt = body.slice(start, start + 700)',
  ].join('\n')

  assert.equal(findVacuousRegions(sameLine).length, 0)
  assert.equal(findVacuousRegions(lineBefore).length, 0)
  assert.equal(findVacuousRegions(sectionWindow).length, 0)
})

test('an opt-out with an empty reason does NOT suppress the offence', () => {
  // The escape hatch stays auditable only while the reason is mandatory: an unexplained
  // opt-out is the next vacuous gate, silencing a real offence with a token nobody reviewed.
  for (const marker of [
    '  // gate-region-ok:',
    '  // gate-region-ok: ',
    '  // gate-region-ok:    \t  ',
  ]) {
    assert.equal(
      findVacuousRegions([marker, OFFENCE].join('\n')).length,
      1,
      `an empty reason must not suppress: ${JSON.stringify(marker)}`,
    )
    assert.equal(
      findVacuousRegions(`${OFFENCE} ${marker.trim()}`).length,
      1,
      `an empty reason must not suppress inline: ${JSON.stringify(marker)}`,
    )
  }
})

test('an opt-out two lines above does NOT suppress the offence', () => {
  // The grammar admits the match's own line and the one immediately before it, and nothing
  // further. Pinned because a multi-line explanatory comment naturally pushes the marker out
  // of range — which is a silent un-suppression, not an error, so only a test catches it.
  const tooFarUp = [
    '  // gate-region-ok: a real reason, but stated too far above the match',
    '  // (a second comment line pushes the marker out of the two-line window)',
    OFFENCE,
  ].join('\n')

  assert.equal(findVacuousRegions(tooFarUp).length, 1)

  // Same reason text, marker moved onto the adjacent line: suppressed. This control is what
  // makes the assertion above about DISTANCE rather than about the reason text.
  const inRange = [
    '  // (a second comment line, now above the marker instead of below it)',
    '  // gate-region-ok: a real reason, stated on the adjacent line',
    OFFENCE,
  ].join('\n')
  assert.equal(findVacuousRegions(inRange).length, 0)
})

test('a `//` inside a string literal does NOT suppress the offence', () => {
  // Raised by the cross-model outside voice: while the opt-out was a bare regex over raw
  // lines, `const s = '// gate-region-ok: x'` above an offence suppressed it with nothing a
  // reviewer would ever read as an opt-out — the escape hatch opening itself. Closed by a
  // single-line quote-state scan in `insideStringLiteral`.
  const inString = `${OFFENCE.replace('  const p', "  const s = '// gate-region-ok: not really'\n  const p")}`
  assert.equal(findVacuousRegions(inString).length, 1)

  // Same on the offence's OWN line, the other half of the two-line window.
  assert.equal(findVacuousRegions(`${OFFENCE} + '// gate-region-ok: nope'`).length, 1)
})

test('documented scope limit: the comment test is not a tokenizer, and where it fails', () => {
  // The two inputs that still suppress, and why closing either needs a tokenizer, are stated
  // ONCE in the `OPT_OUT_SCOPE_LIMITS` block of scripts/check-vacuous-regions.mjs. This case is
  // the executable half: it PINS the limits rather than re-describing them, so a future widening
  // shows up here as a deliberate edit. Do not restate the rationale here — it lived in three
  // places at once, and that is how the block's earlier "fail-closed in every direction" claim
  // survived being false. A wrong documented invariant is worse than an acknowledged gap, since
  // it stops the next reviewer looking.
  //
  // Limit 1 — a marker inside a MULTI-LINE template literal: its own line's quotes are balanced.
  const inTemplate = ['  const t = `', '  // gate-region-ok: not really', OFFENCE].join('\n')
  assert.equal(findVacuousRegions(inTemplate).length, 0)

  // Limit 2 — a regex literal earlier on the line UN-POISONS the state.
  const unpoisoned = [`  const x = /'/ ; const s = '// gate-region-ok: nope'`, OFFENCE].join('\n')
  assert.equal(findVacuousRegions(unpoisoned).length, 0)
})

test('only the leftmost marker on a line is considered — fail-closed, not fail-open', () => {
  // A string-literal marker ahead of a genuine trailing one abandons the line, so the real
  // opt-out is not seen and the offence IS reported. Pinned to fix the direction: this costs
  // an author a confusing red, and must never be "fixed" into suppressing instead.
  const decoyFirst = [`  const s = '// gate-region-ok: nope' // gate-region-ok: real`, OFFENCE]
  assert.equal(findVacuousRegions(decoyFirst.join('\n')).length, 1)
})

test('a real trailing comment still suppresses, even after code on the line', () => {
  // The control that keeps the fix above from overshooting, and it is not hypothetical: the
  // live opt-out at sweep-releases-gate.mjs:42 spells its marker after a ternary `?`, so the
  // obvious tightening — "the marker must be the first non-whitespace on its line" — would
  // un-suppress a reviewed opt-out and turn the gate red on correct code. Both shapes below
  // must stay suppressed.
  const afterTernary = ['      ? // gate-region-ok: a real reason, stated after a ternary', OFFENCE]
  assert.equal(findVacuousRegions(afterTernary.join('\n')).length, 0)
  assert.equal(findVacuousRegions(`${OFFENCE} // gate-region-ok: a real trailing reason`).length, 0)

  // A CLOSED string earlier on the line leaves quote state balanced, so the marker after it is
  // still a comment. Pinned so the scanner is not read as "any quote anywhere disables".
  const afterClosedString = [`  const k = 'x' // gate-region-ok: a real reason`, OFFENCE]
  assert.equal(findVacuousRegions(afterClosedString.join('\n')).length, 0)
})

// --- The shapes the gate must NOT flag --------------------------------------------------

test('reports zero offenders on converted region(...) source', () => {
  // The negative case that proves the gate is not simply always-firing. This is the exact
  // form the 48 converted call sites now take, so a gate that flagged it would have made
  // this ticket's own change unlandable.
  const converted = [
    "import { region, regionUntilNext } from './gate-region-lib.mjs'",
    '',
    "const preflight = region(skill, '## Preflight', '## Step 1:')",
    "const step = sectionRegion(skill, '## Step 6:')",
    "const body = region(prompt, '## Diff')",
    "const returnObject = regionUntilNext(skill, '```json', '```')",
    'const windowed = region(reviewSkill, tier).slice(0, 500)',
    'const section = (from, to) => region(reviewSkill, from, to || null)',
  ].join('\n')

  assert.deepEqual(findVacuousRegions(converted), [])
})

test('reports zero offenders on slices that are not the forbidden shape', () => {
  const benign = [
    // Different receivers: not a self-referential region extraction.
    'const a = head.slice(body.indexOf(m))',
    // An index held in a variable, which the caller is free to have checked.
    'const b = body.slice(start, end)',
    // A small plain numeric token trim, below the section-window threshold.
    'const c = body.slice(0, 50)',
    // `indexOf` with no slice at all.
    "const d = body.indexOf('## Diff')",
    // A receiver whose name merely CONTAINS another's — the backreference is exact.
    'const e = body.slice(bodyText.indexOf(m))',
  ].join('\n')

  assert.deepEqual(findVacuousRegions(benign), [])
})

test('documented scope limit: only the receiver-first argument position is matched', () => {
  // Every "live instance" claim below is DATED AND LOCATED (as of BOS-737), never absolute.
  // An absolute "none exists" goes stale the moment one does, and it is exactly the sentence a
  // later reader trusts instead of re-scanning — which is how a documented limit quietly
  // becomes an undetected hole.
  //
  // `X.slice(0, X.indexOf(m))` goes vacuous by the same mechanism — `indexOf` returns -1 and
  // `slice(0, -1)` hands back nearly the whole receiver instead of the intended head, so a
  // POSITIVE assertion over it can be satisfied by text the window exists to exclude. The plan
  // scoped this gate to the receiver-first form, so the shape below is NOT flagged.
  // Live as of BOS-737, two instances in the scanned roots — enumerated in full, as the
  // sibling bullet below is, so the two bullets teach the same thing about completeness:
  //   - boss-build-skill.test.mjs:3583 was exactly this and fail-OPEN (a widened window let
  //     the degraded tier's own copy satisfy a tier-selection assertion). Converted by hand;
  //     the gate never saw it.
  //   - sweep-pr-gate.test.mjs:542 `subject.slice(0, subject.indexOf(': ') + 2)` — left as
  //     is, and benign: a -1 makes `prefix` one character and `rest` the remainder, so the
  //     reassembled `${prefix}[#7] ${rest}` stops matching the guard pattern and the
  //     `assert.equal(foreign(...), '')` assertions fail loudly.
  assert.deepEqual(findVacuousRegions("const a = body.slice(0, body.indexOf('## Step 8:'))"), [])

  // Same class: a member-expression receiver defeats the single-identifier backreference.
  // Live as of BOS-737: none in the scanned roots.
  assert.deepEqual(findVacuousRegions('const b = o.text.slice(o.text.indexOf(m))'), [])

  // And a CALL-expression receiver, for the same reason. Live as of BOS-737:
  // boss-build-skill.test.mjs:7964 was exactly this and walked straight through the gate; it
  // was converted by hand. The fix for a site like that is to hoist the receiver into a local,
  // which makes it a plain identifier and brings it back inside this gate's reach.
  assert.deepEqual(findVacuousRegions('const c = read(d).slice(read(d).indexOf(m))'), [])

  // The grammar names `slice` and `indexOf` literally, so the sibling methods that share the
  // -1 sentinel are outside it. `lastIndexOf` and `search` are the same vacuity exactly
  // (`slice(-1)` is one character); `substring`/`substr` clamp or swap instead, so they
  // misread the region a different way. None is flagged. Live as of BOS-737, three instances
  // in the scanned roots, none of them currently harmful — and each for a DIFFERENT reason,
  // which is why they are left rather than converted:
  //   - check-skill-shell.mjs:219   `prefix.slice(prefix.lastIndexOf('>') + 1)` — the `+ 1`
  //     maps -1 to 0, the idiom's intended behaviour. Benign by construction.
  //   - boss-build-skill.test.mjs:6294 — quoted verbatim so a grep for it lands, since this
  //     block is a reader-facing map rather than prose:
  //     `emitted.slice(emitted.lastIndexOf('PUSHED=') + 'PUSHED='.length)`. Misreads silently
  //     on a missing marker, but `emitted` is already guarded and every consumer compares the
  //     result to a literal, so it fails loudly today.
  //   - boss-build-skill.test.mjs:6418
  //     `block.slice(0, block.search(/^if \[ "\$REMOTE_HAS_HEAD"/m))` — a -1 WIDENS the
  //     region, and the assertion over it is `doesNotMatch`, which a wider region only makes
  //     harder to satisfy. Fail-closed.
  //
  // The HOISTED-INDEX family — `const i = src.indexOf(m); src.slice(i, j)` — is the same
  // vacuity (-1 makes the region empty) and is by far the most populous shape in the scanned
  // roots (~40 sites). It is outside this grammar by construction: the detector matches the
  // bounds spelled INLINE, and a hoisted bound is a bare identifier the regex cannot trace.
  // Named here so a reader can tell it was considered rather than missed. Live as of BOS-737:
  // every region-extracting site in the scanned roots is either explicitly `-1`/relational
  // guarded, or fail-closed because only positive `assert.match` runs over it.
  assert.deepEqual(findVacuousRegions('const d = body.slice(body.lastIndexOf(m))'), [])
  assert.deepEqual(findVacuousRegions('const e = body.slice(body.search(/m/))'), [])
  assert.deepEqual(
    findVacuousRegions('const f = body.substring(body.indexOf(m), body.indexOf(n))'),
    [],
  )
})

test('documented scope limit: the ORDERING shape is fail-open too, and this gate never sees it', () => {
  // `assert.ok(src.indexOf(a) < src.indexOf(b))` is the same vacuity family one operator over: a
  // deleted `a` makes `indexOf` return -1, and `-1 < n` is TRUE for every real index, so the
  // ordering assertion passes on exactly the value it exists to forbid. Nothing is sliced, so
  // this gate's grammar cannot reach it and no widening of `RAW_REGION_SLICE` would — the fix is
  // `precedes()` in scripts/gate-region-lib.mjs, which throws naming the marker that moved.
  assert.deepEqual(findVacuousRegions("assert.ok(md.indexOf('## A') < md.indexOf('## B'))"), [])

  // Recorded here rather than left silent, because the branch documents its other limits and an
  // undocumented one is the asymmetry a reader trusts. DATED AND LOCATED, never absolute.
  // Converted to `precedes()` by BOS-737 — the twelve instances inside the files this ticket
  // already touches:
  //   - boss-build-skill.test.mjs   x3   (caller-deadline ordering; the two `10#$name` guards)
  //   - proof-brief.test.mjs        x6
  //   - proof-playwright-runner.test.mjs x3
  // NOT converted, live as of BOS-737 — eleven instances of `X.indexOf(a) < X.indexOf(b)` in five
  // files this ticket does not otherwise touch, so converting them would be a scope expansion
  // rather than a ratchet. Enumerated in full so a grep for any one of them lands here:
  //   - scripts/proof-video.test.mjs:510            x1
  //   - scripts/format-affected.test.mjs:433        x1  (array receiver)
  //   - scripts/proof-lib.test.mjs:2011, 2114, 2783 x3
  //   - scripts/proof-publish-report.test.mjs:330   x1  (array receiver)
  //   - skills-toolbox/plan-epic-lib.test.mjs:403, 454, 455, 456, 457  x5  (array receiver)
  // Each is fail-open in the same way: none guards its operands against -1. They are a follow-up,
  // and `precedes()` accepts an array receiver as well as a string, so each is a one-line change.
  assert.deepEqual(findVacuousRegions("assert.ok(order.indexOf('c1') < order.indexOf('c3'))"), [])
})

// --- The scanned set and its one exclusion ----------------------------------------------

test('SCAN_EXCLUSIONS holds exactly this test file and nothing else', () => {
  // A third escape hatch beyond the two `gate-region-ok:` opt-outs. If it grew silently, the
  // PR's "exactly two opt-outs repo-wide" claim would be quietly false and a whole file could
  // leave the gate's coverage with no comment anywhere naming it. Pinned by exact value.
  assert.deepEqual(SCAN_EXCLUSIONS, ['scripts/check-vacuous-regions.test.mjs'])
  assert.deepEqual(SCANNED_ROOTS, ['scripts', 'skills-toolbox'])
  assert.deepEqual(UNFALSIFIABLE_PROSE_PIN_FILES, [
    'scripts/boss-build-skill.test.mjs',
    'scripts/boss-review-skill.test.mjs',
  ])
})

test('the repo scan honours SCAN_EXCLUSIONS and skips node_modules', () => {
  // Driven against an injected stub filesystem: the exclusion is asserted behaviourally, not
  // just as a constant, and no real file is read. `findVacuousRegionsInRepo` takes the same
  // `deps.fs` seam `findMjsFiles` does.
  const files = {
    'scripts/real-gate.mjs': OFFENCE,
    'scripts/gcp-lb-affinity.test.mjs': 'assert.doesNotMatch(source, /x/)',
    'scripts/boss-build-skill.test.mjs': 'assert.doesNotMatch(source, /x/)',
    'scripts/check-vacuous-regions.test.mjs': OFFENCE,
    'scripts/node_modules/vendored.mjs': OFFENCE,
    'skills-toolbox/helper.mjs': OFFENCE,
  }
  const repoRoot = '/repo'
  const abs = (rel) => path.join(repoRoot, ...rel.split('/'))

  const dirs = new Map()
  for (const rel of Object.keys(files)) {
    const parts = rel.split('/')
    for (let i = 0; i < parts.length; i++) {
      const dir = abs(parts.slice(0, i).join('/'))
      if (!dirs.has(dir)) dirs.set(dir, new Map())
      dirs.get(dir).set(parts[i], i === parts.length - 1)
    }
  }

  const stubFs = {
    existsSync: (dir) => dirs.has(dir),
    readdirSync: (dir) =>
      [...(dirs.get(dir) ?? new Map())].map(([name, isFile]) => ({
        name,
        isDirectory: () => !isFile,
      })),
    readFileSync: (file) => files[path.relative(repoRoot, file).split(path.sep).join('/')],
  }

  const found = findVacuousRegionsInRepo(repoRoot, { fs: stubFs })
  const relative = found.map((o) => path.relative(repoRoot, o.file).split(path.sep).join('/'))

  assert.deepEqual(relative.sort(), [
    'scripts/boss-build-skill.test.mjs',
    'scripts/real-gate.mjs',
    'skills-toolbox/helper.mjs',
  ])
})
