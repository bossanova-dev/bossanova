/** @typedef {{value: string, quoted: boolean}} ShellToken */
/** @typedef {{tokens: string[], negated: boolean, skipped: boolean, separatorAfter: string | null}} CommandSegment */
export const SHELL_BUILTINS = new Set([
  '[',
  'cd',
  'echo',
  'false',
  'printf',
  'pwd',
  'set',
  'test',
  'true',
])
const SHELL_COMMAND_SEPARATORS = new Set([';', '|', '&&', '||'])
const SHELL_OPENERS = new Set(['if', 'then', 'elif', 'else', 'while', 'until', 'do', '{', '('])
const SHELL_CLOSERS = new Set(['done', 'fi', 'esac', '}', ')'])
const ENV_VALUE_OPTIONS = new Set(['-u', '--unset', '-C', '--chdir'])
const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/

/** @param {ShellToken[]} tokens @param {string} value @param {boolean} quoted */
function pushShellToken(tokens, value, quoted) {
  if (value) tokens.push({ value, quoted })
}

/** @param {string} command @returns {ShellToken[]} */
export function tokenizeSimpleShellDetailed(command) {
  const source = String(command ?? '')
  /** @type {ShellToken[]} */
  const tokens = []
  let current = ''
  let quoted = false
  /** @type {string | null} */
  let quote = null
  let substitutionDepth = 0
  for (let index = 0; index < source.length; index += 1) {
    const char = source[index]
    if (quote) {
      if (char === quote) quote = null
      else current += char
      continue
    }
    if (substitutionDepth > 0) {
      current += char
      if (char === '(') substitutionDepth += 1
      if (char === ')') substitutionDepth -= 1
      continue
    }
    if (char === '(' && /[$<>]$/.test(current)) {
      current += char
      substitutionDepth = 1
      continue
    }
    if (char === '"' || char === "'" || char === '`') {
      quote = char
      quoted = true
    } else if (
      (char === '&' && source[index + 1] === '&') ||
      (char === '|' && source[index + 1] === '|')
    ) {
      pushShellToken(tokens, current, quoted)
      current = ''
      quoted = false
      tokens.push({ value: char + source[index + 1], quoted: false })
      index += 1
    } else if (char === '|' || char === ';' || char === '(' || char === ')') {
      pushShellToken(tokens, current, quoted)
      current = ''
      quoted = false
      tokens.push({ value: char, quoted: false })
    } else if (/\s/.test(char)) {
      pushShellToken(tokens, current, quoted)
      current = ''
      quoted = false
    } else {
      current += char
    }
  }
  pushShellToken(tokens, current, quoted)
  return tokens
}

/** @param {string} command @returns {string[]} */
export function tokenizeSimpleShell(command) {
  return tokenizeSimpleShellDetailed(command).map((token) => token.value)
}

/** @param {string[]} tokens @param {{stripShellPreamble?: boolean}} [options] */
export function normalizeCommandTokens(tokens, { stripShellPreamble = true } = {}) {
  tokens = [...tokens]
  if (tokens[0] === '!') tokens.shift()
  while (tokens.length > 0 && ASSIGNMENT.test(tokens[0])) tokens.shift()
  if (tokens.at(0) === 'env') {
    tokens.shift()
    while (tokens.length > 0) {
      if (ASSIGNMENT.test(tokens[0])) {
        tokens.shift()
      } else if (tokens[0] === '--') {
        tokens.shift()
        break
      } else if (ENV_VALUE_OPTIONS.has(tokens[0])) {
        tokens.splice(0, 2)
      } else if (tokens[0].startsWith('-')) {
        tokens.shift()
      } else {
        break
      }
    }
  }
  if (stripShellPreamble && tokens[0] === 'set' && tokens[1] === '-o' && tokens[2] === 'pipefail') {
    tokens.splice(0, tokens[3] === ';' ? 4 : 3)
  }
  return tokens
}

/** @param {string[]} raw @param {string | null} separatorAfter @returns {CommandSegment} */
function analyzeSegment(raw, separatorAfter) {
  const tokens = [...raw]
  const assignmentOnly = tokens.length > 0 && tokens.every((token) => ASSIGNMENT.test(token))
  let openedGroup = false
  while (SHELL_OPENERS.has(tokens[0])) {
    if (tokens[0] === '(' || tokens[0] === '{') openedGroup = true
    tokens.shift()
  }
  const closerOnly =
    !openedGroup && tokens.length > 0 && tokens.every((token) => SHELL_CLOSERS.has(token))
  while (tokens.at(-1) === ')' || tokens.at(-1) === '}') tokens.pop()
  const negated = tokens[0] === '!'
  if (negated) tokens.shift()
  while (tokens[0] === '(') tokens.shift()
  const forHeader =
    tokens[0] === 'for' && /^[A-Za-z_][A-Za-z0-9_]*$/.test(tokens[1] ?? '') && tokens[2] === 'in'
  const skipped = assignmentOnly || closerOnly || forHeader
  const normalized =
    tokens[0] === '!' ? tokens : normalizeCommandTokens(tokens, { stripShellPreamble: false })
  return {
    tokens: skipped ? [] : normalized,
    negated: skipped ? false : negated,
    skipped,
    separatorAfter,
  }
}

/** @param {ShellToken[]} tokens @returns {CommandSegment[]} */
export function analyzeCommandSegments(tokens) {
  /** @type {CommandSegment[]} */
  const segments = []
  /** @type {string[]} */
  let current = []
  for (const token of tokens) {
    if (!token.quoted && SHELL_COMMAND_SEPARATORS.has(token.value)) {
      if (current.length > 0) segments.push(analyzeSegment(current, token.value))
      current = []
    } else current.push(token.value)
  }
  if (current.length > 0) segments.push(analyzeSegment(current, null))
  return segments
}
