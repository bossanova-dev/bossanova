/**
 * Render rewrite-prone references (issue identifiers, PR references, bare URLs, repo paths and
 * file citations) inside helper-composed markdown prose as inline code spans so trackers cannot
 * autolink them on save. Wrapping cross-workspace identifiers or tokens like `UTF-8` is accepted
 * harmless over-matching.
 */

import { inlineCodeEnd } from '../markdown-prose.mjs'
import { PLANNING_BULLET_KEYS } from '../skill-config.mjs'

const ISSUE_ID_RE = /(?<![A-Za-z0-9])[A-Za-z][A-Za-z0-9]*-\d+(?![A-Za-z0-9])/
const PR_REF_RE = /^(?:#\d+|[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+#\d+)$/
const URL_RE = /^https?:\/\//i
const FILE_REF_RE =
  /^(?:\.?[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*)\.[A-Za-z][A-Za-z0-9]{1,7}(?::\d+(?:-\d+)?)?$/
const TRAILING_PUNCT_RE = /[.,;:!?"']/
const TRAILING_ALL_PUNCT_RE = /^[)\],.;:!?"']*$/

/**
 * Wrap a single known value in a markdown inline code span wide enough not to collide with
 * embedded backticks.
 * @param {string} text
 * @returns {string}
 */
export function codeRef(text) {
  const longest = Math.max(0, ...(text.match(/`+/g) ?? []).map((run) => run.length))
  const fence = '`'.repeat(longest + 1)
  const pad = /^`|`$/.test(text) ? ' ' : ''
  return `${fence}${pad}${text}${pad}${fence}`
}

/** @param {string} token @returns {boolean} */
function hasReference(token) {
  if (!token) return false
  return (
    ISSUE_ID_RE.test(token) ||
    PR_REF_RE.test(token) ||
    URL_RE.test(token) ||
    (token.includes('/') && /[A-Za-z0-9_-]/.test(token)) ||
    FILE_REF_RE.test(token)
  )
}

/**
 * True when an opening bracket or parenthesis at index 0 closes strictly before the trailing
 * punctuation of `text` (e.g. `[id]/page.tsx` or `(group)/page.tsx`).
 * @param {string} text
 * @param {string} open
 * @param {string} close
 * @returns {boolean}
 */
function closesBeforeTrailingPunct(text, open, close) {
  let depth = 0
  for (let i = 0; i < text.length; i += 1) {
    if (text[i] === open) depth += 1
    else if (text[i] === close && --depth === 0) {
      return !TRAILING_ALL_PUNCT_RE.test(text.slice(i + 1))
    }
  }
  return false
}

/** @param {string} token @returns {{ prefix: string, body: string, suffix: string }} */
function peelToken(token) {
  let prefix = ''
  let body = token
  while (body) {
    const first = body[0]
    if (first === '"' || first === "'") {
      prefix += first
      body = body.slice(1)
    } else if (
      (first === '(' && !closesBeforeTrailingPunct(body, '(', ')')) ||
      (first === '[' && !closesBeforeTrailingPunct(body, '[', ']'))
    ) {
      prefix += first
      body = body.slice(1)
    } else break
  }
  let suffix = ''
  while (body) {
    const last = body.at(-1) ?? ''
    const unbalancedParen =
      last === ')' && (body.match(/\)/g)?.length ?? 0) > (body.match(/\(/g)?.length ?? 0)
    const unbalancedBracket =
      last === ']' && (body.match(/\]/g)?.length ?? 0) > (body.match(/\[/g)?.length ?? 0)
    if (!TRAILING_PUNCT_RE.test(last) && !unbalancedParen && !unbalancedBracket) break
    suffix = last + suffix
    body = body.slice(0, -1)
  }
  return { prefix, body, suffix }
}

/** @param {string} text @param {number} minimumFence @returns {string} */
function renderProse(text, minimumFence) {
  return text.replace(/\S+/g, (token) => {
    const { prefix, body, suffix } = peelToken(token)
    if (!hasReference(body)) return token
    const rendered = codeRef(body)
    const openRun = /^`+/.exec(rendered)?.[0].length ?? 1
    const extra = '`'.repeat(Math.max(0, minimumFence - openRun))
    return prefix + extra + rendered + extra + suffix
  })
}

/** @param {string} line @returns {number} */
function unmatchedFenceWidth(line) {
  let width = 1
  for (let i = 0; i < line.length;) {
    if (line[i] !== '`') {
      i += 1
      continue
    }
    const openWidth = /^`+/.exec(line.slice(i))?.[0].length ?? 1
    const end = inlineCodeEnd(line, i)
    if (end === i + openWidth) width = Math.max(width, openWidth + 1)
    i = end
  }
  return width
}

/** @param {string} line @param {number} start @returns {number} */
function linkEnd(line, start) {
  const bracket = line[start] === '!' ? start + 1 : start
  if (line[bracket] !== '[') return start
  let depth = 0
  let i = bracket
  for (; i < line.length; i += 1) {
    if (line[i] === '\\') i += 1
    else if (line[i] === '[') depth += 1
    else if (line[i] === ']' && --depth === 0) break
  }
  if (depth !== 0 || line[i + 1] !== '(') return start
  depth = 1
  for (i += 2; i < line.length; i += 1) {
    if (line[i] === '\\') i += 1
    else if (line[i] === '(') depth += 1
    else if (line[i] === ')' && --depth === 0) return i + 1
  }
  return start
}

/**
 * Render reference-bearing prose tokens as inline code spans while preserving authored literal
 * markup (existing code spans, markdown links/images, angle-bracket autolinks, HTML/mention
 * elements and HTML comments) byte-identically.
 * @param {string} line
 * @returns {string}
 */
export function inertReferences(line) {
  const minimumFence = unmatchedFenceWidth(line)
  const label = /^([^`[<\r\n]*?): /.exec(line)?.[1]
  const labelEnd =
    label &&
    (PLANNING_BULLET_KEYS.includes(label) ||
      !label.split(/\s+/).some((token) => hasReference(peelToken(token).body)))
      ? label.length + 2
      : 0
  let out = line.slice(0, labelEnd)
  let proseStart = labelEnd
  for (let i = labelEnd; i < line.length;) {
    let end = i
    if (line[i] === '`') {
      const openEnd = i + (/^`+/.exec(line.slice(i))?.[0].length ?? 1)
      const closeEnd = inlineCodeEnd(line, i)
      if (closeEnd > openEnd) end = closeEnd
      else {
        i = openEnd
        continue
      }
    } else if (line[i] === '[' || (line[i] === '!' && line[i + 1] === '[')) {
      end = linkEnd(line, i)
    } else if (line[i] === '<') {
      const rest = line.slice(i)
      const html =
        /^<!--[\s\S]*?-->/.exec(rest) ??
        /^<([a-z][\w-]*)\b(?:"[^"]*"|'[^']*'|[^'">])*?(?<!\/)>[\s\S]*?<\/\1\s*>/i.exec(rest) ??
        /^<(?![\s<])(?:"[^"]*"|'[^']*'|[^'">])+>/.exec(rest)
      if (html) end = i + html[0].length
    }
    if (end > i) {
      out += renderProse(line.slice(proseStart, i), minimumFence) + line.slice(i, end)
      i = end
      proseStart = end
    } else i += 1
  }
  return out + renderProse(line.slice(proseStart), minimumFence)
}
