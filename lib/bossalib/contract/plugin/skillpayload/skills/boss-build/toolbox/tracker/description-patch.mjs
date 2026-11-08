// Compute anchored description deltas rather than asking a model to retranscribe a
// gated body. General hunks cover both additive planning notes and inline premise
// annotations. Simulation and byte cost decide whether a patch is safe to send;
// the stored-description write-back check remains authoritative after the save.

/** @typedef {{op: 'replace', old_string: string, new_string: string} | {op: 'insert_before' | 'insert_after', anchor: string, text: string}} DescriptionPatchOp */

function unique(text, anchor) {
  return (
    anchor.length > 0 &&
    text.indexOf(anchor) >= 0 &&
    text.indexOf(anchor) === text.lastIndexOf(anchor)
  )
}

/** Simulate sequential, exact-once tracker operations without modifying the input.
 * @param {string} base
 * @param {DescriptionPatchOp[]} ops
 */
export function applyDescriptionPatch(base, ops) {
  let current = base
  for (const op of ops) {
    const anchor = op.op === 'replace' ? op.old_string : op.anchor
    if (!unique(current, anchor)) throw new Error('patch anchor must match exactly once')
    const at = current.indexOf(anchor)
    const replacement =
      op.op === 'replace'
        ? op.new_string
        : op.op === 'insert_before'
          ? op.text + anchor
          : anchor + op.text
    current = current.slice(0, at) + replacement + current.slice(at + anchor.length)
  }
  return current
}

function previousLine(base, at) {
  return at <= 1 ? 0 : base.lastIndexOf('\n', at - 2) + 1
}

function nextLine(base, at) {
  const end = base.indexOf('\n', at)
  return end < 0 ? base.length : end + 1
}

function hunks(base, intended, offset) {
  const a = base.match(/[^\n]*\n|[^\n]+$/g) ?? []
  const b = intended.match(/[^\n]*\n|[^\n]+$/g) ?? []
  const lcs = Array.from({ length: a.length + 1 }, () => new Uint32Array(b.length + 1))
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? 1 + lcs[i + 1][j + 1] : Math.max(lcs[i + 1][j], lcs[i][j + 1])
    }
  }
  const result = []
  let i = 0,
    j = 0,
    at = offset
  while (i < a.length || j < b.length) {
    if (i < a.length && j < b.length && a[i] === b[j]) {
      at += a[i++].length
      j++
      continue
    }
    const start = at
    let text = ''
    while (i < a.length || j < b.length) {
      if (i < a.length && j < b.length && a[i] === b[j]) break
      if (j < b.length && (i === a.length || lcs[i][j + 1] > lcs[i + 1][j])) text += b[j++]
      else at += a[i++].length
    }
    result.push({ start, end: at, text })
  }
  return result
}

/** @returns {DescriptionPatchOp | null} */
function anchoredOp(base, { start, end, text }) {
  while (start < end && text && base[start] === text[0]) {
    start++
    text = text.slice(1)
  }
  while (start < end && text && base[end - 1] === text[text.length - 1]) {
    end--
    text = text.slice(0, -1)
  }
  if (start === end) {
    if (start > 0) {
      let left = previousLine(base, start)
      while (!unique(base, base.slice(left, start)) && left > 0) left = previousLine(base, left)
      const anchor = base.slice(left, start)
      if (unique(base, anchor)) return { op: 'insert_after', anchor, text }
    }
    let right = nextLine(base, end)
    while (!unique(base, base.slice(end, right)) && right < base.length)
      right = nextLine(base, right)
    const anchor = base.slice(end, right)
    return unique(base, anchor) ? { op: 'insert_before', anchor, text } : null
  }
  let left = start,
    right = end
  while (!unique(base, base.slice(left, right))) {
    if (left > 0) left = previousLine(base, left)
    else if (right < base.length) right = nextLine(base, right)
    else return null
  }
  return {
    op: 'replace',
    old_string: base.slice(left, right),
    new_string: base.slice(left, start) + text + base.slice(end, right),
  }
}

/** @param {string} base
 * @param {string} intended
 * @returns {{ops: DescriptionPatchOp[], reason?: undefined} | {ops: null, reason: string}}
 */
export function computeDescriptionPatch(base, intended, { maxOps = 50 } = {}) {
  const fallback = (reason) => ({ ops: null, reason })
  if (!base) return fallback('empty-base')
  if (base === intended) return fallback('identical')
  let prefix = 0,
    suffix = 0
  while (prefix < Math.min(base.length, intended.length) && base[prefix] === intended[prefix])
    prefix++
  while (
    suffix < Math.min(base.length, intended.length) - prefix &&
    base[base.length - 1 - suffix] === intended[intended.length - 1 - suffix]
  )
    suffix++
  const changes = hunks(
    base.slice(prefix, base.length - suffix),
    intended.slice(prefix, intended.length - suffix),
    prefix,
  )
  if (changes.length > maxOps) return fallback('max-ops-exceeded')
  /** @type {DescriptionPatchOp[]} */
  const ops = []
  for (const change of changes.reverse()) {
    const op = anchoredOp(base, change)
    if (!op) return fallback('no-unique-anchor')
    ops.push(op)
  }
  try {
    if (applyDescriptionPatch(base, ops) !== intended) return fallback('simulation-mismatch')
  } catch {
    return fallback('simulation-rejected')
  }
  if (Buffer.byteLength(JSON.stringify(ops)) >= Buffer.byteLength(intended))
    return fallback('patch-not-smaller')
  return { ops }
}
