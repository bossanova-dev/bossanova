// Dependency-free dbt YAML and SQL readers.
/** @typedef {{root: string, modelDirs: string[], folders: Record<string, any>}} Project */
/** @typedef {{name: string, type: string}} Column */
/** @typedef {{columns: Column[], resolved: boolean}} Selected */
import path from 'node:path'
import { MATERIALIZATIONS } from './verify-describe-blocks.mjs'

export const UNKNOWN_TYPE = '—'

/** `raw` without a trailing `# comment` outside quotes. */
/** @param {string} raw */
function stripComment(raw) {
  let quote = null

  for (let index = 0; index < raw.length; index += 1) {
    const char = raw[index]

    if (quote) {
      if (char === quote) quote = null
    } else if (char === "'" || char === '"') {
      quote = char
    } else if (char === '#' && (index === 0 || /\s/.test(raw[index - 1]))) {
      return raw.slice(0, index)
    }
  }

  return raw
}

/** @param {string} text */
const unquote = (text) => {
  const value = text.trim()

  return /^(['"]).*\1$/.test(value) ? value.slice(1, -1) : value
}

/** @param {string} text @returns {any} */
function scalar(text) {
  const value = text.trim()

  if (/^\[.*\]$/.test(value)) {
    return value
      .slice(1, -1)
      .split(',')
      .map(unquote)
      .filter((entry) => entry !== '')
  }

  if (value === 'true' || value === 'false') return value === 'true'
  if (value === 'null' || value === '~') return null

  return unquote(value)
}

/** @param {string} text @returns {any} */
export function parseYaml(text) {
  const lines = text.split('\n')
  let at = 0
  /** @param {string} raw */
  const indentOf = (raw) => raw.match(/^ */)[0].length
  /** @param {string} raw */
  const content = (raw) => {
    const line = stripComment(raw).trim()

    return line === '---' ? '' : line
  }
  const skipBlank = () => {
    while (at < lines.length && !content(lines[at])) at += 1
  }

  /** @param {number} indent @returns {any} */
  function node(indent) {
    skipBlank()

    return content(lines[at]).startsWith('-') ? list(indent) : map(indent)
  }

  /** The value after `key:` on a line indented `indent`. */
  /** @param {string} rest @param {number} indent @returns {any} */
  function value(rest, indent) {
    const inline = stripComment(rest).trim()

    if (/^[>|][-+]?\d*$/.test(inline)) {
      const body = []

      while (at < lines.length && (!lines[at].trim() || indentOf(lines[at]) > indent)) {
        body.push(lines[at].trim())
        at += 1
      }

      return body.join(' ').trim()
    }

    if (inline) {
      const body = [inline]
      while (at < lines.length && (!lines[at].trim() || indentOf(lines[at]) > indent)) {
        body.push(stripComment(lines[at]).trim())
        at += 1
      }
      return scalar(body.filter(Boolean).join(' '))
    }

    skipBlank()

    if (at >= lines.length) return null

    const next = lines[at]
    const nested =
      indentOf(next) > indent || (indentOf(next) === indent && content(next).startsWith('-'))

    return nested ? node(indentOf(next)) : null
  }

  /** @param {number} indent @returns {Record<string, any>} */
  function map(indent) {
    const out = {}

    for (;;) {
      skipBlank()

      if (at >= lines.length || indentOf(lines[at]) !== indent) return out

      const line = content(lines[at])

      if (line.startsWith('-')) return out

      const match = line.match(/^("[^"]*"|'[^']*'|[^:]+?)\s*:(?:\s+(.*))?$/)

      at += 1

      if (match) out[unquote(match[1])] = value(match[2] ?? '', indent)
    }
  }

  /** @param {number} indent @returns {any[]} */
  function list(indent) {
    const out = []

    for (;;) {
      skipBlank()

      if (at >= lines.length || indentOf(lines[at]) !== indent) return out

      const line = content(lines[at])

      if (!line.startsWith('-')) return out

      const rest = line.slice(1).trimStart()

      if (!rest) {
        at += 1
        out.push(value('', indent))
      } else if (/^[^'"[{][^:]*:(\s|$)/.test(rest)) {
        const itemIndent = indent + (line.length - rest.length)

        lines[at] = `${' '.repeat(itemIndent)}${rest}`
        out.push(map(itemIndent))
      } else {
        at += 1
        out.push(scalar(rest))
      }
    }
  }

  skipBlank()

  return at < lines.length ? node(indentOf(lines[at])) : null
}

/** dbt's materialisation names, mapped onto the proof page's. */
/** @param {unknown} name */
export const materialization = (name) => {
  const value = String(name ?? '')
    .trim()
    .toLowerCase()
    .replace('_', '-')

  return MATERIALIZATIONS.includes(value) ? value : undefined
}

/**
 * A dbt project from its dbt_project.yml: its root directory, its model directories, and the
 * `models:` folder config tree under the project's own name.
 */
/** @param {string} file @param {string} text */
export function readProject(file, text) {
  const config = parseYaml(text) ?? {}
  const root = path.posix.dirname(file)
  const modelPaths = Array.isArray(config['model-paths']) ? config['model-paths'] : ['models']

  return {
    folders: config.models?.[config.name] ?? {},
    modelDirs: modelPaths.map((dir) => path.posix.join(root, dir)),
    root,
  }
}

/** The model directory holding `file`, or undefined. */
/** @param {Project} project @param {string} file */
export const modelDirOf = (project, file) =>
  project.modelDirs.find((dir) => dir === '.' || file.startsWith(`${dir}/`))

/** The deepest `+materialized` and `+schema` dbt_project.yml sets over a model's folders. */
/** @param {Project} project @param {string} file */
export function folderConfig(project, file) {
  const dir = modelDirOf(project, file)
  const folders = path.posix
    .dirname(path.posix.relative(dir ?? project.root, file))
    .split('/')
    .filter((part) => part && part !== '.')
  /** @type {{materialized?: string, schema?: string}} */
  const found = {}
  let node = project.folders

  for (const folder of [null, ...folders]) {
    if (folder !== null) node = node?.[folder]
    if (!node || typeof node !== 'object') break
    if (node['+materialized']) found.materialized = node['+materialized']
    if (node['+schema']) found.schema = node['+schema']
  }

  return found
}

/** The `materialized=` and `schema=` a model's own config() sets. */
/** @param {string} sql */
export function modelConfig(sql) {
  const call = sql.match(/config\s*\(([\s\S]*?)\)\s*-?}}/)
  /** @param {string} key */
  const setting = (key) => call?.[1].match(new RegExp(`\\b${key}\\s*=\\s*['"]([\\w-]+)['"]`))?.[1]

  return { materialized: setting('materialized'), schema: setting('schema') }
}

/** Every model entry in one schema.yml: name -> {columns, materialized?}. */
/** @param {string} text @returns {Map<string, {columns: string[], materialized?: string}>} */
export function schemaEntries(text) {
  const entries = new Map()
  const models = parseYaml(text)?.models

  for (const model of Array.isArray(models) ? models : []) {
    if (!model?.name) continue

    entries.set(String(model.name), {
      columns: (model.columns ?? [])
        .map((column) => column?.name)
        .filter(Boolean)
        .map(String),
      materialized: model.config?.materialized,
    })
  }

  return entries
}

/**
 * `sql` with every comment, Jinja tag and string literal body blanked to spaces, so a scan for
 * keywords, parentheses and commas sees only SQL structure; offsets and lines are unchanged.
 */
/** @param {string} sql */
export function maskSql(sql) {
  const out = sql.split('')
  /** @param {number} from @param {number} to */
  const blank = (from, to) => {
    for (let index = from; index < to; index += 1) {
      if (out[index] !== '\n') out[index] = ' '
    }
  }
  const pairs = [
    ['--', '\n'],
    ['/*', '*/'],
    ['{#', '#}'],
    ['{%', '%}'],
    ['{{', '}}'],
  ]
  let index = 0

  while (index < sql.length) {
    const pair = pairs.find(([open]) => sql.startsWith(open, index))

    if (pair) {
      const close = sql.indexOf(pair[1], index + pair[0].length)
      const end = close < 0 ? sql.length : close + pair[1].length

      blank(index, end)
      // A Jinja expression stays one opaque token, so `{{ ... }} as name` still names a column.
      if (pair[0] === '{{') out[index] = '?'
      index = end
    } else if (sql[index] === "'") {
      let end = index + 1

      while (end < sql.length) {
        if (sql[end] === "'" && sql[end + 1] === "'") end += 2
        else if (sql[end] === "'") break
        else end += 1
      }

      blank(index + 1, end)
      index = end + 1
    } else {
      index += 1
    }
  }

  return out.join('')
}

/** Identifiers at parenthesis depth 0 within [start, end), with their offsets. */
/** @param {string} masked @param {number} start @param {number} end */
function topWords(masked, start, end) {
  const words = []
  let depth = 0

  for (let index = start; index < end; index += 1) {
    const char = masked[index]

    if (char === '(') depth += 1
    else if (char === ')') depth -= 1
    else if (depth === 0 && /[A-Za-z_]/.test(char)) {
      const word = masked.slice(index).match(/^[A-Za-z_][\w$]*/)[0]

      if (index === start || !/[\w$.]/.test(masked[index - 1])) {
        words.push({ index, word: word.toLowerCase() })
      }

      index += word.length - 1
    }
  }

  return words
}

/** The offset of the `)` closing the `(` at `open`. */
/** @param {string} masked @param {number} open */
function closing(masked, open) {
  let depth = 0

  for (let index = open; index < masked.length; index += 1) {
    if (masked[index] === '(') depth += 1
    else if (masked[index] === ')' && (depth -= 1) === 0) return index
  }

  return masked.length
}

/** [start, end) ranges split at depth-0 commas. */
/** @param {string} masked @param {number} start @param {number} end */
function splitTop(masked, start, end) {
  const parts = []
  let depth = 0
  let from = start

  for (let index = start; index < end; index += 1) {
    if (masked[index] === '(') depth += 1
    else if (masked[index] === ')') depth -= 1
    else if (masked[index] === ',' && depth === 0) {
      parts.push([from, index])
      from = index + 1
    }
  }

  parts.push([from, end])

  return parts
}

/** Naming conventions used only when SQL has no explicit cast. */
/** @type {[RegExp, string][]} */
const NAME_TYPES = [
  [/_precise_cents$/, 'numeric(16,4)'],
  [/_cents$/, 'bigint'],
  [/_at$/, 'timestamptz'],
  [/_on$/, 'date'],
  [/^(is|has)_/, 'boolean'],
  [/^num_/, 'bigint'],
  [/^(rate|percent)_/, 'numeric'],
]

/** An expression's type: its outermost cast, else the naming convention, else unknown. */
/** @param {string} masked @param {number} start @param {number} end @param {string} name */
function expressionType(masked, start, end, name) {
  const expression = masked.slice(start, end).trim()
  const cast = expression.match(/^cast\s*\(([\s\S]*)\)$/i)

  if (cast && closing(expression, expression.indexOf('(')) === expression.length - 1) {
    const separator = topWords(cast[1], 0, cast[1].length).find((word) => word.word === 'as')
    const typed = separator ? cast[1].slice(separator.index + 2) : undefined

    if (typed) return typed.trim().replace(/\s+/g, ' ').toLowerCase()
  }

  let depth = 0
  let lastCast = -1
  let compound = false

  for (let index = start; index < end - 1; index += 1) {
    if (masked[index] === '(') depth += 1
    else if (masked[index] === ')') depth -= 1
    else if (depth === 0 && masked.startsWith('::', index)) lastCast = index
    else if (depth === 0 && /[+\-*/%<>=!|&^]/.test(masked[index])) compound = true
  }

  const operatorWords = new Set([
    'and',
    'or',
    'not',
    'is',
    'like',
    'ilike',
    'in',
    'between',
    'similar',
    'at',
    'collate',
  ])
  if (
    lastCast >= 0 &&
    !compound &&
    !topWords(masked, start, lastCast).some(({ word }) => operatorWords.has(word))
  ) {
    const typed = masked
      .slice(lastCast + 2, end)
      .trim()
      .match(
        /^[A-Za-z][\w.]*(?:\s+(?:precision|varying|(?:with|without)\s+time\s+zone))?\s*(?:\([\d, ]+\))?(?:\[\])*$/i,
      )?.[0]

    if (typed) return typed.trim().replace(/\s+/g, ' ').toLowerCase()
  }

  return nameType(name)
}

const IDENT = String.raw`("?)([A-Za-z_][\w$]*)\1`

/**
 * The columns a model's SQL selects: `{columns: [{name, type}], resolved}`, where
 * `resolved` is false when a `*` could not be followed into a CTE (a `ref()`, a join) or a
 * Jinja statement writes part of the list.
 */
/** @param {string} sql @returns {Selected} */
export function selectColumns(sql) {
  const masked = maskSql(sql)

  /** @param {number} start @param {number} end @param {Map<string, number[]>} ctes @param {number} depth @returns {Selected} */
  const query = (start, end, ctes, depth) => {
    const words = topWords(masked, start, end)
    let cursor = 0

    if (words[0]?.word === 'with') {
      ctes = new Map(ctes)

      for (;;) {
        const name = masked
          .slice(words[cursor + 1]?.index ?? end, end)
          .match(new RegExp(`^${IDENT}`))

        if (!name) break

        const open = masked.indexOf('(', words[cursor + 1].index)

        if (open < 0) break

        const close = closing(masked, open)

        ctes.set(name[2].toLowerCase(), [open + 1, close])

        const after = masked.slice(close + 1, end).match(/^\s*,/)

        cursor = words.findIndex((word) => word.index > close)
        if (!after || cursor < 0) break
        cursor -= 1
      }
    }

    const select = words.findIndex((word, index) => index >= cursor && word.word === 'select')

    if (select < 0) return { columns: [], resolved: false }

    const tail = words.slice(select + 1)
    const stop = tail.find((word) =>
      ['from', 'union', 'intersect', 'except', 'where'].includes(word.word),
    )
    let listStart = words[select].index + 'select'.length
    const listEnd = stop?.index ?? end
    const distinct = masked.slice(listStart, listEnd).match(/^\s*distinct\b(\s*on\s*\()?/i)

    if (distinct) {
      listStart += distinct[0].length
      if (distinct[1]) listStart = closing(masked, listStart - 1) + 1
    }

    const fromTarget =
      stop?.word === 'from'
        ? masked
            .slice(stop.index + 4, end)
            .match(new RegExp(`^\\s*${IDENT}`))?.[2]
            ?.toLowerCase()
        : undefined
    const joined = tail.some((word) => word.word === 'join')
    const columns = []
    // A Jinja statement in the list (a `{% for %}` over stages, say) makes columns no scan sees.
    let resolved = !sql.slice(listStart, listEnd).includes('{%')

    for (const [from, to] of splitTop(masked, listStart, listEnd)) {
      const text = masked.slice(from, to)
      const offset = from + text.search(/\S|$/)
      const expression = text.trim()
      const alias = expression.match(new RegExp(`(?:^|\\s)as\\s+${IDENT}\\s*$`, 'i'))
      const bare = expression.match(new RegExp(`^(?:${IDENT}\\.)?("?)([A-Za-z_][\\w$]*)\\3$`))
      const star = expression.match(/^(?:"?([A-Za-z_][\w$]*)"?\.)?\*$/)

      if (star) {
        const source = (star[1] ?? (joined ? undefined : fromTarget))?.toLowerCase()
        const cte = source ? ctes.get(source) : undefined

        if (cte && depth < 8) {
          const inner = query(cte[0], cte[1], ctes, depth + 1)

          columns.push(...inner.columns)
          resolved &&= inner.resolved
        } else {
          resolved = false
        }

        continue
      }

      const name = alias
        ? alias[1]
          ? alias[2]
          : alias[2].toLowerCase()
        : bare
          ? bare[3]
            ? bare[4]
            : bare[4].toLowerCase()
          : undefined

      if (!name) {
        resolved = false
        continue
      }

      const nameOffset = alias ? offset + expression.length - alias[0].trimStart().length : offset

      columns.push({
        name,
        type: expressionType(masked, offset, alias ? nameOffset : offset + expression.length, name),
      })
    }

    return { columns, resolved }
  }

  return query(0, masked.length, new Map(), 0)
}

/** @param {string} column */
export const nameType = (column) =>
  NAME_TYPES.find(([pattern]) => pattern.test(column))?.[1] ?? UNKNOWN_TYPE
