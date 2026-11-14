import { posix } from 'node:path'
import { detectAreas } from './verify-areas.mjs'

export const MAX_COVER_READS = 20
export const MAX_CONFIG_READS = 30
export const MAX_PATH_PATTERNS = 1000
const relative = /^\.\.?(?:\/|$)/
const scriptExtension = /\.(?:[cm]?[jt]s|[jt]sx)$/
const jsxFree = /\.[cm]?ts$/
// Words a new expression, so a JSX element or a regex, can follow. Any other word ends an operand.
const expressionKeyword =
  /^(?:return|throw|case|default|yield|await|typeof|void|delete|new|in|of|instanceof|else|do|extends|break|continue|debugger)$/
export const testPath = /(?:\.(?:test|spec)\.[^/]+$|(?:^|\/)(?:__tests__|e2e)\/)/
/** @typedef {import('./verify-areas.mjs').ChangedFile} ChangedFile */
/** @typedef {{outcome?: string, verdict?: string, recorded?: boolean}} ProofOutcome */

/** Lex only code and quoted literals. Template text and regex literals are opaque. Ambiguous slash contexts reject coverage,
 * so skipped text cannot hide a mock or manufacture a module reference. JSX is a primary expression, so a `<` on the
 * line of a finished operand is a comparison or a type argument list and lexes as code, as does every `<` in a file that holds no JSX.
 * @param {string} source @param {boolean} [jsxAllowed]
 * @returns {{value:string, literal:boolean}[]|null}
 */
