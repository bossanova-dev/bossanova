/**
 * The HTML for the blocks an area's describers typed, in a fixed order: tables on one shared
 * column grid, the GraphQL schema read as field names and types in two columns, routes with their
 * auth, client calls as the operation they send and their call-site diff, then code excerpts.
 * Every value is escaped here.
 */
import { colourLine, escapeHtml as e, languageOf, proseHtml } from './verify-proof-code.mjs'

/**
 * @typedef {import('./verify-describe-blocks.mjs').Block} Block
 * @typedef {import('./verify-describe-blocks.mjs').SchemaType} SchemaType
 * @typedef {import('./verify-describe-blocks.mjs').SchemaField} SchemaField
 * @typedef {Readonly<Record<string, string>>} Tickets
 */

/** @type {Readonly<Record<string, string>>} */
const CHANGE_TONE = Object.freeze({ added: 'success', changed: 'warning', removed: 'danger' })
/** @type {Readonly<Record<string, string>>} */
const RELATION_WORD = Object.freeze({
  'dbt-model': 'dbt model',
  'materialized-view': 'materialized view',
})
/** A table hides its plain columns when it is new and has more than this many. */
const NEW_TABLE_COLUMNS = 6
const OPERATION_KEYWORD = /^(query|mutation|subscription|fragment)$/
/** @param {string} word @param {string} tone */
const pill = (word, tone) => `<span class="pill ${tone}">${e(word)}</span>`

/** A GraphQL type with its names coloured and its non-null marks set apart. @param {string} type */
const sdlType = (type) =>
  type.replace(/([A-Za-z_]\w*)|(!)|([^A-Za-z_!]+)/g, (_, name, bang, other) =>
    name ? `<span class="ty">${e(name)}</span>` : bang ? '<span class="nn">!</span>' : e(other),
  )

/** One field: its name and arguments, one per line when there are several, then its type. @param {SchemaField} field */
function fieldRow(field) {
  const args = field.args ?? []
  const arg = (/** @type {{name: string, type: string, default?: string}} */ a) =>
    `<span class="fn">${e(a.name)}</span><span class="pu">: </span>${sdlType(a.type)}${a.default ? `<span class="dr"> = ${e(a.default)}</span>` : ''}`
  const params = !args.length
    ? ''
    : args.length === 1
      ? `<span class="pu">(</span>${arg(args[0])}<span class="pu">)</span>`
      : `<span class="pu">(</span><span class="args">${args.map((a) => `<span>${arg(a)}</span>`).join('')}</span><span class="pu">)</span>`
  const was = field.before ? `<s class="was">${e(field.before)}</s> ` : ''
  const directives = field.directives?.length
    ? ` <span class="dr">${e(field.directives.join(' '))}</span>`
    : ''
  const mark = field.change === 'removed' ? ' rm' : field.change === 'added' ? ' ad' : ''
  return `<div class="fr${mark}"><span class="fnc"><span class="fn">${e(field.name)}</span>${params}</span><span class="ftc">${was}${field.type ? sdlType(field.type) : ''}${directives}</span></div>`
}

