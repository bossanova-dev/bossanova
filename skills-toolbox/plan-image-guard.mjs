#!/usr/bin/env node

// plan-image-guard — deterministic, LLM-untrusted guard that fails loud when boss-plan's
// rewritten Linear description drops any image the reporter embedded in the original. Extraction
// remains raw for useful failure reporting; comparisons canonicalize Linear upload URLs to their
// stable asset identity so rotating signatures do not produce false drops.
//
// Linear does not expose description history to agents, so a "helpful" summary that paraphrases
// an inline `![](https://uploads.linear.app/…)` into `[screenshot: …]` text destroys the only copy
// of the URL. boss-plan Phase 4 runs this as a hard pre-write gate: it compares the set of image
// URLs in the original description against the rewritten one and aborts the write-back (no
// half-planned issue) when any image was dropped. For uploads.linear.app assets, identity is the
// origin plus pathname; signatures and fragments remain raw in extraction but are ignored here.
//
// Node builtins only — this runs in dependency-light cron worktrees.

import { readFileSync } from 'node:fs'

import { isMainModule } from './main-module.mjs'

// Inline markdown image start: the destination is parsed below so balanced parentheses in a URL
// do not get mistaken for the wrapper's closing delimiter.
const INLINE_IMAGE_START = /!\[/g
// Ordinary Markdown links can carry credential-bearing destinations too. They are not image-parity
// inputs, but the publish-time secret gate must inspect them alongside image destinations.
const INLINE_LINK_START = /(?<!!)\[/g
// Reference-style Markdown image definitions. We resolve only definitions used by an image
// reference, so ordinary links do not become image-guard inputs.
const REFERENCE_DEFINITION =
  /^(?:(?: {0,3}>[\t ]?| {0,3}(?:[-+*]|\d{1,9}[.)])[\t ]+))* {0,3}\[((?:\\.|[^\]\\\r\n])+)\]:[\t ]*(?:\r?\n[\t ]*)?(?:<((?:\\.|[^>\r\n])*)>|([^\s]+))/gm
