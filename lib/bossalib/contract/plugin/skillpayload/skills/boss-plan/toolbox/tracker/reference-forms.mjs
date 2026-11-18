import { inlineCodeEnd, mapProseLines } from '../markdown-prose.mjs'

/** Reference transform order, shared by the pre-write and post-write description gates. */
export const REFERENCE_TRANSFORM_IDS = Object.freeze([
  'bare-reference-autolinking',
  'pull-request-reference-autolinking',
])

const ISSUE_IDENTIFIER = /^[A-Z][A-Z0-9]{0,9}-[1-9][0-9]*$/
const PR_LABEL = /^[^\s/#\[\]`]+\/[^\s/#\[\]`]+#([1-9][0-9]*)$/

const absoluteHttp = (destination) => {
  try {
    const url = new URL(destination)
    return /^https?:$/.test(url.protocol) ? url : null
  } catch {
    return null
  }
}

const referenceLinkAt = (line, start) => {
  if (line[start] !== '[') return null
  let depth = 1
  let end = start + 1
  for (; end < line.length && depth > 0; end += 1) {
    if (line[end] === '\\') end += 1
    else if (line[end] === '[') depth += 1
    else if (line[end] === ']') depth -= 1
  }
  if (depth !== 0) return null
  const label = line.slice(start + 1, end - 1)
  const labelEnd = end
  if (line[end] !== '(') return { end, label, destination: null }
  const destinationStart = ++end
  const angled = line[end] === '<'
  if (angled) {
    const close = line.indexOf('>', end + 1)
    if (close < 0) return { end: labelEnd, label, destination: null }
    end = close + 1
  }
  depth = 1
  for (; end < line.length && depth > 0; end += 1) {
    if (line[end] === '\\') end += 1
    else if (line[end] === '(') depth += 1
    else if (line[end] === ')') depth -= 1
  }
  if (depth !== 0) return { end: labelEnd, label, destination: null }
  const raw = line.slice(destinationStart, end - 1)
  const destination = angled ? /^<([^<>\s]+)>$/.exec(raw)?.[1] : /^[^<>\s]+$/.exec(raw)?.[0]
  return { end, label, destination }
}

const mentionAt = (line, start, tag) => {
  const opening = `<${tag}`
  if (!line.startsWith(opening, start)) return null
  const tail = line.slice(start + opening.length)
  const element = /^(?:\s+[^\s"'=<>`/]+\s*=\s*(?:"[^"]*"|'[^']*'))*\s*\/>/.exec(tail)
  if (!element) return null
  const attributes = [...element[0].matchAll(/([^\s"'=<>`/]+)\s*=\s*(?:"([^"]*)"|'([^']*)')/g)]
  const labels = attributes.filter((attribute) => attribute[1] === 'label')
  if (labels.length !== 1) return null
  return { end: start + opening.length + element[0].length, label: labels[0][2] ?? labels[0][3] }
}

const rewriteReferences = (line, tag, linkValue, mentionValue, unwrapAngles = false) => {
  if (/^ {0,3}\[[^\]]+\]:/.test(line)) return line
  let out = ''
  for (let i = 0; i < line.length;) {
    if (line[i] === '\\' || line[i] === '`') {
      const end = line[i] === '`' ? inlineCodeEnd(line, i) : i + 2
      const escaped =
        line[i] === '\\' && (referenceLinkAt(line, i + 1) || mentionAt(line, i + 1, tag))
      const literalEnd = escaped ? escaped.end : end
      out += line.slice(i, literalEnd)
      i = literalEnd
      continue
    }
    const mention = mentionAt(line, i, tag)
    if (mention) {
      out += mentionValue(mention.label) ?? line.slice(i, mention.end)
      i = mention.end
      continue
    }
    const link = referenceLinkAt(line, i)
    if (link) {
      out +=
        (line[i - 1] !== '!' && linkValue(link.label, link.destination)) || line.slice(i, link.end)
      i = link.end
      continue
    }
    const angle = unwrapAngles && /^<(https?:\/\/[^<>\s]+)>/.exec(line.slice(i))
    if (angle) {
      out += angle[1]
      i += angle[0].length
      continue
    }
    out += line[i++]
  }
  return out
}

/**
 * Preserve the existing URL/domain/identifier derivability rule and accept self-closing issue
 * mentions with an identifier label. Literal code, images and escaped links stay unchanged.
 * Side-agnostic: removing an authored domain or identifier link also compares equal.
 */
export function canonicalizeBareReferences(text) {
  return mapProseLines(text, (line) =>
    rewriteReferences(
      line,
      'issue',
      (label, destination) => {
        const url = absoluteHttp(destination)
        const identifier =
          url && ISSUE_IDENTIFIER.test(label) && url.pathname.split('/').includes(label)
        const self = /^https?:\/\//.test(label) && destination === label
        const domain =
          /^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+(?:[:/][^\s]*)?$/.test(label) &&
          (destination === 'http://' + label || destination === 'https://' + label)
        return /^[^\s\[\]<>`]+$/.test(label) && (self || domain || identifier) ? label : null
      },
      (label) => (ISSUE_IDENTIFIER.test(label) ? label : null),
      true,
    ),
  )
}

/**
 * Map owner/repo#N links with absolute HTTP(S) destinations and self-closing pull-request mentions
 * to #N, outside code, images and escaped links. N is positive without leading zeroes. A tracker
 * backslash in owner/repo is accepted. Side-agnostic residual: deleting an authored PR link also
 * compares equal; an intended bare owner/repo#N stays distinct because it was not measured.
 */
export function canonicalizePullRequestReferences(text) {
  const value = (label) => {
    const match = PR_LABEL.exec(label)
    return match ? `#${match[1]}` : null
  }
  return mapProseLines(text, (line) =>
    rewriteReferences(
      line,
      'pull-request',
      (label, destination) => absoluteHttp(destination) && value(label),
      value,
    ),
  )
}

/** Apply only declared reference transforms; an empty tolerated set is the identity. */
export function normalizeReferenceForms(text, tolerated) {
  let out = String(text ?? '')
  if (tolerated.has(REFERENCE_TRANSFORM_IDS[0])) out = canonicalizeBareReferences(out)
  if (tolerated.has(REFERENCE_TRANSFORM_IDS[1])) out = canonicalizePullRequestReferences(out)
  return out
}
