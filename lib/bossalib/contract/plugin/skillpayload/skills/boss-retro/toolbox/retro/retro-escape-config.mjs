import path from 'node:path'
import { isDeepStrictEqual } from 'node:util'
import { globToRegExp, verifyPolicyFromConfigText } from '../skill-config.mjs'
import { AREAS, GENERATED_GLOBS, NO_SIGN_OFF_AREA, reviewAreas } from '../verify/verify-areas.mjs'

export const MAX_PRS = 3
export const BRANCH_PREFIX = 'boss-retro/escape-'
export const SKIP_REASONS = Object.freeze(['no-paths', 'generated', 'covered', 'already-asks'])

/** @typedef {{number: number, title?: string, mergedAt?: string}} Pull
 * @typedef {{original: Pull, fix: Pull, basis: string, areas: {area: string, paths: string[]}[], areaBasis: string}} EscapeRow
 * @typedef {{original: Pull, fix: Pull, basis: string, name: string, globs: string[], paths: string[], branch: string}} Proposal
 */
const generated = GENERATED_GLOBS.map(globToRegExp)
/** @param {Pick<EscapeRow, 'original' | 'fix'>} row */
export const escapeBranch = (row) => `${BRANCH_PREFIX}${row.original.number}-${row.fix.number}`

/** @param {EscapeRow[]} rows @param {import('../verify/verify-review-policy.mjs').ReviewPolicy} review */
export function proposeAlwaysReview(rows, review) {
  /** @type {Proposal[]} */
  const proposals = []
  /** @type {{original: Pull, fix: Pull, reason: string}[]} */
  const skipped = []
  for (const row of rows) {
    const identity = { original: row.original, fix: row.fix }
    const allPaths = [...new Set(row.areas.flatMap((entry) => entry.paths))].sort()
    if (row.areaBasis !== 'overlap' || !allPaths.length) {
      skipped.push({ ...identity, reason: 'no-paths' })
      continue
    }
    const authored = allPaths.filter((file) => !generated.some((pattern) => pattern.test(file)))
    if (!authored.length) {
      skipped.push({ ...identity, reason: 'generated' })
      continue
    }
    const { areas } = reviewAreas(authored, review)
    // An escape through a no-sign-off path is exactly what the proposal is for, so it is not covered.
    const covered = new Set(
      areas
        .filter((entry) => entry.source === 'config' && entry.area !== NO_SIGN_OFF_AREA)
        .flatMap((entry) => entry.paths),
    )
    const asked = new Set(
      areas
        .filter((entry) => entry.source === 'builtin' && entry.defaultAsk === 'always')
        .flatMap((entry) => entry.paths),
    )
    const paths = authored.filter((file) => !covered.has(file) && !asked.has(file))
    if (!paths.length) {
      skipped.push({
        ...identity,
        reason: authored.every((file) => covered.has(file)) ? 'covered' : 'already-asks',
      })
      continue
    }
    const directories = [...new Set(paths.map((file) => path.posix.dirname(file)))].filter(
      (dir) => dir !== '.',
    )
    const globs = [
      ...directories
        .filter((dir) => !directories.some((other) => other !== dir && dir.startsWith(`${other}/`)))
        .map((dir) => `${dir}/**`),
      ...paths.filter((file) => path.posix.dirname(file) === '.'),
    ].sort()
    const area = row.areas.find((entry) => entry.paths.some((file) => paths.includes(file)))
    const label =
      AREAS.find((entry) => entry.area === area?.area)?.label ?? area?.area ?? 'Other changes'
    const name = `Escaped: ${label}`
    proposals.push({
      ...identity,
      basis: row.basis,
      name: Array.from(name).length <= 60 && !/[\r\n]/.test(name) ? name : 'Escaped',
      globs,
      paths,
      branch: escapeBranch(row),
    })
  }
  return { proposals, skipped }
}

/** @typedef {{start: number, end: number, members?: {key: string, value: Span}[]}} Span */
/** @param {string} text @returns {Span} */
function scan(text) {
  let cursor = 0
  const whitespace = () => {
    while (/\s/.test(text[cursor] ?? '') && cursor < text.length) cursor++
  }
  const string = () => {
    const start = cursor++
    while (cursor < text.length) {
      const char = text[cursor++]
      if (char === '\\') cursor++
      else if (char === '"') return JSON.parse(text.slice(start, cursor))
    }
    throw new Error('unterminated string')
  }
  /** @returns {Span} */
  const value = () => {
    whitespace()
    const start = cursor
    if (text[cursor] === '{') {
      cursor++
      /** @type {{key: string, value: Span}[]} */
      const members = []
      whitespace()
      while (text[cursor] !== '}') {
        const key = string()
        whitespace()
        cursor++
        members.push({ key, value: value() })
        whitespace()
        if (text[cursor] !== ',') break
        cursor++
        whitespace()
      }
      cursor++
      return { start, end: cursor, members }
    }
    if (text[cursor] === '[') {
      cursor++
      whitespace()
      while (text[cursor] !== ']') {
        value()
        whitespace()
        if (text[cursor] !== ',') break
        cursor++
      }
      cursor++
    } else if (text[cursor] === '"') string()
    else {
      while (cursor < text.length && !/[\s,}\]]/.test(text[cursor])) cursor++
    }
    return { start, end: cursor }
  }
  return value()
}

