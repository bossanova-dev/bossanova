/** Shared literal-context scanners for description normalizers. */
const quoteContent = (line, limit = Infinity) => {
  let depth = 0
  while (depth < limit) {
    const prefix = /^[ \t]{0,3}>[ \t]?/.exec(line)
    if (!prefix) break
    line = line.slice(prefix[0].length)
    depth += 1
  }
  return { line, depth }
}

export const fencedLineMap = (lines) => {
  const FENCE_OPEN = /^[ \t]{0,3}(`{3,}|~{3,})/
  const FENCE_CLOSE = /^[ \t]{0,3}(`{3,}|~{3,})[ \t]*$/
  const literal = lines.map(() => false)
  let fence = null
  for (let i = 0; i < lines.length; i += 1) {
    if (fence) {
      const content = quoteContent(lines[i], fence.depth)
      if (content.depth === fence.depth) {
        literal[i] = true
        const close = FENCE_CLOSE.exec(content.line)
        if (close && close[1][0] === fence.char && close[1].length >= fence.length) fence = null
        continue
      }
      fence = null
    }
    const content = quoteContent(lines[i])
    const open = FENCE_OPEN.exec(content.line)
    if (open) {
      literal[i] = true
      fence = { char: open[1][0], length: open[1].length, depth: content.depth }
    }
  }
  return literal
}

export const indentedCodeMap = (lines, literal) => {
  const INDENTED = /^(?: {4}| {0,3}\t)/
  const ATX_HEADING = /^[ \t]{0,3}#{1,6}(?:[ \t]|$)/
  const code = lines.map(() => false)
  for (let i = 0; i < lines.length; i += 1) {
    if (literal[i] || lines[i].trim() === '' || !INDENTED.test(lines[i])) continue
    const prev = i - 1
    code[i] =
      prev < 0 ||
      lines[prev].trim() === '' ||
      code[prev] ||
      literal[prev] ||
      ATX_HEADING.test(lines[prev])
  }
  return code
}

export const inlineCodeEnd = (line, i) => {
  let open = i
  while (open < line.length && line[open] === '`') open += 1
  const width = open - i
  let close = -1
  for (let j = open; j < line.length;) {
    if (line[j] !== '`') {
      j += 1
      continue
    }
    let k = j
    while (k < line.length && line[k] === '`') k += 1
    if (k - j === width) {
      close = k
      break
    }
    j = k
  }
  return close === -1 ? open : close
}

export const mapProseLines = (text, transform) => {
  const lines = String(text).split('\n')
  const literal = fencedLineMap(lines)
  const code = indentedCodeMap(lines, literal)
  return lines.map((line, i) => (literal[i] || code[i] ? line : transform(line))).join('\n')
}
