#!/usr/bin/env node

// Fail-closed region extraction for gate and test sources (BOS-737).
//
// Extracting a section as `source.slice(i, j)`, where `i` and `j` each come from a
// `source.indexOf(…)` spelled inline in the argument list, is this repo's most common
// vacuity shape. `indexOf` returns -1 when a marker moves, `String.prototype.slice`
// reads -1 as "one character from the end", and the resulting 0-or-1-character region
// makes every `assert.doesNotMatch` / `!region.includes(…)` assertion over it pass
// while the forbidden content sits untouched in the file. The assertion goes green on
// exactly the value it exists to forbid.
//
// `region()` throws instead, so a moved marker is a red test that names the marker
// rather than a silent green. `scripts/check-vacuous-regions.mjs` is the gate that
// stops the raw shape coming back.
//
// `precedes()` closes the same vacuity one operator over. An ordering assertion written
// `assert.ok(src.indexOf(a) < src.indexOf(b))` passes when `a` disappears, because the
// missing marker's -1 is less than every real index — so it, too, goes green on exactly
// the value it exists to forbid. It is a distinct shape from the slice one, outside this
// ticket's gate by construction (nothing is sliced), which is why it is a helper here and
// not a detector there; the unconverted population is enumerated in
// scripts/check-vacuous-regions.test.mjs.

/**
 * Extract the substring `[startMarker, endMarker)` from `source`, fail-closed.
 *
 * On the happy path the return value is byte-identical to `source.slice(i, j)` with
 * `i = source.indexOf(startMarker)` and `j = source.indexOf(endMarker)` — both indices
 * are searched from position 0 — which is what makes converting an existing raw call
 * site behaviour-preserving.
 *
 * Throws, with a distinct marker-naming message, when:
 *   1. `startMarker` is absent,
 *   2. `endMarker` was supplied but is absent,
 *   3. `endMarker` was found before `startMarker`,
 *   4. the extracted region is empty or whitespace-only.
 *
 * @param {string} source Text to extract from.
 * @param {string} startMarker Literal marker the region starts at (inclusive).
 * @param {string|null} [endMarker] Literal marker the region ends at (exclusive).
 *   `null` (or `undefined`) means "to the end of `source`".
 * @param {string} [label] Name used to prefix the thrown messages. Pass the mirror or file this
 *   `source` came from wherever the caller loops over more than one: that is what makes a failure
 *   name the mirror that drifted and not only the marker that moved. The 26 sites in
 *   `scripts/boss-build-skill.test.mjs` that BOS-737 moved off that file's local `sliceSection`
 *   helper each pass an interpolated mirror path this way, as do its three `regionUntilNext()`
 *   API-gate extractions. Coverage there is NOT uniform, and this note must not be read as
 *   claiming it is: as of BOS-737 roughly half of that file's in-mirror-loop sites still take the
 *   default, so a bare `region:` prefix means "this site passed no label", never "the canonical
 *   mirror". Defaults to `'region'`.
 * @returns {string} The extracted region.
 */
export function region(source, startMarker, endMarker = null, label = 'region') {
  const text = String(source)
  const startIndex = text.indexOf(startMarker)
  if (startIndex === -1) {
    throw new Error(`${label}: start marker not found: ${JSON.stringify(startMarker)}`)
  }

  const hasEnd = endMarker !== null && endMarker !== undefined
  let endIndex = text.length
  if (hasEnd) {
    endIndex = text.indexOf(endMarker)
    if (endIndex === -1) {
      throw new Error(`${label}: end marker not found: ${JSON.stringify(endMarker)}`)
    }
    // Searched from 0, not from startIndex, so out-of-order markers stay observable
    // instead of collapsing into the "end marker not found" case.
    if (endIndex < startIndex) {
      throw new Error(
        `${label}: end marker ${JSON.stringify(endMarker)} precedes start marker ` +
          `${JSON.stringify(startMarker)}`,
      )
    }
  }

  const extracted = text.slice(startIndex, endIndex)
  if (extracted.trim() === '') {
    const end = hasEnd ? JSON.stringify(endMarker) : '<end of source>'
    throw new Error(
      `${label}: region between ${JSON.stringify(startMarker)} and ${end} is empty or ` +
        `whitespace-only`,
    )
  }
  return extracted
}