function tokens(source, jsxAllowed = true) {
  const result = []
  let at = 0
  let ambiguous = false
  const quoted = (quote) => {
    const start = ++at
    let escaped = false
    while (at < source.length && source[at] !== quote) {
      if (source[at] === '\\') {
        escaped = true
        at++
      }
      at++
    }
    if (at >= source.length) return null
    const value = source.slice(start, at++)
    return escaped ? null : value
  }
  const slash = () => {
    at++
    let bracket = false
    while (at < source.length && !/[\r\n]/.test(source[at])) {
      const c = source[at++]
      if (c === '\\') at++
      else if (c === '[') bracket = true
      else if (c === ']') bracket = false
      else if (c === '/' && !bracket) break
    }
  }
  const comment = () => {
    if (source.startsWith('//', at)) {
      while (at < source.length && !/[\n\r\u2028\u2029]/.test(source[at])) at++
      return true
    }
    if (source.startsWith('/*', at)) {
      const end = source.indexOf('*/', at + 2)
      at = end < 0 ? source.length : end + 2
      return true
    }
    return false
  }
  const template = (collect = true) => {
    at++
    while (at < source.length) {
      if (source[at] === '\\') {
        at += 2
        continue
      }
      if (source[at] === '`') {
        at++
        return
      }
      if (source.startsWith('${', at)) {
        at += 2
        const start = at
        let depth = 1
        while (at < source.length && depth) {
          if (comment()) continue
          const c = source[at]
          if (c === "'" || c === '"') quoted(c)
          else if (c === '`') template(false)
          else if (c === '/') {
            ambiguous = true
            slash()
          } else {
            at++
            if (c === '{') depth++
            if (c === '}') depth--
          }
        }
        if (collect && depth === 0) {
          const expression = tokens(source.slice(start, at - 1), jsxAllowed)
          if (expression === null) ambiguous = true
          else
            result.push({ value: ';', literal: false }, ...expression, {
              value: ';',
              literal: false,
            })
        }
      } else at++
    }
  }
  // JSX tag slashes are markup, while child expressions remain executable code.
  const jsx = () => {
    const tags = []
    const expression = () => {
      const start = ++at
      let depth = 1
      while (at < source.length && depth) {
        if (comment()) continue
        const c = source[at]
        if (c === "'" || c === '"') quoted(c)
        else if (c === '`') template(false)
        else if (c === '/') {
          ambiguous = true
          slash()
        } else {
          at++
          if (c === '{') depth++
          if (c === '}') depth--
        }
      }
      const code = depth === 0 ? tokens(source.slice(start, at - 1)) : null
      if (code === null) ambiguous = true
      else result.push({ value: ';', literal: false }, ...code, { value: ';', literal: false })
    }
    do {
      if (source[at] === '{') {
        expression()
        continue
      }
      if (source[at] !== '<') {
        at++
        continue
      }
      const tag = /^<(\/?)([A-Za-z_$][\w$.:-]*)?(?=[\s/>])/.exec(source.slice(at))
      if (!tag) {
        ambiguous = true
        return
      }
      const closing = tag[1] === '/'
      const name = tag[2] ?? ''
      at += tag[0].length
      while (at < source.length && source[at] !== '>') {
        const c = source[at]
        if (c === "'" || c === '"') quoted(c)
        else if (c === '{') expression()
        else at++
      }
      if (at >= source.length) {
        ambiguous = true
        return
      }
      const selfClosing = source[at - 1] === '/'
      at++
      if (closing) {
        if (tags.pop() !== name) {
          ambiguous = true
          return
        }
      } else if (!selfClosing) tags.push(name)
    } while (at < source.length && tags.length)
    if (tags.length) ambiguous = true
  }
  const operandEnds = () => {
    const previous = result.at(-1)
    return (
      previous !== undefined &&
      !previous.literal &&
      /^[\w$]+$/.test(previous.value) &&
      !expressionKeyword.test(previous.value)
    )
  }
  // A line break lets a statement that ends in a word, such as `let x`, end before JSX.
  let lineBreak = false
  while (at < source.length) {
    const from = at
    if (comment() || /\s/.test(source[at])) {
      if (at === from) at++
      lineBreak ||= /[\n\r\u2028\u2029]/.test(source.slice(from, at))
      continue
    }
    const c = source[at]
    const sameLine = !lineBreak
    lineBreak = false
    if (
      c === '<' &&
      jsxAllowed &&
      !(sameLine && operandEnds()) &&
      /^<(?:[A-Za-z_$][\w$.:-]*(?=[\s/>])|>)/.test(source.slice(at))
    ) {
      result.push({ value: ';', literal: false })
      jsx()
      result.push({ value: ';', literal: false })
      continue
    }
    if (c === '`') {
      template()
      result.push({ value: 'opaque', literal: false })
      continue
    }
    if (c === '/') {
      const previous = result.at(-1)
      if (
        previous &&
        !previous.literal &&
        [')', '}', '<', '>', 'await', 'yield', 'of'].includes(previous.value)
      ) {
        ambiguous = true
        break
      }
      const regexPosition =
        !previous ||
        (!previous.literal &&
          ([
            '=',
            '(',
            '[',
            '{',
            ',',
            ':',
            ';',
            '!',
            '?',
            '&',
            '|',
            '+',
            '-',
            '*',
            '/',
            '%',
            '^',
            '~',
            '<',
            '>',
            '=>',
            '&&',
            '||',
            '??',
          ].includes(previous.value) ||
            (expressionKeyword.test(previous.value) &&
              !['.', '?.'].includes(result.at(-2)?.value))))
      if (regexPosition) {
        slash()
        result.push({ value: 'opaque', literal: false })
      } else {
        result.push({ value: '/', literal: false })
        at++
      }
      continue
    }
    if (c === "'" || c === '"') {
      const value = quoted(c)
      result.push({ value: value ?? '', literal: value !== null })
      continue
    }
    const word = /^(?:[\w$]+|\+\+|--|=>|&&|\|\||\?\?|\?\.)/.exec(source.slice(at))?.[0]
    result.push({ value: word ?? c, literal: false })
    at += word?.length ?? 1
  }
  return ambiguous ? null : result
}

/** @param {{value:string, literal:boolean}[]} code @param {number} open index of a `<` @returns {number} index after its `>` */
function afterTypeArguments(code, open) {
  let depth = 0
  for (let at = open; at < code.length; at++) {
    if (code[at].literal) continue
    if (code[at].value === '<') depth++
    if (code[at].value === '>' && --depth === 0) return at + 1
  }
  return -1
}