/** @param {string} text @param {Span} object @param {string} key @param {unknown} value @param {string} unit */
function spliceMember(text, object, key, value, unit) {
  const members = object.members ?? []
  const existing = members.find((entry) => entry.key === key)
  const prefix = text.slice(0, object.start).match(/(?:^|\n)([\t ]*)[^\n]*$/)?.[1] ?? ''
  const child = `${prefix}${unit}`
  const rendered = JSON.stringify(value, null, unit).replace(/\n/g, `\n${child}`)
  if (existing) {
    const indent = text.slice(0, existing.value.start).match(/(?:^|\n)([\t ]*)[^\n]*$/)?.[1] ?? ''
    const replacement = JSON.stringify(value, null, unit).replace(/\n/g, `\n${indent}`)
    return text.slice(0, existing.value.start) + replacement + text.slice(existing.value.end)
  }
  const multiline = text.slice(object.start, object.end).includes('\n')
  const last = members.at(-1)
  const offset = last?.value.end ?? object.start + 1
  const addition = multiline
    ? `${last ? ',' : ''}\n${child}${JSON.stringify(key)}: ${rendered}`
    : `${last ? ', ' : ''}${JSON.stringify(key)}: ${JSON.stringify(value)}`
  return text.slice(0, offset) + addition + text.slice(offset)
}

/** Minimal edit, checked against the expected object and verify's base policy validator.
 * @param {string | null | undefined} configText @param {Pick<Proposal, 'name' | 'globs'>} proposal
 * @returns {string | {unchanged: true} | {error: string}}
 */
export function applyProposal(configText, proposal) {
  try {
    const text = configText ?? '{}'
    verifyPolicyFromConfigText(text, 'escape config')
    const original = JSON.parse(text)
    const defaults = original.verifyDefaults ?? {}
    const always = defaults.alwaysReview ?? {}
    const globs = [
      ...new Set([
        ...(Object.hasOwn(always, proposal.name) ? always[proposal.name] : []),
        ...proposal.globs,
      ]),
    ]
    const expected = {
      ...original,
      verifyDefaults: { ...defaults, alwaysReview: { ...always, [proposal.name]: globs } },
    }
    if (isDeepStrictEqual(original, expected)) return { unchanged: true }
    const indent = text.match(/\n([\t ]+)"/)?.[1] ?? '  '
    let result
    try {
      let object = scan(text)
      const keys = ['verifyDefaults', 'alwaysReview', proposal.name]
      let value = expected
      for (const key of keys) {
        value = value[key]
        const member = object.members?.find((entry) => entry.key === key)
        if (!member || key === proposal.name) {
          result = spliceMember(text, object, key, value, indent)
          break
        }
        object = member.value
      }
      if (!isDeepStrictEqual(JSON.parse(result), expected)) result = undefined
    } catch {}
    result ??= JSON.stringify(expected, null, indent) + '\n'
    if (configText == null) result = JSON.stringify(expected, null, indent) + '\n'
    verifyPolicyFromConfigText(result, 'escape config')
    return result
  } catch {
    return { error: 'invalid-config' }
  }
}

/** @param {Proposal} proposal */
export const prTitle = (proposal) =>
  `Ask for review after escape #${proposal.original.number}: ${proposal.name}`
/** @param {Proposal} proposal */
export function prBody(proposal) {
  return `Propose review coverage after #${proposal.original.number}, fixed by #${proposal.fix.number} (${proposal.basis}).\n\nShared paths:\n${proposal.paths.map((file) => `- \`${file}\``).join('\n')}\n\nAdd \`${proposal.name}\` to \`verifyDefaults.alwaysReview\`:\n${proposal.globs.map((glob) => `- \`${glob}\``).join('\n')}\n\nEvery future PR touching these paths asks a person for review.\n\nA person must review and merge this PR because \`.boss-skills.json\` is agent rules. No automated build or verify stage acts on it. Closing it unmerged is a valid answer; the retro will not reopen it.\n`
}
