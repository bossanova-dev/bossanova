// Line-oriented reader for the GitHub Actions YAML that the workflow gates check
// (scripts/check-action-pins.mjs, scripts/check-vacuous-job-gates.mjs).
//
// The repo carries no YAML parser for scripts (`yaml` is only a pnpm override), so the gates read
// structure line by line. This module is the one place that decides what a quoted scalar, a
// comment, a block-scalar body and a mapping key look like, so a quoting or escaping fix lands in
// every gate at once.

import fs from 'node:fs'
import path from 'node:path'

export const WORKFLOW_DIR = '.github/workflows'
export const WORKFLOW_EXTENSIONS = ['.yml', '.yaml']

// A block-scalar indicator ending a line, after any node properties (`run: &x |`, `- !!str >-`).
const BLOCK_SCALAR = /(?:^|:\s|^(?:-\s+)+)\s*(?:[&!]\S*\s+)*[|>][+-]?[1-9]?[+-]?$/

// YAML double-quoted escapes that are not a plain "drop the backslash".
const DOUBLE_QUOTED_ESCAPES = {
  0: '\0',
  a: '\x07',
  b: '\b',
  t: '\t',
  n: '\n',
  v: '\v',
  f: '\f',
  r: '\r',
  e: '\x1b',
  N: '\x85',
  _: '\xa0',
  L: ' ',
  P: ' ',
}

export function indentOf(raw) {
  return raw.length - raw.trimStart().length
}

// Per-character flags marking which characters of `text` sit inside a quoted scalar, quote
// characters included. A quote only opens a scalar where one can start (line start, after a
// flow indicator `{` `[` `,`, after `:`/`-`/`?` plus whitespace, or right after a quoted key's
// `:`), so the apostrophe in a plain `name: it's` is literal. Inside double quotes a backslash
// escapes the next character; inside single quotes `''` is an escaped quote and a backslash is
// literal.
export function quotedMask(text) {
  const mask = new Array(text.length).fill(false)
  let quote = null
  for (let i = 0; i < text.length; i++) {
    const ch = text[i]
    if (quote) {
      mask[i] = true
      if (quote === '"' && ch === '\\' && i + 1 < text.length) {
        mask[++i] = true
      } else if (quote === "'" && ch === "'" && text[i + 1] === "'") {
        mask[++i] = true
      } else if (ch === quote) {
        quote = null
      }
      continue
    }
    if ((ch === "'" || ch === '"') && scalarCanStart(text, i, mask)) {
      quote = ch
      mask[i] = true
    }
  }
  return mask
}

function scalarCanStart(text, i, mask) {
  let j = i - 1
  while (j >= 0 && /\s/.test(text[j])) j--
  if (j < 0) return true
  const prev = text[j]
  if (prev === '{' || prev === '[' || prev === ',') return true
  // JSON-style `"key":"value"` may put the value directly after the colon.
  if (prev === ':') return j < i - 1 || (j > 0 && mask[j - 1])
  if ((prev === '-' || prev === '?') && j < i - 1) return j === 0 || /\s/.test(text[j - 1])
  return false
}

// Index of the `#` that opens a YAML comment, or -1. A `#` only starts a comment at the line
// start or after whitespace, and never inside a quoted scalar.
export function commentStart(text) {
  const mask = quotedMask(text)
  for (let i = 0; i < text.length; i++) {
    if (!mask[i] && text[i] === '#' && (i === 0 || /\s/.test(text[i - 1]))) return i
  }
  return -1
}

// Length of the leading `- ` list markers on a structural line (`- - key` nests two).
function listMarkerLength(text) {
  return /^(?:-\s+)*/.exec(text)[0].length
}

// Column of the first mapping key on a structural line: its indent plus any `- ` markers.
export function keyColumn(entry) {
  return entry.indent + listMarkerLength(entry.text)
}

// The column a block scalar opened on `entry` ends at: a body line must be indented deeper.
// For `- name: >-` that is the key's column, so the step's sibling keys are structure again;
// for a keyless `- |` it is the dash's column, so a body at the key column stays body.
function scalarEndColumn(entry) {
  const markers = listMarkerLength(entry.text)
  if (splitKey(entry.text.slice(markers))) return entry.indent + markers
  return markers > 0 ? entry.indent + entry.text.slice(0, markers).lastIndexOf('-') : entry.indent
}

// Every line of `text`, classified: `blank`, `comment` (comment-only), `scalar` (inside a
// block-scalar body, where YAML has no comments) or `structural` (with its trailing comment
// stripped into `text`). Every line yields one entry, in order; line numbers are 1-based.
export function classifyLines(text) {
  const out = []
  const raws = text.split('\n')
  let scalarIndent = -1
  for (let i = 0; i < raws.length; i++) {
    const raw = raws[i].replace(/\r$/, '')
    const line = i + 1
    if (raw.trim() === '') {
      out.push({ line, kind: 'blank', raw })
      continue
    }
    const indent = indentOf(raw)
    if (scalarIndent >= 0) {
      if (indent > scalarIndent) {
        out.push({ line, kind: 'scalar', raw, indent })
        continue
      }
      scalarIndent = -1
    }
    const body = raw.trimStart()
    const cut = commentStart(body)
    const content = (cut >= 0 ? body.slice(0, cut) : body).trimEnd()
    if (content === '') {
      out.push({ line, kind: 'comment', raw, indent })
      continue
    }
    const entry = { line, kind: 'structural', raw, indent, text: content }
    out.push(entry)
    if (BLOCK_SCALAR.test(content)) scalarIndent = scalarEndColumn(entry)
  }
  return out
}