/** @param {string} source @param {string} [path] @returns {{specs:string[],mocked:string[]}} */
function moduleReferences(source, path = '') {
  const code = tokens(source, !jsxFree.test(path))
  if (code === null) return { specs: [], mocked: [] }
  const specs = new Set()
  const mocked = new Set()
  for (let i = 0; i < code.length; i++) {
    const token = code[i]
    if (token.literal) continue
    const value = token.value
    const next = code[i + 1]
    if (
      ['vi', 'jest'].includes(value) &&
      code[i + 1]?.value === '.' &&
      ['mock', 'doMock'].includes(code[i + 2]?.value)
    ) {
      const open = code[i + 3]?.value === '<' ? afterTypeArguments(code, i + 3) : i + 3
      if (open < 0) return { specs: [], mocked: [] }
      const arg = code[open]?.value === '(' ? code[open + 1] : undefined
      // An escaped string lexes as an empty non-literal, so its module is unknown.
      if (arg?.value === '' && !arg.literal) return { specs: [], mocked: [] }
      if (arg?.literal) mocked.add(arg.value)
      else if (
        arg?.value === 'import' &&
        code[open + 2]?.value === '(' &&
        code[open + 3]?.literal &&
        code[open + 4]?.value === ')'
      )
        mocked.add(code[open + 3].value)
    }
    if (['.', '?.'].includes(code[i - 1]?.value)) continue
    if (value === 'import' && next?.literal) specs.add(next.value)
    if (
      ['import', 'require'].includes(value) &&
      next?.value === '(' &&
      code[i + 2]?.literal &&
      code[i + 3]?.value === ')'
    )
      specs.add(code[i + 2].value)
    if (
      (value === 'import' && next && !next.literal && /^[\w${*]/.test(next.value)) ||
      (value === 'export' && ['{', '*'].includes(next?.value))
    ) {
      for (let j = i + 1; j < code.length; j++) {
        if ([';', 'import', 'export'].includes(code[j].value) && !code[j].literal) break
        if (code[j].value === 'from' && !code[j].literal && code[j + 1]?.literal) {
          specs.add(code[j + 1].value)
          break
        }
      }
    }
  }
  return { specs: [...specs], mocked: [...mocked] }
}

/** Static relative references, excluding explicitly mocked spellings. Coverage additionally
 * compares each import and mock against the actual changed source path.
 * @param {string} source @param {string} [path] the test's path, whose extension says whether it can hold JSX
 * @returns {string[]}
 */
export function importSpecifiers(source, path) {
  const { specs, mocked } = moduleReferences(source, path)
  return specs.filter(
    (spec) =>
      relative.test(spec) &&
      !mocked.some((mock) => posix.normalize(mock) === posix.normalize(spec)),
  )
}

/** @param {string} test @param {string} spec @param {string} ui @returns {boolean} */
export function resolvesTo(test, spec, ui) {
  return relative.test(spec) && targets(posix.join(posix.dirname(test), spec), ui)
}

/** @param {string} path repo-relative module path @param {string} ui @returns {boolean} */
function targets(path, ui) {
  const target = path.replace(/(?<=.)\/+$/, '')
  const source = posix.normalize(ui)
  if (target === source) return true
  if (!scriptExtension.test(source)) return false
  if (target === source.slice(0, -posix.extname(source).length)) return true
  if (
    /\.(?:js|jsx|mjs|cjs)$/.test(target) &&
    /\.(?:ts|tsx|mts|cts)$/.test(source) &&
    target.slice(0, -posix.extname(target).length) ===
      source.slice(0, -posix.extname(source).length)
  )
    return true
  return source.startsWith(`${target}/index.`) && !source.slice(target.length + 1).includes('/')
}

/** Remove the comments and trailing commas tsconfig permits, then parse; null when malformed.
 * @param {string} text @returns {unknown} */
function parseConfig(text) {
  /** @type {string[]} */
  const json = []
  let comma = -1
  for (let at = text.charCodeAt(0) === 0xfeff ? 1 : 0; at < text.length; at++) {
    const c = text[at]
    if (c === '"') {
      const start = at
      for (at++; at < text.length && text[at] !== '"'; at++) if (text[at] === '\\') at++
      json.push(text.slice(start, at + 1))
      comma = -1
    } else if (text.startsWith('//', at)) {
      while (at + 1 < text.length && text[at + 1] !== '\n') at++
    } else if (text.startsWith('/*', at)) {
      const end = text.indexOf('*/', at + 2)
      if (end < 0) return null
      at = end + 1
      json.push(' ')
    } else {
      if ((c === '}' || c === ']') && comma >= 0) json[comma] = ' '
      json.push(c)
      if (c === ',') comma = json.length - 1
      else if (!/\s/.test(c)) comma = -1
    }
  }
  try {
    return JSON.parse(json.join(''))
  } catch {
    return null
  }
}

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const isObject = (value) => typeof value === 'object' && value !== null && !Array.isArray(value)

/** How a test's non-relative specifiers resolve: plainly as packages, not at all because a config could
 * not be read, or through the merged `baseUrl` and `paths`, each already repo-relative.
 * @typedef {{state:'none'}|{state:'unknown'}|({state:'aliases'} & CompilerPaths)} ModuleScope */
/** @typedef {{baseUrl?:string, paths?:{dir:string, patterns:Record<string, unknown>}}} CompilerPaths */

/** Find each test's nearest tsconfig or jsconfig in the head's file list and merge its relative
 * `extends` chain, reading at most `MAX_CONFIG_READS` files. Without a file list every specifier is
 * a package, since a missing config and a failed read look alike.
 * @param {(path:string)=>string|null} readContent @param {()=>Iterable<string>|null} listFiles */
function moduleScopes(readContent, listFiles) {
  /** @type {Set<string>|null|undefined} */
  let present
  /** @type {Map<string, CompilerPaths|undefined|'loading'>} */
  const loaded = new Map()
  let reads = 0
  /** @param {string} path @returns {CompilerPaths|undefined} */
  const load = (path) => {
    if (loaded.has(path)) {
      const known = loaded.get(path)
      return known === 'loading' ? undefined : known
    }
    loaded.set(path, 'loading')
    const result = parse(path)
    loaded.set(path, result)
    return result
  }
  /** @param {string} path @returns {CompilerPaths|undefined} */
  const parse = (path) => {
    if (!present?.has(path) || reads >= MAX_CONFIG_READS) return undefined
    reads++
    let config
    try {
      const text = readContent(path)
      config = typeof text === 'string' ? parseConfig(text) : null
    } catch {
      return undefined
    }
    if (!isObject(config)) return undefined
    const dir = posix.dirname(path)
    /** @type {CompilerPaths} */
    let merged = {}
    for (const parent of [config.extends ?? []].flat()) {
      if (typeof parent !== 'string' || parent.startsWith('/')) return undefined
      if (!relative.test(parent)) continue
      const base = posix.join(dir, parent)
      const inherited = load(present?.has(base) || base.endsWith('.json') ? base : `${base}.json`)
      if (!inherited) return undefined
      merged = { ...merged, ...inherited }
    }
    const own = isObject(config.compilerOptions) ? config.compilerOptions : {}
    if (own.baseUrl !== undefined) {
      if (typeof own.baseUrl !== 'string' || own.baseUrl.startsWith('/')) return undefined
      merged.baseUrl = posix.join(dir, own.baseUrl)
    }
    if (isObject(own.paths)) {
      if (Object.keys(own.paths).length > MAX_PATH_PATTERNS) return undefined
      merged.paths = { dir, patterns: own.paths }
    }
    return merged
  }
  /** @param {string} test @returns {ModuleScope} */
  return (test) => {
    if (present === undefined) {
      try {
        const paths = listFiles()
        present = paths ? new Set(paths) : null
      } catch {
        present = null
      }
    }
    if (!present) return { state: 'none' }
    for (let dir = posix.dirname(test); ; dir = posix.dirname(dir)) {
      for (const name of ['tsconfig.json', 'jsconfig.json']) {
        const path = dir === '.' ? name : `${dir}/${name}`
        if (!present.has(path)) continue
        const paths = load(path)
        return paths ? { state: 'aliases', ...paths } : { state: 'unknown' }
      }
      if (dir === '.' || dir === '/') return { state: 'none' }
    }
  }
}

/** The modules a non-relative specifier can name under TypeScript's rules: an exact `paths` key, else
 * the wildcard with the longest prefix, else `baseUrl`. TypeScript takes the first substitution that
 * exists, so an import counts only through the first, while a mock is checked against every one.
 * null means the specifier matched a pattern that names no usable module.
 * @param {ModuleScope} scope @param {string} spec @returns {{first:string|null, all:string[]}|null} */
function aliasTargets(scope, spec) {
  if (scope.state !== 'aliases') return { first: null, all: [] }
  const { baseUrl, paths } = scope
  /** @type {{prefix:number, star:number, pattern:string}|null} */
  let best = null
  for (const pattern of Object.keys(paths?.patterns ?? {})) {
    const star = pattern.indexOf('*')
    if (star !== pattern.lastIndexOf('*')) continue
    const matches =
      star < 0
        ? pattern === spec
        : spec.length >= pattern.length - 1 &&
          spec.startsWith(pattern.slice(0, star)) &&
          spec.endsWith(pattern.slice(star + 1))
    const prefix = star < 0 ? Infinity : star
    if (matches && (!best || prefix > best.prefix)) best = { prefix, star, pattern }
  }
  if (!best || !paths) {
    const target = baseUrl === undefined ? null : posix.join(baseUrl, spec)
    return { first: target, all: target === null ? [] : [target] }
  }
  const { star, pattern } = best
  const substitutions = paths.patterns[pattern]
  if (!Array.isArray(substitutions)) return null
  const captured = star < 0 ? '' : spec.slice(star, spec.length - (pattern.length - star - 1))
  const all = substitutions.map((substitution) =>
    typeof substitution === 'string' && !substitution.startsWith('/')
      ? posix.join(baseUrl ?? paths.dir, substitution.replace('*', captured))
      : null,
  )
  return all.includes(null) || !all.length
    ? null
    : { first: all[0], all: all.filter((target) => target !== null) }
}

/** `listFiles` names every file at the head, which alias resolution needs to find configs.
 * @param {{files:ChangedFile[],readContent:(path:string)=>string|null,listFiles?:()=>Iterable<string>|null}} input */
export function testCoverage({ files, readContent, listFiles = () => null }) {
  const uiPaths = detectAreas(files).find((area) => area.area === 'ui')?.paths ?? []
  const tests = files
    .filter(
      (file) =>
        ['added', 'modified', 'renamed'].includes(file.status) &&
        testPath.test(file.path) &&
        scriptExtension.test(file.path),
    )
    .map((file) => file.path)
  const coveredPaths = new Set()
  const scopeOf = moduleScopes(readContent, listFiles)
  if (uiPaths.length && tests.length <= MAX_COVER_READS) {
    for (const path of tests) {
      let source
      try {
        source = readContent(path)
      } catch {
        continue
      }
      if (typeof source !== 'string') continue
      const { specs, mocked } = moduleReferences(source, path)
      const aliased = (/** @type {string} */ spec) => !relative.test(spec)
      /** @type {ModuleScope} */
      const scope = [...specs, ...mocked].some(aliased) ? scopeOf(path) : { state: 'none' }
      /** @param {string} spec */
      const resolve = (spec) =>
        relative.test(spec)
          ? {
              first: posix.join(posix.dirname(path), spec),
              all: [posix.join(posix.dirname(path), spec)],
            }
          : aliasTargets(scope, spec)
      const mocks = mocked.map(resolve)
      // An alias mock that cannot be resolved might name the UI file, so nothing in the test counts.
      if (mocks.includes(null) || (scope.state === 'unknown' && mocked.some(aliased))) continue
      const imported = specs.map((spec) => resolve(spec)?.first ?? null).filter((t) => t !== null)
      const replaced = mocks.flatMap((mock) => mock?.all ?? [])
      for (const ui of uiPaths)
        if (imported.some((t) => targets(t, ui)) && !replaced.some((t) => targets(t, ui)))
          coveredPaths.add(ui)
    }
  }
  const uncovered = uiPaths.filter((path) => !coveredPaths.has(path))
  return { uiPaths, tests, uncovered, covered: uiPaths.length > 0 && uncovered.length === 0 }
}

/** @param {Function} read @param {{repo:string,headSha:string,files:(ChangedFile|{filename:string,previous_filename?:string,status?:string,changes?:number,patch?:string})[]}} context */
export function readUiTestCoverage(read, { repo, headSha, files }) {
  const changed = files.map((file) =>
    'filename' in file
      ? {
          path: file.filename,
          previousPath: file.previous_filename,
          status: file.status,
          changes: file.changes,
          patch: file.patch,
        }
      : file,
  )
  return testCoverage({
    files: changed,
    listFiles() {
      const tree = `repos/${repo}/git/trees/${encodeURIComponent(headSha)}?recursive=1`
      const r = read('gh', ['api', tree], { json: true })
      const entries = r?.ok && r.payload?.truncated === false ? r.payload.tree : null
      return Array.isArray(entries)
        ? entries.filter((entry) => entry?.type === 'blob').map((entry) => String(entry.path))
        : null
    },
    readContent(path) {
      const encoded = path.split('/').map(encodeURIComponent).join('/')
      const r = read(
        'gh',
        ['api', `repos/${repo}/contents/${encoded}?ref=${encodeURIComponent(headSha)}`],
        { json: true },
      )
      const p = r?.payload
      return r?.ok &&
        p?.type === 'file' &&
        p?.encoding === 'base64' &&
        typeof p.content === 'string'
        ? Buffer.from(p.content, 'base64').toString('utf8')
        : null
    },
  })
}

/** @param {{ciGreen?:boolean,coverage?:{covered?:boolean},outcomes?:ProofOutcome[]}} input
 * @returns {'tests'|'recording'|'none'} */
export function uiProof({ ciGreen, coverage, outcomes = [] }) {
  if (ciGreen === true && coverage?.covered === true) return 'tests'
  return outcomes.some((o) => o.outcome === 'ok' && o.verdict === 'pass' && o.recorded === true)
    ? 'recording'
    : 'none'
}
