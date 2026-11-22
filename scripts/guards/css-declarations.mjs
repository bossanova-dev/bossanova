// The declarations a stylesheet spells out, for the web CSS guards. Comments and strings are
// blanked first, so a value never holds either, and every offset still indexes the original text.

/** Comments and strings blanked to spaces, keeping every offset and line in place. */
export function blankCommentsAndStrings(css) {
  return css
    .replace(/\/\*[\s\S]*?\*\//g, (comment) => comment.replace(/[^\n]/g, ' '))
    .replace(/(["'])(?:\\.|(?!\1)[^\\\n])*\1/g, (text) => text.replace(/[^\n]/g, ' '))
}

// A rule whose every selector is :root (or html), the token layer's own scope.
function isTokenScope(stack) {
  const rule = stack.findLast((prelude) => !prelude.startsWith('@'))
  return (
    rule !== undefined &&
    rule.split(',').every((selector) => /^(?::root|html)(?![\w-])/.test(selector.trim()))
  )
}

/**
 * Every declaration inside a rule, with its value as blanked text and its offset in the source.
 * `tokenDefinition` marks a custom property declared on :root, the token layer's own scope.
 *
 * @param {string} css
 * @returns {{property: string, value: string, valueStart: number, line: number,
 *   tokenDefinition: boolean, stack: string[]}[]}
 */
export function cssDeclarations(css) {
  const source = blankCommentsAndStrings(css)
  const found = []
  const stack = []
  let start = 0
  const declaration = (end) => {
    const text = source.slice(start, end)
    const colon = text.indexOf(':')
    const property = text.slice(0, colon).trim().toLowerCase()
    if (colon === -1 || property.startsWith('@') || stack.length === 0) return
    found.push({
      property,
      value: text.slice(colon + 1),
      valueStart: start + colon + 1,
      line: source.slice(0, start + text.search(/\S/)).split('\n').length,
      tokenDefinition: /^-{2}/.test(property) && isTokenScope(stack),
      stack: [...stack],
    })
  }
  for (let i = 0; i < source.length; i++) {
    const ch = source[i]
    if (ch === '{') {
      stack.push(source.slice(start, i).trim())
      start = i + 1
    } else if (ch === '}' || ch === ';') {
      declaration(i)
      if (ch === '}') stack.pop()
      start = i + 1
    }
  }
  return found
}