// Structural lines: blank lines, comment-only lines and block-scalar bodies dropped, trailing
// comments stripped. Each entry keeps its 1-based line number and indentation.
export function structuralLines(text) {
  return classifyLines(text)
    .filter((entry) => entry.kind === 'structural')
    .map(({ line, indent, text }) => ({ line, indent, text }))
}

// The value of a scalar, with one level of single or double quoting removed and its escapes
// decoded. Anything else is returned trimmed.
export function unquote(value) {
  const v = value.trim()
  if (v.length < 2 || !(v[0] === "'" || v[0] === '"') || v.at(-1) !== v[0]) return v
  const body = v.slice(1, -1)
  if (v[0] === "'") return body.replaceAll("''", "'")
  return decodeEscapes(body)
}

// `text` with every YAML double-quoted escape decoded (`\n`, `\"`, `\x6e`, `\u0020`, …). An
// out-of-range code point is left as written.
export function decodeEscapes(text) {
  return text.replace(/\\(x[0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}|[\s\S])/g, (_, esc) => {
    if (esc.length === 1) return DOUBLE_QUOTED_ESCAPES[esc] ?? esc
    const codePoint = Number.parseInt(esc.slice(1), 16)
    return codePoint <= 0x10ffff ? String.fromCodePoint(codePoint) : `\\${esc}`
  })
}

// A mapping key at the start of `text` and the text after its `:`. A plain key needs whitespace
// or the line end after the colon (`uses:x` is one plain scalar, not a key); a quoted key may be
// followed by the colon directly (JSON style). Whitespace before the colon is allowed either way.
function splitKey(text) {
  const plain = /^([A-Za-z0-9_-]+)[ \t]*:(?=\s|$)/.exec(text)
  if (plain) return { key: plain[1], rest: text.slice(plain[0].length).trim() }
  const quoted = /^("(?:[^"\\]|\\[\s\S])*"|'(?:[^']|'')*')[ \t]*:/.exec(text)
  if (quoted) return { key: unquote(quoted[1]), rest: text.slice(quoted[0].length).trim() }
  return null
}

// `key: value` on a structural line, allowing leading `- ` list markers. Quoted keys are
// decoded, so `"uses"`, `'uses'` and `uses :` all read as `uses`.
export function keyValue(text) {
  const kv = splitKey(text.slice(listMarkerLength(text)))
  return kv ? { key: kv.key, value: kv.rest === '' ? '' : unquote(kv.rest) } : null
}

// Top-level `key: value` pairs of a one-line flow mapping (`- { a: 1, b: 'x, y' }`), splitting
// on commas outside quotes and nested braces/brackets. Null when the line is not a flow mapping.
export function flowPairs(text) {
  const m = /^(?:-\s+)?\{(.*)\}$/.exec(text)
  if (!m) return null
  const body = m[1]
  const mask = quotedMask(body)
  const parts = []
  let depth = 0
  let start = 0
  for (let i = 0; i < body.length; i++) {
    if (mask[i]) continue
    const c = body[i]
    if (c === '{' || c === '[') depth++
    else if (c === '}' || c === ']') depth--
    else if (c === ',' && depth === 0) {
      parts.push(body.slice(start, i))
      start = i + 1
    }
  }
  parts.push(body.slice(start))
  return parts.flatMap((part) => {
    const kv = splitKey(part.trim())
    return kv ? [{ key: kv.key, value: unquote(kv.rest) }] : []
  })
}

// Indexes of the structural lines nested under the line at `start`: every following line indented
// deeper than `column` (default: that line's key column), up to the first that is not.
export function nestedUnder(lines, start, column = keyColumn(lines[start])) {
  const out = []
  for (let i = start + 1; i < lines.length && lines[i].indent > column; i++) out.push(i)
  return out
}

/**
 * The workflow files directly under `dir`, as sorted repo-relative POSIX paths.
 *
 * @param {string} repoRoot Absolute repository root.
 * @param {{fs?: typeof fs, dir?: string, extensions?: string[]}} [opts]
 * @returns {string[]}
 */
export function listWorkflowFiles(
  repoRoot,
  { fs: fsImpl = fs, dir = WORKFLOW_DIR, extensions = WORKFLOW_EXTENSIONS } = {},
) {
  const root = path.join(repoRoot, ...dir.split('/'))
  if (!fsImpl.existsSync(root)) return []
  return fsImpl
    .readdirSync(root)
    .filter((name) => extensions.some((extension) => name.endsWith(extension)))
    .map((name) => `${dir}/${name}`)
    .sort()
}
