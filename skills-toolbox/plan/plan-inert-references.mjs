import { inlineCodeEnd } from '../markdown-prose.mjs'
import { PLANNING_BULLET_KEYS } from '../skill-config.mjs'

/** @param {string} text */
export function codeRef(text) {
  const longest = Math.max(0, ...(text.match(/`+/g) ?? []).map((run) => run.length))
  const fence = '`'.repeat(longest + 1)
  const pad = /^`|`$/.test(text) ? ' ' : ''
  return `${fence}${pad}${text}${pad}${fence}`
}

/** @param {string} token */
function hasReference(token) {
  return /https?:\/\/|^#\d+|\[#\d+\]|[\w.-]+\/[\w.-]+#\d+|(?:^|[^a-z0-9])[a-z][a-z0-9]{0,9}-[1-9]\d*(?=$|[^a-z0-9])/i.test(
    token,
  )
}

/** @param {string} text @param {number} minimumFence */
function renderProse(text, minimumFence) {
  return text.replace(/\S+/g, (token) => {
    const prefix = /^\(*/.exec(token)[0]
    let body = token.slice(prefix.length)
    let suffix = ''
    while (body) {
      const last = body.at(-1)
      const unbalanced =
        last === ')' && (body.match(/\)/g)?.length ?? 0) > (body.match(/\(/g)?.length ?? 0)
      if (!/[.,;:!?]/.test(last) && !unbalanced) break
      suffix = last + suffix
      body = body.slice(0, -1)
    }
    if (!hasReference(body)) return token
    const rendered = codeRef(body)
    const extra = '`'.repeat(Math.max(0, minimumFence - /^`+/.exec(rendered)[0].length))
    return prefix + extra + rendered + extra + suffix
  })
}

/** @param {string} line */
function unmatchedFenceWidth(line) {
  let width = 1
  for (let i = 0; i < line.length;) {
    if (line[i] !== '`') {
      i += 1
      continue
    }
    const openWidth = /^`+/.exec(line.slice(i))[0].length
    const end = inlineCodeEnd(line, i)
    if (end === i + openWidth) width = Math.max(width, openWidth + 1)
    i = end
  }
  return width
}

/** @param {string} line @param {number} start */
function linkEnd(line, start) {
  if (line[start] !== '[') return start
  let depth = 0
  let i = start
  for (; i < line.length; i += 1) {
    if (line[i] === '\\') i += 1
    else if (line[i] === '[') depth += 1
    else if (line[i] === ']' && --depth === 0) break
  }
  if (line[i + 1] !== '(') return start
  depth = 1
  for (i += 2; i < line.length; i += 1) {
    if (line[i] === '\\') i += 1
    else if (line[i] === '(') depth += 1
    else if (line[i] === ')' && --depth === 0) return i + 1
  }
  return start
}

/** Render reference-bearing prose tokens while preserving authored literal markup.
 * @param {string} line */
export function inertReferences(line) {
  const minimumFence = unmatchedFenceWidth(line)
  const label = /^([^`[<]*?): /.exec(line)?.[1]
  const labelEnd =
    label &&
    (PLANNING_BULLET_KEYS.includes(label) ||
      !label.split(/\s+/).some((token) => hasReference(token.replace(/^\(+/, ''))))
      ? label.length + 2
      : 0
  let out = line.slice(0, labelEnd)
  let proseStart = labelEnd
  for (let i = labelEnd; i < line.length;) {
    let end = i
    if (line[i] === '`') {
      const openEnd = i + /^`+/.exec(line.slice(i))[0].length
      const closeEnd = inlineCodeEnd(line, i)
      if (closeEnd > openEnd) end = closeEnd
      else {
        i = openEnd
        continue
      }
    } else if (line[i] === '[') end = linkEnd(line, i)
    else if (line[i] === '<') {
      const element = /^<([a-z][\w-]*)\b(?:"[^"]*"|'[^']*'|[^'">])*?>.*?<\/\1\s*>/i.exec(
        line.slice(i),
      )
      const angle = element ?? /^<(?:"[^"]*"|'[^']*'|[^'">])*>/.exec(line.slice(i))
      if (angle) end = i + angle[0].length
    }
    if (end > i) {
      out += renderProse(line.slice(proseStart, i), minimumFence) + line.slice(i, end)
      i = end
      proseStart = end
    } else i += 1
  }
  return out + renderProse(line.slice(proseStart), minimumFence)
}