/** One type: its keyword, name and what changed, then its fields or values. @param {SchemaType} t @param {Tickets} tickets */
function schemaType(t, tickets) {
  const counts = ['added', 'changed', 'removed']
    .map((change) => [change, t.fields.filter((f) => f.change === change).length])
    .filter(([, n]) => n)
    .map(([change, n]) => `${n} ${change}`)
  const state =
    t.change === 'added'
      ? '<span class="new">new</span>'
      : t.change === 'removed'
        ? '<span class="gone">removed</span>'
        : counts.length
          ? `<span class="dim">${counts.join(', ')}</span>`
          : ''
  const flag =
    t.class === 'breaking'
      ? pill('breaking', 'danger')
      : t.class === 'dangerous'
        ? pill('risky', 'warning')
        : ''
  // A changed enum marks its new values; a new one needs no marks.
  const listed = t.keyword === 'enum' || t.keyword === 'union'
  const body = !t.fields.length
    ? ''
    : listed
      ? `<div class="enumvs">${t.fields.map((f) => `<span class="enumv${f.change === 'removed' ? ' rm' : f.change === 'added' && t.change === 'changed' ? ' ad' : ''}">${e(f.name)}</span>`).join('')}</div>`
      : `<div class="fields">${t.fields.map((f) => fieldRow(t.change === 'changed' ? f : { ...f, change: undefined })).join('')}</div>`
  const mobile = t.mobile
    ? `<div class="mob"><b>Old mobile builds</b><span>${proseHtml(t.mobile, tickets)}</span></div>`
    : ''
  return `<div class="sb${t.change === 'removed' ? ' rm' : ''}"><div class="sbh"><span class="kw">${e(t.keyword)}</span><span class="tn">${e(t.name)}</span>${state}${flag}</div>${body}${mobile}</div>`
}

/**
 * One GraphQL document as lines, one selection per line and indented by depth. Names that belong
 * together stay on one line: a keyword and its name, an alias and its field, a spread and its
 * type condition, a field and its directives.
 * @param {string} text
 */