// HTML <img …src=url> — double quoted, single quoted, or unquoted. The prefix parses
// complete attributes, so a literal `>` inside a quoted value cannot end the tag and a
// credential-bearing unquoted URL receives the same checks as a quoted one.
const HTML_IMG_SRC =
  /<img\b(?:\s+(?!src\b)[^\s"'=<>`]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*\s+src\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>`]+))/gi
// HTML image-submit controls can load an image through src. Require type=image rather than
// treating every input src as an image destination or credential candidate.
const HTML_INPUT_IMAGE_SRC =
  /<input\b(?=(?:\s+[^\s"'=<>`]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*\s+type\s*=\s*(?:"image"|'image'|image)(?=\s|\/?>))(?:\s+(?!src\b)[^\s"'=<>`]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*\s+src\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>`]+))/gi
// HTML video elements can use poster as their rendered preview image. Treat it as an image
// destination so entity-encoded credentials cannot bypass the publish-time secret gate.
const HTML_VIDEO_POSTER =
  /<video\b(?:\s+(?!poster\b)[^\s"'=<>`]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*\s+poster\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>`]+))/gi
// HTML responsive-image candidates. Values may be double quoted, single quoted, or unquoted; each
// candidate starts with a URL followed by an optional density or width descriptor, so
// credential-bearing candidates in both <img> and <picture><source> pass through the same gate.
const HTML_RESPONSIVE_SRCSET =
  /<(?:img|source)\b(?:\s+(?!srcset\b)[^\s"'=<>`]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*\s+srcset\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>`]+))/gi
// HTML anchors are not image-parity inputs, but their href values are destinations that can carry
// credential-bearing URLs and therefore must be inspected before a plan is published.
const HTML_LINK_HREF =
  /<(?:a|area)\b(?:\s+(?!href\b)[^\s"'=<>`]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*\s+href\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>`]+))/gi
// SVG images can load external resources through href or the legacy xlink:href. Treat both as
// image destinations so entity-encoded credentials cannot bypass the publish-time secret gate.
const HTML_SVG_IMAGE_HREF =
  /<image\b(?:\s+(?!(?:xlink:)?href\b)[^\s"'=<>`]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*\s+(?:xlink:)?href\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>`]+))/gi
// Bare Linear upload/attachment asset URL in prose. Stop at whitespace and quote/bracket
// characters that fence markdown/HTML. Parentheses are parsed below because they can belong to
// an asset path as well as wrap a URL in prose.
const BARE_LINEAR_URL = /https?:\/\/uploads\.linear\.app\/[^\s<>\]"']*/gi
// Browsers remove ASCII tabs and newlines while parsing URLs. Accept their literal and entity
// spellings between URL syntax characters so decoded prose cannot hide a credential-bearing URL.
const URL_PARSER_CONTROLS = String.raw`(?:[\t\n\r]|&(?:#x0*9|#0*9|tab|newline);?)*`
// Bare external URLs in prose may be copied into a Screenshots section without Markdown image
// syntax. They are credential candidates even though they do not participate in image parity.
// Match HTML-encoded syntax and URL-parser controls so safe-source comparison canonicalizes the
// URL a renderer treats as the same destination.
const BARE_EXTERNAL_URL = new RegExp(
  String.raw`(?:h${URL_PARSER_CONTROLS}t${URL_PARSER_CONTROLS}t${URL_PARSER_CONTROLS}p${URL_PARSER_CONTROLS}s${URL_PARSER_CONTROLS}(?::|&colon;|&#(?:x0*3a|0*58);?)${URL_PARSER_CONTROLS}(?:(?:\/|&sol;|&#(?:x0*2f|0*47);?)${URL_PARSER_CONTROLS}){2}|\/${URL_PARSER_CONTROLS}\/)[^\s<>\]"']*`,
  'gi',
)

// Trailing prose/markdown punctuation to trim off a captured URL. Queries stay raw at extraction
// time and are stripped only by uploadIdentity during comparisons.
const TRAILING_PUNCT = /[\].,]+$/
// A safe source may replace non-image sensitive prose only with an explicit redaction marker.
// This lets the guard distinguish a permitted omission from arbitrary reporter-prose loss.
const REDACTION_MARKER = /\[redacted(?:\s*:[^\]\r\n]*)?\]/gi

const HTML_NAMED_ENTITIES = new Map([
  ['amp', '&'],
  ['apos', "'"],
  ['ast', '*'],
  ['bsol', '\\'],
  ['circ', '^'],
  ['colon', ':'],
  ['comma', ','],
  ['commat', '@'],
  ['dollar', '$'],
  ['equals', '='],
  ['excl', '!'],
  ['grave', '`'],
  ['gt', '>'],
  ['lcub', '{'],
  ['lowbar', '_'],
  ['lt', '<'],
  ['lpar', '('],
  ['lsqb', '['],
  ['newline', '\n'],
  ['num', '#'],
  ['percnt', '%'],
  ['period', '.'],
  ['plus', '+'],
  ['quest', '?'],
  ['quot', '"'],
  ['rcub', '}'],
  ['rpar', ')'],
  ['rsqb', ']'],
  ['semi', ';'],
  ['sol', '/'],
  ['tab', '\t'],
  ['tilde', '~'],
  ['vert', '|'],
])

// URLs in HTML attributes and Markdown destinations are decoded by their renderers before
// requests. Decode numeric references plus every named reference that can alter URL syntax or
// query parsing, so an encoded credential key or delimiter cannot evade the publish-time gate.
function decodeHtmlEntities(value) {
  return String(value).replace(
    /&(?:#x([0-9a-f]+);?|#([0-9]+);?|([a-z][a-z0-9]+));/gi,
    (raw, hex, decimal, named) => {
      if (hex || decimal) {
        const codePoint = Number.parseInt(hex || decimal, hex ? 16 : 10)
        if (Number.isSafeInteger(codePoint) && codePoint >= 0 && codePoint <= 0x10ffff) {
          return String.fromCodePoint(codePoint)
        }
        return raw
      }
      return HTML_NAMED_ENTITIES.get(named.toLowerCase()) ?? raw
    },
  )
}

// CSS escapes are decoded before a CSS url() is fetched.
function decodeCssEscapes(value) {
  return String(value).replace(
    /\\(?:([0-9a-f]{1,6})(?:\r\n|[\n\r\f]|[ \t])?|(?:\r\n|[\n\r\f])|([\s\S]))/gi,
    (raw, hex, character) => {
      if (!hex) return character ?? ''
      const codePoint = Number.parseInt(hex, 16)
      if (codePoint === 0 || codePoint > 0x10ffff || (codePoint >= 0xd800 && codePoint <= 0xdfff)) {
        return '\uFFFD'
      }
      return String.fromCodePoint(codePoint)
    },
  )
}

// A CSS identifier character, or a CSS escape standing in for one. The function name is escapable
// too — `u\72l(…)` is the url() function to a CSS parser — so the identifier must be decoded before
// it can be recognized, not matched literally.
const CSS_IDENT_RUN = String.raw`(?:\\[0-9a-f]{1,6}(?:\r\n|[ \n\r\t\f])?|\\[\s\S]|[a-z0-9_-])+`

// Decode CSS escapes only in CSS URL tokens. Decoding the whole Markdown document would make
// ordinary backslash-significant prose such as `C:\\temp` compare equal to `C:temp`.
//
// The leading identifier is captured as an escape-bearing run and decoded before the `url`
// comparison, so an escaped function name cannot hide a signed upload from the scan below. Matching
// the WHOLE identifier run also keeps `my-url(…)` out: it decodes to `my-url`, not `url`.
function decodeCssUrlEscapes(value) {
  return String(value).replace(
    new RegExp(
      `(${CSS_IDENT_RUN})\\(\\s*(?:"((?:\\\\[\\s\\S]|[^"\\\\])*)"|'((?:\\\\[\\s\\S]|[^'\\\\])*)'|((?:\\\\[\\s\\S]|[^)])*))\\s*\\)`,
      'gi',
    ),
    (raw, ident, doubleQuoted, singleQuoted, unquoted) => {
      if (decodeCssEscapes(ident).toLowerCase() !== 'url') return raw
      const url = doubleQuoted ?? singleQuoted ?? unquoted
      if (!url) return raw
      // Replace inside the destination only. Splitting at the opening paren keeps a decoded
      // identifier from being rewritten when it happens to repeat the destination's text.
      const open = raw.indexOf('(')
      return raw.slice(0, open + 1) + raw.slice(open + 1).replace(url, decodeCssEscapes(url))
    },
  )
}

function advanceMarkdownColumn(column, character) {
  return character === '\t' ? column + 4 - (column % 4) : column + 1
}

function markdownColumns(value) {
  let columns = 0
  for (const character of String(value)) {
    columns = advanceMarkdownColumn(columns, character)
  }
  return columns
}

function stripIndentColumns(value, requiredColumns) {
  const text = String(value)
  let columns = 0
  let index = 0
  while (index < text.length && (text[index] === ' ' || text[index] === '\t')) {
    columns = advanceMarkdownColumn(columns, text[index])
    index += 1
    if (columns >= requiredColumns) return text.slice(index)
  }
  return null
}

function containerLines(markdown) {
  const lines = String(markdown ?? '').match(/.*(?:\r?\n|$)/g) ?? []
  const normalized = []
  let offset = 0
  let listIndent = null

  for (const raw of lines) {
    let content = raw
    const prefix = []
    while (true) {
      const quote = /^(?: {0,3})>[\t ]?/.exec(content)
      if (!quote) break
      prefix.push('quote')
      content = content.slice(quote[0].length)
    }
    const list = /^(?: {0,3})(?:[-+*]|\d{1,9}[.)])[\t ]+/.exec(content)
    if (list) {
      listIndent = markdownColumns(list[0])
      prefix.push(`list:${listIndent}`)
      content = content.slice(list[0].length)
    } else {
      const continuation = listIndent === null ? null : stripIndentColumns(content, listIndent)
      if (continuation !== null) {
        prefix.push(`list:${listIndent}`)
        content = continuation
      } else if (!/^[\t ]*(?:\r?\n)?$/.test(content)) {
        listIndent = null
      }
    }
    normalized.push({
      content,
      contentOffset: offset + raw.indexOf(content),
      offset,
      raw,
      prefix: prefix.join('/'),
    })
    offset += raw.length
  }
  return normalized
}

function fencedCodeRanges(markdown) {
  const lines = containerLines(markdown)
  const ranges = []
  let fence = null

  for (const { content: line, offset, raw, prefix } of lines) {
    const opening = /^(?: {0,3})(`{3,}|~{3,})([^\r\n]*)/.exec(line)
    if (!fence && opening && !(opening[1][0] === '`' && opening[2].includes('`'))) {
      fence = { character: opening[1][0], length: opening[1].length, start: offset, prefix }
    } else if (fence) {
      const closing = new RegExp(
        `^(?: {0,3})${fence.character}{${fence.length},}[\\t ]*(?:\\r?\\n)?$`,
      )
      if (fence.prefix === prefix && closing.test(line)) {
        ranges.push({ start: fence.start, end: offset + raw.length })
        fence = null
      }
    }
  }
  if (fence) ranges.push({ start: fence.start, end: String(markdown ?? '').length })
  return ranges
}

// CommonMark type-1 HTML blocks continue through their matching closing tag, including blank
// lines. Type-6 blocks start with one of these block-level tags and continue through the next
// blank line. Markdown reference definitions inside either are inert HTML text, not links.
const HTML_BLOCKS_UNTIL_CLOSING_TAG = new Set(['pre', 'script', 'style'])
const HTML_BLOCK_TAGS = new Set(
  'address article aside base basefont blockquote body caption center col colgroup dd details dialog dir div dl dt fieldset figcaption figure footer form h1 h2 h3 h4 h5 h6 head header hr html iframe legend li link main menu menuitem nav ol p plaintext pre script search section style summary table tbody td tfoot th thead title tr track ul'.split(
    ' ',
  ),
)
// CommonMark type-7 blocks begin with a complete open or closing HTML tag that
// is not handled by types 1 or 6 (for example a custom element). They run to
// the next blank line, so Markdown-looking definitions within are inert.
const HTML_TYPE_SEVEN_TAG =
  /^(?: {0,3})<\/?[a-z][a-z0-9-]*(?:\s+[^\s"'=<>`]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*\s*\/?>[\t ]*(?:\r?\n)?$/i

function rawHtmlBlockRanges(markdown) {
  const text = String(markdown ?? '')
  const lines = containerLines(text)
  const ranges = []
  let start = null
  let closingTag = null
  let commentStart = null
  let processingInstructionStart = null
  let declarationStart = null
  let cdataStart = null

  for (const { content: line, offset, raw } of lines) {
    if (commentStart !== null) {
      if (raw.includes('-->')) {
        ranges.push({ start: commentStart, end: offset + raw.length })
        commentStart = null
      }
      continue
    }
    if (processingInstructionStart !== null) {
      if (raw.includes('?>')) {
        ranges.push({ start: processingInstructionStart, end: offset + raw.length })
        processingInstructionStart = null
      }
      continue
    }
    if (declarationStart !== null) {
      if (raw.includes('>')) {
        ranges.push({ start: declarationStart, end: offset + raw.length })
        declarationStart = null
      }
      continue
    }
    if (cdataStart !== null) {
      if (raw.includes(']]>')) {
        ranges.push({ start: cdataStart, end: offset + raw.length })
        cdataStart = null
      }
      continue
    }
    if (/^(?: {0,3})<!--/.test(line)) {
      if (line.includes('-->')) {
        ranges.push({ start: offset, end: offset + raw.length })
      } else {
        commentStart = offset
      }
      continue
    }
    if (/^(?: {0,3})<\?/.test(line)) {
      if (line.includes('?>')) ranges.push({ start: offset, end: offset + raw.length })
      else processingInstructionStart = offset
      continue
    }
    if (/^(?: {0,3})<!\[CDATA\[/.test(line)) {
      if (line.includes(']]>')) ranges.push({ start: offset, end: offset + raw.length })
      else cdataStart = offset
      continue
    }
    if (/^(?: {0,3})<![A-Z]/.test(line)) {
      if (line.includes('>')) ranges.push({ start: offset, end: offset + raw.length })
      else declarationStart = offset
      continue
    }
    if (start === null) {
      const opening = /^(?: {0,3})<([a-z][a-z0-9-]*)(?:\s|\/?>|$)/i.exec(line)
      if (opening && HTML_BLOCK_TAGS.has(opening[1].toLowerCase())) {
        start = offset
        const tag = opening[1].toLowerCase()
        if (HTML_BLOCKS_UNTIL_CLOSING_TAG.has(tag)) closingTag = tag
      }
      if (start === null && HTML_TYPE_SEVEN_TAG.test(line)) start = offset
    }
    if (start !== null) {
      if (closingTag && new RegExp(`</${closingTag}\\s*>`, 'i').test(line)) {
        ranges.push({ start, end: offset + raw.length })
        start = null
        closingTag = null
      } else if (!closingTag && /^[\t ]*(?:\r?\n)?$/.test(line)) {
        ranges.push({ start, end: offset + raw.length })
        start = null
      }
    }
  }
  if (commentStart !== null) ranges.push({ start: commentStart, end: text.length })
  if (processingInstructionStart !== null)
    ranges.push({ start: processingInstructionStart, end: text.length })
  if (declarationStart !== null) ranges.push({ start: declarationStart, end: text.length })
  if (cdataStart !== null) ranges.push({ start: cdataStart, end: text.length })
  if (start !== null) ranges.push({ start, end: text.length })
  return ranges
}

function isInRanges(index, ranges) {
  return ranges.some((range) => index >= range.start && index < range.end)
}

function htmlTagRanges(markdown) {
  const text = String(markdown ?? '')
  const ranges = []
  const tagStart = /<\/?[a-z][a-z0-9-]*\b/gi
  for (const match of text.matchAll(tagStart)) {
    let quote = null
    for (let index = match.index + match[0].length; index < text.length; index += 1) {
      const character = text[index]
      if (quote !== null) {
        if (character === quote) quote = null
      } else if (character === '"' || character === "'") {
        quote = character
      } else if (character === '>') {
        ranges.push({ start: match.index, end: index + 1 })
        break
      }
    }
  }
  return ranges
}

function cleanInlineDestination(raw) {
  const s = String(raw).trim()
  const angle = /^<(.*)>$/.exec(s)
  if (angle) return decodeHtmlEntities(unescapeMarkdownPunctuation(angle[1].trim()))
  // Strip an optional markdown title: the destination is everything up to the first whitespace.
  return decodeHtmlEntities(unescapeMarkdownPunctuation(s.split(/\s+/)[0]))
}

function unescapeMarkdownPunctuation(value) {
  return String(value).replace(/\\([!"#$%&'()*+,\-./:;<=>?@[\\\]^_`{|}~])/g, '$1')
}

function indexOfUnescaped(markdown, character, start) {
  for (let i = start; i < markdown.length; i += 1) {
    if (markdown[i] === '\\' && i + 1 < markdown.length) {
      i += 1
      continue
    }
    if (markdown[i] === character) return i
  }
  return -1
}

function imageLabelEnd(markdown, start) {
  let depth = 0
  for (let i = start + 1; i < markdown.length; i += 1) {
    if (markdown[i] === '\\' && i + 1 < markdown.length) {
      i += 1
    } else if (markdown[i] === '[') {
      depth += 1
    } else if (markdown[i] === ']') {
      if (depth === 0) return i
      depth -= 1
    }
  }
  return -1
}

function referenceLabel(label) {
  return String(label).replace(/\\(.)/g, '$1').trim().replace(/\s+/g, ' ').toLowerCase()
}

function normalizeUrl(url) {
  return String(url).trim()
}

function trimBareLinearUrl(raw) {
  let url = normalizeUrl(raw).replace(TRAILING_PUNCT, '')
  // Strip only surplus closing parentheses that wrap a prose URL. Balanced parentheses may be
  // part of an asset pathname and must remain available to credential checks.
  while (url.endsWith(')')) {
    const opens = (url.match(/\(/g) ?? []).length
    const closes = (url.match(/\)/g) ?? []).length
    if (closes <= opens) break
    url = url.slice(0, -1)
  }
  return url
}

function srcsetDestinations(srcset) {
  return String(srcset)
    .split(',')
    .map((candidate) => candidate.trim().split(/\s+/, 1)[0])
    .filter(Boolean)
}

function hasOptionalInlineTitleBeforeClose(markdown, index) {
  let start = index
  while (/\s/.test(markdown[start] ?? '')) start += 1
  if (markdown[start] === ')') return true

  const delimiter = markdown[start]
  if (delimiter !== '"' && delimiter !== "'" && delimiter !== '(') return false

  const closingDelimiter = delimiter === '(' ? ')' : delimiter
  let depth = 0
  for (let i = start + 1; i < markdown.length; i += 1) {
    const char = markdown[i]
    if (char === '\\' && i + 1 < markdown.length) {
      i += 1
      continue
    }
    if (delimiter === '(' && char === '(') {
      depth += 1
      continue
    }
    if (char !== closingDelimiter) continue
    if (depth > 0) {
      depth -= 1
      continue
    }
    let close = i + 1
    while (/\s/.test(markdown[close] ?? '')) close += 1
    return markdown[close] === ')'
  }
  return false
}

function inlineDestinations(markdown, startPattern, labelOffset) {
  const md = String(markdown ?? '')
  const hits = []
  for (const match of md.matchAll(startPattern)) {
    // Scan the label so an escaped closing bracket remains label content instead of preventing
    // discovery of the image destination. Nested brackets are supported for the same reason.
    const labelStart = match.index + labelOffset
    const labelEnd = imageLabelEnd(md, labelStart)
    if (labelEnd < 0 || md[labelEnd + 1] !== '(') continue
    let start = labelEnd + 2
    while (/\s/.test(md[start] ?? '')) start += 1
    if (md[start] === '<') {
      const close = indexOfUnescaped(md, '>', start + 1)
      if (close >= 0 && hasOptionalInlineTitleBeforeClose(md, close + 1)) {
        hits.push({
          index: match.index,
          start: start + 1,
          end: close,
          value: md.slice(start + 1, close),
        })
      }
      continue
    }
    let depth = 0
    for (let i = start; i < md.length; i += 1) {
      const char = md[i]
      // Markdown escapes punctuation in a destination. In particular, an escaped closing
      // parenthesis belongs to the URL rather than closing the image wrapper.
      if (char === '\\' && (md[i + 1] === '(' || md[i + 1] === ')')) {
        i += 1
      } else if (char === '(') {
        depth += 1
      } else if (char === ')') {
        if (depth === 0) {
          hits.push({ index: match.index, start, end: i, value: md.slice(start, i) })
          break
        }
        depth -= 1
      } else if (/\s/.test(char) && depth === 0) {
        const destination = md.slice(start, i)
        const close = md.indexOf(')', i)
        if (destination && close >= 0)
          hits.push({ index: match.index, start, end: i, value: destination })
        break
      }
    }
  }
  return hits
}

function inlineImageDestinations(markdown) {
  return inlineDestinations(markdown, INLINE_IMAGE_START, 1)
}

function inlineLinkDestinations(markdown) {
  return inlineDestinations(markdown, INLINE_LINK_START, 0)
}

function referenceDefinitionEntries(markdown) {
  const md = String(markdown ?? '')
  const definitions = []
  const fencedRanges = fencedCodeRanges(md)
  const htmlBlockRanges = rawHtmlBlockRanges(md)
  const definitionStarts = new Set()
  const add = (match, baseOffset) => {
    const label = referenceLabel(match[1])
    const raw = match[2] ?? match[3]
    const start = baseOffset + match[0].indexOf(raw)
    if (
      definitionStarts.has(start) ||
      isInRanges(start, fencedRanges) ||
      isInRanges(start, htmlBlockRanges)
    ) {
      return
    }
    definitionStarts.add(start)
    definitions.push({ label, start, end: start + raw.length, value: cleanInlineDestination(raw) })
  }
  for (const match of md.matchAll(REFERENCE_DEFINITION)) {
    if (isInRanges(match.index, fencedRanges) || isInRanges(match.index, htmlBlockRanges)) continue
    add(match, match.index)
  }
  // A list item's continuation is parsed after its marker indentation is removed. In particular,
  // an item whose marker plus padding is four or more columns can contain a definition that is
  // four-or-more spaces indented in source while still being active Markdown.
  const lines = containerLines(md)
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]
    if (!line.prefix.includes('list:')) continue
    const next = lines[index + 1]
    const candidate =
      next && next.prefix === line.prefix ? `${line.content}${next.content}` : line.content
    const match =
      /^(?: {0,3})\[((?:\\.|[^\]\\\r\n])+)\]:[\t ]*(?:\r?\n[\t ]*)?(?:<((?:\\.|[^>\r\n])*)>|([^\s]+))/.exec(
        candidate,
      )
    if (!match) continue
    const raw = match[2] ?? match[3]
    const rawOffset = match[0].indexOf(raw)
    const baseOffset =
      next && rawOffset >= line.content.length
        ? next.contentOffset - line.content.length
        : line.contentOffset
    add(match, baseOffset)
  }
  return definitions
}

function referenceDefinitions(markdown) {
  const definitions = new Map()
  for (const definition of referenceDefinitionEntries(markdown)) {
    // CommonMark renders the first active definition for a label. Image-parity
    // extraction follows that resolution, while the credential gate below scans
    // every active entry so a duplicate cannot hide a secret-bearing URL.
    if (!definitions.has(definition.label)) definitions.set(definition.label, definition)
  }
  return definitions
}

function referenceImageDestinations(markdown) {
  const md = String(markdown ?? '')
  const definitions = referenceDefinitions(md)

  const hits = []
  const seen = new Set()
  for (const match of md.matchAll(INLINE_IMAGE_START)) {
    const labelStart = match.index + 1
    const labelEnd = imageLabelEnd(md, labelStart)
    if (labelEnd < 0 || md[labelEnd + 1] === '(') continue

    const referenceStart = labelEnd + 1
    let label = md.slice(labelStart + 1, labelEnd)
    if (md[referenceStart] === '[') {
      const referenceEnd = indexOfUnescaped(md, ']', referenceStart + 1)
      if (referenceEnd < 0) continue
      label = md.slice(referenceStart + 1, referenceEnd) || label
    }
    const definition = definitions.get(referenceLabel(label))
    if (!definition || seen.has(definition.start)) continue
    seen.add(definition.start)
    hits.push({ index: match.index, ...definition })
  }
  return hits
}

function isCredentialQueryKey(key) {
  const compact = String(key)
    .toLowerCase()
    .replace(/[^a-z0-9]/g, '')
  return (
    compact === 'sig' ||
    compact === 'auth' ||
    compact === 'key' ||
    compact.includes('signature') ||
    compact.includes('token') ||
    compact === 'jwt' ||
    compact.includes('session') ||
    compact.includes('bearer') ||
    compact.endsWith('idtoken') ||
    compact.includes('credential') ||
    compact.includes('password') ||
    compact.includes('secret') ||
    compact.includes('authorization') ||
    compact.endsWith('apikey') ||
    compact.endsWith('accesskey') ||
    compact.endsWith('accesskeyid') ||
    compact.endsWith('keyid')
  )
}

function redactedExternalIdentity(parsed, { redactUserinfo = false } = {}) {
  const parameters = [...parsed.searchParams]
  const fragmentParameters = [...new URLSearchParams(parsed.hash.slice(1))]
  const hasCredentialQuery = parameters.some(([key]) => isCredentialQueryKey(key))
  const hasCredentialFragment = fragmentParameters.some(([key]) => isCredentialQueryKey(key))
  const hasUserinfo = parsed.username !== '' || parsed.password !== ''
  const redactedUserinfo =
    parsed.username !== '' && isExplicitlyRedactedCredentialValue(parsed.password)
  if (!hasCredentialQuery && !hasCredentialFragment && !hasUserinfo) return null
  // Safe-source files may use `username:REDACTED@host` to retain a URL shape
  // without preserving its password. Only that documented spelling may match
  // a live userinfo value from the original source.
  if (hasUserinfo && !redactUserinfo && !redactedUserinfo) return null

  // The secret gate must redact credential query values before a plan is published. Keep every
  // non-credential parameter as part of the image identity, but make credential values compare
  // equal whether they are still signed or have been replaced by a redaction marker.
  if (hasCredentialQuery) {
    parsed.search = ''
    for (const [key, value] of parameters) {
      parsed.searchParams.append(key, isCredentialQueryKey(key) ? '__redacted__' : value)
    }
  }
  if (hasCredentialFragment) {
    parsed.hash = ''
    for (const [key, value] of fragmentParameters) {
      parsed.hash += `${parsed.hash ? '&' : ''}${encodeURIComponent(key)}=${encodeURIComponent(
        isCredentialQueryKey(key) ? '__redacted__' : value,
      )}`
    }
  }
  if (hasUserinfo) parsed.password = '__redacted__'
  return parsed.toString()
}

function isExplicitlyRedactedCredentialValue(value) {
  const normalized = String(value).trim()
  return (
    normalized.toLowerCase() === 'redacted' || /^\[redacted(?::\s*[^\]]+)?\]$/i.test(normalized)
  )
}

function parseUrl(url) {
  const raw = String(url).trim()
  // Network-path references are valid Markdown destinations. Give those URLs a scheme solely
  // for inspection, so their host and query cannot bypass the credential gate.
  return raw.startsWith('//') ? new URL(raw, 'https://network-path.invalid') : new URL(raw)
}

function normalizeHtmlAttributeUrl(value) {
  // The URL parser removes ASCII tabs and newlines from attribute values before
  // parsing. Match that behavior before cleanInlineDestination applies its
  // Markdown-title whitespace rule, so controls cannot hide a credential query.
  return String(value).replace(/[\t\n\r]/g, '')
}

function credentialCandidateUrls(markdown) {
  const text = String(markdown ?? '')
  // Renderers decode entities in prose before treating a bare URL as a link.
  // Scan that view too, otherwise `https&colon;//…?token&equals;secret` evades
  // the credential gate even though it renders as a credential-bearing URL.
  const decodedText = decodeCssUrlEscapes(decodeHtmlEntities(text))
  const nonBareUrlRanges = [
    ...inlineImageDestinations(decodedText),
    ...inlineLinkDestinations(decodedText),
    ...referenceDefinitionEntries(decodedText),
  ]
  const urls = extractOrdered(text)
  const seen = new Set(urls)
  const add = (value) => {
    const url = cleanInlineDestination(value)
    if (url && !seen.has(url)) {
      seen.add(url)
      urls.push(url)
    }
  }
  for (const destination of inlineLinkDestinations(text)) {
    add(destination.value)
  }
  for (const definition of referenceDefinitionEntries(text)) {
    add(definition.value)
  }
  for (const match of text.matchAll(HTML_LINK_HREF)) {
    add(normalizeHtmlAttributeUrl(match[1] ?? match[2] ?? match[3]))
  }
  for (const match of text.matchAll(HTML_SVG_IMAGE_HREF)) {
    add(normalizeHtmlAttributeUrl(match[1] ?? match[2] ?? match[3]))
  }
  for (const match of text.matchAll(HTML_INPUT_IMAGE_SRC)) {
    add(normalizeHtmlAttributeUrl(match[1] ?? match[2] ?? match[3]))
  }
  for (const match of text.matchAll(HTML_VIDEO_POSTER)) {
    add(normalizeHtmlAttributeUrl(match[1] ?? match[2] ?? match[3]))
  }
  for (const match of decodedText.matchAll(BARE_EXTERNAL_URL)) {
    // Markdown destinations have dedicated structural parsing above. This decoded prose scan
    // must not reclassify them as bare URLs, but credentials in any input attribute still need
    // inspection before the plan is published.
    if (isInRanges(match.index, nonBareUrlRanges)) continue
    const url = normalizeUrl(normalizeHtmlAttributeUrl(match[0])).replace(TRAILING_PUNCT, '')
    if (url && !seen.has(url)) {
      seen.add(url)
      urls.push(url)
    }
  }
  return urls
}

function unredactedExternalCredentialCount(markdown) {
  return credentialCandidateUrls(markdown).filter((url) => {
    try {
      const parsed = parseUrl(url)
      const hasUserinfo = parsed.username !== '' || parsed.password !== ''
      const redactedUserinfo =
        parsed.username !== '' && isExplicitlyRedactedCredentialValue(parsed.password)
      return (
        (hasUserinfo && !redactedUserinfo) ||
        (parsed.hostname.toLowerCase() !== 'uploads.linear.app' &&
          [...parsed.searchParams].some(
            ([key, value]) =>
              isCredentialQueryKey(key) && !isExplicitlyRedactedCredentialValue(value),
          )) ||
        [...new URLSearchParams(parsed.hash.slice(1))].some(
          ([key, value]) =>
            isCredentialQueryKey(key) && !isExplicitlyRedactedCredentialValue(value),
        )
      )
    } catch {
      return false
    }
  }).length
}

// uploadIdentity(url) -> string: stable identity for Linear uploads; credential-redacted identity
// for external URLs with sensitive query keys; trimmed raw URL otherwise. Invalid URLs deliberately
// stay raw so malformed references can still be reported as dropped.
export function uploadIdentity(url, { redactUserinfo = false } = {}) {
  const raw = String(url).trim()
  try {
    const parsed = parseUrl(raw)
    if (parsed.hostname.toLowerCase() === 'uploads.linear.app') {
      return `${parsed.origin}${parsed.pathname}`
    }
    return redactedExternalIdentity(parsed, { redactUserinfo }) ?? raw
  } catch {
    return raw
  }
}

// Ordered, de-duplicated list of every image URL in document order.
function extractOrdered(markdown) {
  const md = String(markdown ?? '')
  const hits = []
  const inlineHits = inlineImageDestinations(md)
  const referenceHits = referenceImageDestinations(md)
  const push = (index, value, trimTrailingPunctuation = false) => {
    const url = normalizeUrl(value).replace(trimTrailingPunctuation ? TRAILING_PUNCT : /$^/, '')
    if (url) hits.push({ index, url })
  }
  for (const m of inlineHits) push(m.index, cleanInlineDestination(m.value))
  for (const m of referenceHits) push(m.index, m.value)
  for (const m of md.matchAll(HTML_IMG_SRC)) {
    push(m.index, decodeHtmlEntities(m[1] ?? m[2] ?? m[3]))
  }
  for (const m of md.matchAll(HTML_SVG_IMAGE_HREF)) {
    push(m.index, decodeHtmlEntities(m[1] ?? m[2] ?? m[3]))
  }
  for (const m of md.matchAll(HTML_RESPONSIVE_SRCSET)) {
    for (const url of srcsetDestinations(decodeHtmlEntities(m[1] ?? m[2] ?? m[3])))
      push(m.index, url)
  }
  for (const m of md.matchAll(BARE_LINEAR_URL)) {
    if (!inlineHits.some((inline) => m.index >= inline.start && m.index < inline.end)) {
      push(m.index, trimBareLinearUrl(m[0]))
    }
  }
  hits.sort((a, b) => a.index - b.index)
  const seen = new Set()
  const ordered = []
  for (const { url } of hits) {
    if (seen.has(url)) continue
    seen.add(url)
    ordered.push(url)
  }
  return ordered
}

// extractImageRefs(markdown) -> Set<string>: every image URL, normalized + de-duplicated.
export function extractImageRefs(markdown) {
  return new Set(extractOrdered(markdown))
}

// findDroppedImages(originalMarkdown, rewrittenMarkdown) -> string[]: URLs present in the original
// but absent from the rewritten (order-stable by original document order, de-duplicated). Direction
// matters — extra images added by the rewrite are never reported.
export function findDroppedImages(originalMarkdown, rewrittenMarkdown) {
  const rewritten = new Set(extractOrdered(rewrittenMarkdown).map(uploadIdentity))
  return extractOrdered(originalMarkdown).filter((url) => !rewritten.has(uploadIdentity(url)))
}

export function parseImageGuardArgs(argv) {
  const args = {
    original: null,
    rewritten: null,
    allowEmptyOriginal: false,
    expectImages: null,
    requireVerbatim: false,
    requireSafeSource: false,
    requireUnsignedUploads: false,
  }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    if (flag === '--original') {
      args.original = argv[(i += 1)]
    } else if (flag === '--rewritten') {
      args.rewritten = argv[(i += 1)]
    } else if (flag === '--allow-empty-original') {
      args.allowEmptyOriginal = true
    } else if (flag === '--expect-images') {
      const value = argv[(i += 1)]
      if (!/^\d+$/.test(value ?? '') || !Number.isSafeInteger(Number(value))) {
        throw new Error('--expect-images <N> must be a non-negative integer')
      }
      args.expectImages = Number(value)
    } else if (flag === '--require-verbatim') {
      args.requireVerbatim = true
    } else if (flag === '--require-safe-source') {
      args.requireSafeSource = true
    } else if (flag === '--require-unsigned-uploads') {
      args.requireUnsignedUploads = true
    }
  }
  if (!args.original) {
    throw new Error('--original <path> is required')
  }
  if (!args.rewritten) {
    throw new Error('--rewritten <path> is required')
  }
  return args
}

function canonicalizeImageUrls(
  markdown,
  { redactExternalCredentials = true, redactUserinfo = false } = {},
) {
  const canonicalize = (url) => {
    if (!redactExternalCredentials && !isLinearUpload(url)) return url
    return uploadIdentity(url, { redactUserinfo })
  }
  let canonicalizedInline = String(markdown ?? '')
  for (const inline of inlineImageDestinations(canonicalizedInline).reverse()) {
    const url = cleanInlineDestination(inline.value)
    canonicalizedInline =
      canonicalizedInline.slice(0, inline.start) +
      canonicalize(url) +
      canonicalizedInline.slice(inline.end)
  }
  for (const inline of inlineLinkDestinations(canonicalizedInline).reverse()) {
    const url = cleanInlineDestination(inline.value)
    canonicalizedInline =
      canonicalizedInline.slice(0, inline.start) +
      canonicalize(url) +
      canonicalizedInline.slice(inline.end)
  }
  for (const reference of referenceDefinitionEntries(canonicalizedInline).sort(
    (a, b) => b.start - a.start,
  )) {
    canonicalizedInline =
      canonicalizedInline.slice(0, reference.start) +
      canonicalize(reference.value) +
      canonicalizedInline.slice(reference.end)
  }
  const canonicalizeHtmlUrlAttribute = (markdown, matcher) =>
    markdown.replace(matcher, (rawElement, double, single, unquoted) => {
      const url = double ?? single ?? unquoted
      return rawElement.replace(url, canonicalize(decodeHtmlEntities(url)))
    })
  const canonicalizedHtml = canonicalizeHtmlUrlAttribute(canonicalizedInline, HTML_IMG_SRC)
  const canonicalizedInputImages = canonicalizeHtmlUrlAttribute(
    canonicalizedHtml,
    HTML_INPUT_IMAGE_SRC,
  )
  const canonicalizedVideoPosters = canonicalizeHtmlUrlAttribute(
    canonicalizedInputImages,
    HTML_VIDEO_POSTER,
  )
  const canonicalizedLinks = canonicalizeHtmlUrlAttribute(canonicalizedVideoPosters, HTML_LINK_HREF)
  const canonicalizedSvgImages = canonicalizeHtmlUrlAttribute(
    canonicalizedLinks,
    HTML_SVG_IMAGE_HREF,
  )
  const canonicalizedSrcset = canonicalizedSvgImages.replace(
    HTML_RESPONSIVE_SRCSET,
    (rawImage, double, single, unquoted) => {
      const srcset = double ?? single ?? unquoted
      const canonical = decodeHtmlEntities(srcset).replace(
        /(^|,)([\t ]*)([^\s,]+)/g,
        (_, prefix, whitespace, url) => `${prefix}${whitespace}${canonicalize(url)}`,
      )
      return rawImage.replace(srcset, canonical)
    },
  )
  return decodeCssUrlEscapes(canonicalizedSrcset).replace(BARE_EXTERNAL_URL, (rawUrl) => {
    const decodedUrl = normalizeHtmlAttributeUrl(decodeHtmlEntities(rawUrl))
    const url = trimBareLinearUrl(decodedUrl)
    return `${canonicalize(url)}${decodedUrl.slice(url.length)}`
  })
}

function originalNotesBodies(markdown) {
  // Original notes is the terminal plan section. Its verbatim payload may itself contain H2
  // headings, so splitting at the next `##` would reject an otherwise exact copy. Locate headings
  // outside fenced code: a plan may document this template inside a fenced example.
  const text = String(markdown ?? '')
  const lines = text.match(/.*(?:\r?\n|$)/g) ?? []
  let offset = 0
  let fence = null
  const headingEnds = []
  for (const line of lines) {
    const bare = line.replace(/\r?\n$/, '')
    const opening = /^(?: {0,3})(`{3,}|~{3,})(.*)$/.exec(bare)
    if (fence === null && opening && !(opening[1][0] === '`' && opening[2].includes('`'))) {
      const run = opening[1]
      fence = { char: run[0], length: run.length }
    } else if (fence !== null) {
      // CommonMark permits an info string only on an opening fence. A matching marker with one
      // inside an open fence is content, not a close, so wait for a whitespace-only suffix.
      const closing = /^(?: {0,3})(`{3,}|~{3,})[ \t]*$/.exec(bare)
      if (closing) {
        const run = closing[1]
        if (run[0] === fence.char && run.length >= fence.length) {
          fence = null
        }
      }
    } else if (fence === null && /^## Original notes[ \t]*$/.test(bare)) {
      headingEnds.push(offset + line.length)
    }
    offset += line.length
  }
  return headingEnds.map((headingEnd) => {
    const body = text.slice(headingEnd)
    // Consume only the template's one blank heading/content separator; remaining payload is exact.
    return body.replace(/^(?:\r?\n)?/, '')
  })
}

function originalNotesBody(markdown, originalText) {
  const candidates = originalNotesBodies(markdown)
  if (candidates.length === 0) return null
  const original = canonicalizeImageUrls(originalText, { redactExternalCredentials: false })
  // The copied source can itself contain an unfenced `## Original notes` heading. Select the
  // wrapper candidate whose complete payload matches the canonicalized source, not a nested one.
  return (
    candidates.find(
      (candidate) =>
        canonicalizeImageUrls(candidate, { redactExternalCredentials: false }) === original,
    ) ?? candidates[0]
  )
}

function verifyVerbatimOriginalNotes(originalText, rewrittenText) {
  const notes = originalNotesBody(rewrittenText, originalText)
  if (notes === null) {
    return 'plan-image-guard: ## Original notes section is missing from the rewritten description'
  }
  const unsafeCredentials = unredactedExternalCredentialCount(rewrittenText)
  if (unsafeCredentials > 0) {
    return `plan-image-guard: rewritten description contains ${unsafeCredentials} external image URL(s) with unredacted credential query values`
  }
  // The safe source's credential redaction is itself meaningful. Only raw image parity erases
  // credential values; Original notes must keep redacted references verbatim.
  const original = canonicalizeImageUrls(originalText, { redactExternalCredentials: false })
  const rewritten = canonicalizeImageUrls(notes, { redactExternalCredentials: false })
  if (original === rewritten) return null

  const rewrittenLines = rewritten.split('\n')
  const originalLines = original.split('\n')
  const lineCount = (text) => {
    if (text.length === 0) return 0
    const lines = text.split('\n').length
    return text.endsWith('\n') ? lines - 1 : lines
  }
  const originalLineCount = lineCount(original)
  const rewrittenLineCount = lineCount(rewritten)
  const maxLines = Math.max(originalLines.length, rewrittenLines.length)
  let differingIndex = 0
  for (; differingIndex < maxLines; differingIndex += 1) {
    if (originalLines[differingIndex] !== rewrittenLines[differingIndex]) break
  }

  const strippedOriginal = original.replace(/\n+$/, '')
  const strippedRewritten = rewritten.replace(/\n+$/, '')
  if (strippedOriginal === strippedRewritten) {
    const line = Math.max(1, Math.min(originalLineCount || 1, rewrittenLineCount || 1))
    return `plan-image-guard: ## Original notes trailing newline differs at line ${line} (original lines: ${originalLineCount}, rewritten lines: ${rewrittenLineCount})`
  }

  const originalLine = originalLines[differingIndex] ?? ''
  const rewrittenLine = rewrittenLines[differingIndex] ?? ''
  let kind = 'content'
  if (originalLine.trim() === rewrittenLine.trim()) kind = 'whitespace-only'
  const maxColumns = Math.max(originalLine.length, rewrittenLine.length)
  let column = 1
  for (; column <= maxColumns; column += 1) {
    if (originalLine.charAt(column - 1) !== rewrittenLine.charAt(column - 1)) break
  }
  return `plan-image-guard: ## Original notes ${kind} difference at line ${differingIndex + 1}, column ${column} (original lines: ${originalLineCount}, rewritten lines: ${rewrittenLineCount})`
}

function escapeRegex(value) {
  return String(value).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function verifySafeSource(originalText, safeText) {
  // Canonical image identity permits the two mandatory URL transformations: stripping Linear
  // upload signatures and replacing external credential values with redactions. Any other
  // omitted raw text must be represented by an explicit redaction marker in the safe source.
  const original = canonicalizeImageUrls(originalText, { redactUserinfo: true })
  const safe = canonicalizeImageUrls(safeText)
  if (original === safe) return null

  let cursor = 0
  let pattern = '^'
  for (const marker of safe.matchAll(REDACTION_MARKER)) {
    pattern += escapeRegex(safe.slice(cursor, marker.index))
    pattern += '[\\s\\S]*'
    cursor = marker.index + marker[0].length
  }
  pattern += escapeRegex(safe.slice(cursor))
  pattern += '$'
  if (new RegExp(pattern).test(original)) return null

  return 'plan-image-guard: safe source drops or alters unredacted original notes'
}

function isLinearUpload(url) {
  try {
    return parseUrl(url).hostname.toLowerCase() === 'uploads.linear.app'
  } catch {
    return false
  }
}

function distinctUploadIdentities(markdown) {
  return new Set(extractOrdered(markdown).filter(isLinearUpload).map(uploadIdentity))
}

function signedUploadCount(markdown) {
  return credentialCandidateUrls(markdown).filter((url) => {
    try {
      const parsed = parseUrl(url)
      return isLinearUpload(url) && parsed.search !== ''
    } catch {
      return false
    }
  }).length
}

function main() {
  const {
    original,
    rewritten,
    allowEmptyOriginal,
    expectImages,
    requireVerbatim,
    requireSafeSource,
    requireUnsignedUploads,
  } = parseImageGuardArgs(process.argv.slice(2))
  const originalText = readFileSync(original, 'utf8')
  const rewrittenText = readFileSync(rewritten, 'utf8')

  // Refuse to CERTIFY a comparison we could not meaningfully perform. `findDroppedImages` is right
  // to return [] here — empty-in/empty-out is correct set-difference semantics, and it stays that
  // way — but the CLI is the gate, and a gate that blesses an unverifiable input is not a gate.
  // An empty original is ambiguous between "the upstream extraction broke, so the reporter's images
  // are invisible to us" and "the ticket genuinely has no description"; we see only files, so we
  // cannot tell them apart. Fail closed on the ambiguity and make the benign case an explicit,
  // auditable opt-out — a flag in the transcript — rather than a silent default. This is deliberate:
  // whatever breaks the caller's extraction usually empties BOTH scratch files at once, which is
  // exactly the input the old vacuous pass was most confident about.
  if (!allowEmptyOriginal && originalText.trim() === '') {
    console.error(
      'plan-image-guard: original is empty — cannot verify image parity; ' +
        'pass --allow-empty-original only if the source description is genuinely empty',
    )
    process.exitCode = 1
    return
  }

  if (expectImages !== null) {
    const observed = distinctUploadIdentities(originalText).size
    if (observed < expectImages) {
      console.error(
        `plan-image-guard: original contains ${observed} distinct image identities; expected at least ${expectImages}`,
      )
      process.exitCode = 1
      return
    }
  }

  if (requireVerbatim) {
    const mismatch = verifyVerbatimOriginalNotes(originalText, rewrittenText)
    if (mismatch) {
      console.error(mismatch)
      process.exitCode = 1
      return
    }
  }

  if (requireSafeSource) {
    const mismatch = verifySafeSource(originalText, rewrittenText)
    if (mismatch) {
      console.error(mismatch)
      process.exitCode = 1
      return
    }
  }

  if (requireUnsignedUploads) {
    const signed = signedUploadCount(rewrittenText)
    if (signed > 0) {
      console.error(
        `plan-image-guard: rewritten description contains ${signed} signed upload URL(s); strip query strings before write`,
      )
      process.exitCode = 1
      return
    }
  }

  const dropped = findDroppedImages(originalText, rewrittenText)
  if (dropped.length > 0) {
    for (const url of dropped) {
      console.error(url)
    }
    console.error(
      `plan-image-guard: ${dropped.length} image reference(s) dropped from the rewritten description`,
    )
    process.exitCode = 1
  }
}

// isMainModule resolves both paths through symlinks so this fail-closed CLI gate cannot be skipped.
const invokedDirectly = isMainModule(import.meta.url)
if (invokedDirectly) {
  try {
    main()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