/**
 * Extract from `startMarker` to the FIRST `endMarker` at or after the end of `startMarker`,
 * fail-closed on the same four conditions as {@link region}.
 *
 * `region()` searches both markers from position 0, which is byte-identical to the raw form
 * it replaces but cannot express a delimiter that also opens the region — a fenced block,
 * where the closing ``` is a suffix of the opening ```json, is the case in this repo. The
 * raw form spells that as a `fromIndex` on the end search:
 * `src.slice(i, src.indexOf(end, i + startMarker.length))`. This helper is that, fail-closed.
 *
 * Deliberately separate from `region()` rather than a fifth parameter: the two search
 * origins give different answers on out-of-order markers, and a flag that silently changes
 * which index a caller gets is how the next vacuity lands.
 *
 * @param {string} source Text to extract from.
 * @param {string} startMarker Literal marker the region starts at (inclusive).
 * @param {string} endMarker Literal marker the region ends at (exclusive), searched from
 *   the first character after `startMarker`.
 * @param {string} [label] Name used to prefix the thrown messages — see {@link region}.
 * @returns {string} The extracted region.
 */
export function regionUntilNext(source, startMarker, endMarker, label = 'region') {
  const text = String(source)
  const startIndex = text.indexOf(startMarker)
  if (startIndex === -1) {
    throw new Error(`${label}: start marker not found: ${JSON.stringify(startMarker)}`)
  }

  const searchFrom = startIndex + String(startMarker).length
  const endIndex = text.indexOf(endMarker, searchFrom)
  if (endIndex === -1) {
    throw new Error(
      `${label}: end marker not found after ${JSON.stringify(startMarker)}: ` +
        `${JSON.stringify(endMarker)}`,
    )
  }

  const extracted = text.slice(startIndex, endIndex)
  if (extracted.trim() === '') {
    throw new Error(
      `${label}: region between ${JSON.stringify(startMarker)} and the next ` +
        `${JSON.stringify(endMarker)} is empty or whitespace-only`,
    )
  }
  return extracted
}

/**
 * True when `earlierMarker` occurs before `laterMarker` in `source`, fail-closed on absence.
 *
 * The shape this replaces is `source.indexOf(a) < source.indexOf(b)`. When `a` is absent
 * `indexOf` returns -1, and `-1 < n` is TRUE for every real index `n` — so the ordering
 * assertion passes precisely when the thing whose position it pins has been deleted. When `b`
 * is absent the same expression flips to a false negative. Both are silent; this throws
 * instead, naming the marker that moved.
 *
 * Returns a boolean rather than asserting, so a call site keeps its own `assert.ok(…, message)`
 * and can still combine several orderings with `&&`. Both arguments go through the receiver's
 * own `indexOf`, so an array of ordered tokens works exactly as a string does.
 *
 * @param {string|unknown[]} source Text (or array) the two markers are looked up in.
 * @param {unknown} earlierMarker Marker required to come first.
 * @param {unknown} laterMarker Marker required to come second.
 * @param {string} [label] Name used to prefix the thrown messages — see {@link region}.
 * @returns {boolean} True when `earlierMarker` strictly precedes `laterMarker`.
 */
export function precedes(source, earlierMarker, laterMarker, label = 'order') {
  const earlier = source.indexOf(earlierMarker)
  if (earlier === -1) {
    throw new Error(`${label}: earlier marker not found: ${JSON.stringify(earlierMarker)}`)
  }
  const later = source.indexOf(laterMarker)
  if (later === -1) {
    throw new Error(`${label}: later marker not found: ${JSON.stringify(laterMarker)}`)
  }
  return earlier < later
}