export function operationLines(text) {
  const tokens =
    text
      .replace(/#[^\n]*/g, '')
      .replace(/\s+/g, ' ')
      .replace(/ \(/g, '(')
      .match(/[^\s{}(]+(?:\([^)]*\))?|\{|\}/g) ?? []
  /** @type {string[]} */
  const lines = []
  let current = ''
  let depth = 0
  const pad = () => '  '.repeat(Math.max(0, depth))
  for (const token of tokens) {
    if (token === '{') {
      lines.push(`${pad()}${current}${current ? ' ' : ''}{`)
      current = ''
      depth++
    } else if (token === '}') {
      if (current) lines.push(pad() + current)
      current = ''
      depth--
      lines.push(`${pad()}}`)
    } else {
      const last = current.split(' ').at(-1) ?? ''
      const joins =
        OPERATION_KEYWORD.test(last) ||
        last === 'on' ||
        last === '...' ||
        last.endsWith(':') ||
        token === 'on' ||
        token.startsWith('@')
      if (current && !joins) {
        lines.push(pad() + current)
        current = token
      } else current = current ? `${current} ${token}` : token
    }
  }
  if (current) lines.push(pad() + current)
  return lines
}

/** @param {string} line */
const operationLine = (line) =>
  e(line)
    .replace(/^(\s*)(query|mutation|subscription|fragment)\b/, '$1<span class="kw">$2</span>')
    .replace(/(\$\w+)/g, '<span class="nn">$1</span>')
    .replace(/(:\s*\[?)([A-Z]\w*)/g, '$1<span class="ty">$2</span>')

/** @param {Extract<Block, {kind: 'client-call'}>} call @param {Tickets} tickets */
function clientCall(call, tickets) {
  const operation = call.operation
    ? `<pre class="gql">${operationLines(call.operation).map(operationLine).join('\n')}</pre>`
    : ''
  const site = [
    ...(call.before ?? []).map((line) => `<span class="dl">- ${e(line)}</span>`),
    ...(call.after ?? []).map((line) => `<span class="al">+ ${e(line)}</span>`),
  ]
  const path = call.path
    ? `<code class="loc" title="${e(call.path)}">${e(call.path.slice(call.path.lastIndexOf('/') + 1))}</code>`
    : ''
  return `<div class="cc"><div class="cch"><span class="tn">${e(call.name)}</span>${path}</div>${operation}${site.length ? `<pre class="gql">${site.join('\n')}</pre>` : ''}${call.note ? `<p class="note">${proseHtml(call.note, tickets)}</p>` : ''}</div>`
}

/** @param {Extract<Block, {kind: 'route'}>} route @param {Tickets} tickets */
const routeRow = (route, tickets) =>
  `<div class="rt${route.change === 'removed' ? ' rm' : ''}"><span class="mt">${e(route.method)}</span><span class="rp"><code>${e(route.path)}</code>${route.change === 'added' ? '' : ` <span class="ch">${e(route.change)}</span>`}${route.note ? `<span class="dim">${proseHtml(route.note, tickets)}</span>` : ''}</span>${route.auth === 'none' ? pill('no auth', 'danger') : pill(route.auth, 'muted')}</div>`

/**
 * A table on the shared column grid. A new table with many columns lists only the ones flagged as
 * personal data or annotated, and names the rest on one line.
 * @param {Extract<Block, {kind: 'table'}>} t @param {Tickets} tickets
 */
function table(t, tickets) {
  const fresh = t.change === 'added'
  const long = fresh && t.columns.length > NEW_TABLE_COLUMNS
  const marked = (/** @type {typeof t.columns[number]} */ c) => c.pii || c.note
  const shown = long ? t.columns.filter(marked) : t.columns
  const rest = long ? t.columns.filter((c) => !marked(c)) : []
  const rows = shown
    .map(
      (c) =>
        `<tr${c.change === 'removed' ? ' class="rm"' : ''}><td class="cn">${e(c.name)}${c.pii ? '<span class="pii">PII</span>' : ''}</td><td class="ct">${e(c.type)}${c.constraints ? ` · ${e(c.constraints)}` : ''}</td><td class="cx">${!fresh && c.change ? pill(c.change, CHANGE_TONE[c.change]) : ''}${c.note ? `<span>${proseHtml(c.note, tickets)}</span>` : ''}</td></tr>`,
    )
    .join('')
  const also = rest.length
    ? `<tr class="also"><td colspan="3">Also <span>${rest.map((c) => e(c.name)).join(', ')}</span></td></tr>`
    : ''
  const relation = RELATION_WORD[t.relation] ?? t.relation
  const kind = t.materialization ? `${relation} · ${t.materialization}` : relation
  return `<div class="tbox"><div class="tbh"><span class="nm">${e(t.name)}</span><span class="dim">${e(kind)}</span>${pill(t.change, CHANGE_TONE[t.change])}</div>${rows || also ? `<table class="cols"><colgroup><col class="cn"><col class="ct"><col></colgroup>${rows}${also}</table>` : ''}</div>`
}

/** @param {Extract<Block, {kind: 'code'}>} c @param {Tickets} tickets */
function code(c, tickets) {
  const lang = languageOf(c.path)
  const rows = c.lines
    .map(
      (line, i) =>
        `<tr><td class="n">${c.start + i}</td><td class="t">${colourLine(line, lang)}</td></tr>`,
    )
    .join('')
  return `${c.title ? `<p class="btitle">${proseHtml(c.title, tickets)}</p>` : ''}<figure class="code excerpt"><figcaption><span class="path" title="${e(c.path)}">${e(c.path)}:${c.start}</span></figcaption><div class="scroll"><table>${rows}</table></div></figure>`
}

/**
 * The HTML for an area's blocks. Steps are not here: they join After merge.
 * @param {Block[]} blocks @param {Tickets} tickets
 */
export function blocksHtml(blocks, tickets) {
  /** @template {Block['kind']} K @param {K} kind @returns {Extract<Block, {kind: K}>[]} */
  const of = (kind) => /** @type {any[]} */ (blocks.filter((b) => b.kind === kind))
  const routes = of('route')
  const calls = of('client-call')
  return [
    ...of('table').map((t) => table(t, tickets)),
    ...of('schema').map(
      (s) => `<div class="schema">${s.types.map((t) => schemaType(t, tickets)).join('')}</div>`,
    ),
    routes.length
      ? `<h3 class="sub">Routes</h3><div class="routes">${routes.map((r) => routeRow(r, tickets)).join('')}</div>`
      : '',
    calls.length
      ? `<h3 class="sub">Client calls</h3><div class="calls">${calls.map((c) => clientCall(c, tickets)).join('')}</div>`
      : '',
    ...of('code').map((c) => code(c, tickets)),
  ].join('')
}
