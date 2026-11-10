import { posix } from 'node:path'
import { detectAreas } from './verify-areas.mjs'

export const MAX_COVER_READS = 20
const scriptExtension = /\.(?:[cm]?[jt]s|[jt]sx)$/
export const testPath = /(?:\.(?:test|spec)\.[^/]+$|(?:^|\/)(?:__tests__|e2e)\/)/
/** @typedef {import('./verify-areas.mjs').ChangedFile} ChangedFile */
/** @typedef {{outcome?: string, verdict?: string, recorded?: boolean}} ProofOutcome */

/** Lex only code and quoted literals. Template text and regex literals are opaque. Ambiguous slash contexts reject coverage,
 * so skipped text cannot hide a mock or manufacture a module reference.
 * @param {string} source
 * @returns {{value:string, literal:boolean}[]|null}
 */
function tokens(source) {
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
      while (at < source.length && source[at] !== '\n') at++
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
          const expression = tokens(source.slice(start, at - 1))
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
  while (at < source.length) {
    if (comment()) continue
    const c = source[at]
    if (/\s/.test(c)) {
      at++
      continue
    }
    if (c === '<' && /^<(?:[A-Za-z_$][\w$.:-]*(?=[\s/>])|>)/.test(source.slice(at))) {
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
        [')', '}', '<', 'await', 'yield'].includes(previous.value)
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
            (['return', 'throw', 'case', 'delete', 'void', 'typeof', 'else', 'do'].includes(
              previous.value,
            ) &&
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

/** @param {string} source @returns {{specs:string[],mocked:string[]}} */
function moduleReferences(source) {
  const code = tokens(source)
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
      ['mock', 'doMock'].includes(code[i + 2]?.value) &&
      code[i + 3]?.value === '('
    ) {
      const arg = code[i + 4]
      if (arg?.literal) mocked.add(arg.value)
      else if (
        arg?.value === 'import' &&
        code[i + 5]?.value === '(' &&
        code[i + 6]?.literal &&
        code[i + 7]?.value === ')'
      )
        mocked.add(code[i + 6].value)
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
  return {
    specs: [...specs].filter((spec) => /^\.\.?\//.test(spec)),
    mocked: [...mocked].filter((spec) => /^\.\.?\//.test(spec)),
  }
}

/** Static relative references, excluding explicitly mocked spellings. Coverage additionally
 * compares each import and mock against the actual changed source path.
 * @param {string} source @returns {string[]}
 */
export function importSpecifiers(source) {
  const { specs, mocked } = moduleReferences(source)
  return specs.filter(
    (spec) => !mocked.some((mock) => posix.normalize(mock) === posix.normalize(spec)),
  )
}

/** @param {string} test @param {string} spec @param {string} ui @returns {boolean} */
export function resolvesTo(test, spec, ui) {
  if (!/^\.\.?\//.test(spec)) return false
  const target = posix.normalize(posix.join(posix.dirname(test), spec))
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

/** @param {{files:ChangedFile[],readContent:(path:string)=>string|null}} input */
export function testCoverage({ files, readContent }) {
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
  if (uiPaths.length && tests.length <= MAX_COVER_READS) {
    for (const path of tests) {
      let source
      try {
        source = readContent(path)
      } catch {
        continue
      }
      if (typeof source !== 'string') continue
      const { specs, mocked } = moduleReferences(source)
      for (const ui of uiPaths)
        if (
          specs.some((spec) => resolvesTo(path, spec, ui)) &&
          !mocked.some((spec) => resolvesTo(path, spec, ui))
        )
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
