import ts from 'typescript'
import { scannedFiles } from './registry.mjs'

/** Canonical, tracked source files covered by a comment guard's registry record. */
export function commentFiles(repoRoot, scans) {
  return scannedFiles(repoRoot, scans).filter(
    (file) =>
      /\.(?:go|ts|tsx|mjs)$/.test(file) &&
      !/(?:^|\/)gen\//.test(file) &&
      !/\.pb\.go$|_pb\.ts$|\.generated\.mjs$/.test(file) &&
      !file.startsWith('.codex/skills/') &&
      !/(?:^|\/)toolbox\//.test(file) &&
      !file.startsWith('services/docs/src/vendor/'),
  )
}

function goCommentRanges(text) {
  const ranges = []
  for (let i = 0; i < text.length;) {
    const char = text[i]
    if (char === '"' || char === "'" || char === '`') {
      const quote = char
      i++
      while (i < text.length) {
        if (text[i] === '\\' && quote !== '`') i += 2
        else if (text[i++] === quote) break
      }
    } else if (text.startsWith('//', i)) {
      const pos = i
      while (i < text.length && !/[\r\n]/.test(text[i])) i++
      ranges.push({ pos, end: i })
    } else if (text.startsWith('/*', i)) {
      const pos = i
      const end = text.indexOf('*/', i + 2)
      i = end < 0 ? text.length : end + 2
      ranges.push({ pos, end: i })
    } else i++
  }
  return ranges
}

function typeScriptCommentRanges(text, kind) {
  const source = ts.createSourceFile(
    kind === 'tsx' ? 'source.tsx' : 'source.ts',
    text,
    ts.ScriptTarget.Latest,
    true,
    kind === 'tsx' ? ts.ScriptKind.TSX : kind === 'mjs' ? ts.ScriptKind.JS : ts.ScriptKind.TS,
  )
  const ranges = new Map()
  const literals = []
  const protectedKinds = new Set([
    ts.SyntaxKind.StringLiteral,
    ts.SyntaxKind.RegularExpressionLiteral,
    ts.SyntaxKind.NoSubstitutionTemplateLiteral,
    ts.SyntaxKind.TemplateHead,
    ts.SyntaxKind.TemplateMiddle,
    ts.SyntaxKind.TemplateTail,
    ts.SyntaxKind.JsxText,
  ])
  const add = (comments) => {
    for (const range of comments ?? []) ranges.set(range.pos, range)
  }
  const visit = (node) => {
    if (protectedKinds.has(node.kind)) literals.push([node.getStart(source), node.end])
    add(ts.getLeadingCommentRanges(text, node.pos))
    add(ts.getTrailingCommentRanges(text, node.end))
    for (const child of node.getChildren(source)) visit(child)
  }
  visit(source)
  return [...ranges.values()].filter(
    ({ pos }) => !literals.some(([start, end]) => pos >= start && pos < end),
  )
}

/** Extract comments with their delimiters and one-based source line spans. */
export function commentsOf(text, kind) {
  kind = kind.replace(/^\./, '').toLowerCase()
  const ranges = (kind === 'go' ? goCommentRanges(text) : typeScriptCommentRanges(text, kind)).sort(
    (a, b) => a.pos - b.pos,
  )
  const lineStarts = [0]
  for (let i = 0; i < text.length; i++) {
    if (text[i] === '\r') {
      if (text[i + 1] === '\n') i++
      lineStarts.push(i + 1)
    } else if (text[i] === '\n') lineStarts.push(i + 1)
  }
  const lineOf = (pos) => {
    let low = 0
    let high = lineStarts.length
    while (low + 1 < high) {
      const middle = (low + high) >>> 1
      if (lineStarts[middle] <= pos) low = middle
      else high = middle
    }
    return low + 1
  }
  const comments = []
  let previous
  for (const range of ranges) {
    const line = lineOf(range.pos)
    const endLine = lineOf(Math.max(range.pos, range.end - 1))
    const content = text.slice(range.pos, range.end)
    const prior = comments.at(-1)
    if (
      previous &&
      content.startsWith('//') &&
      text.startsWith('//', previous.pos) &&
      line === prior.endLine + 1 &&
      /^\r?\n[^\S\r\n]*$/.test(text.slice(previous.end, range.pos))
    ) {
      prior.text += text.slice(previous.end, range.end)
      prior.endLine = endLine
    } else comments.push({ text: content, line, endLine })
    previous = range
  }
  return comments
}
