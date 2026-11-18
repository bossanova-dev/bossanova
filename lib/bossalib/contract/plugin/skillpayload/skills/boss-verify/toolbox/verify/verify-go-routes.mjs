// Lexical route facts: imports enable a framework; dynamic paths remain unknown.
import { LABEL_CHARS, ROUTE_METHODS } from './verify-describe-blocks.mjs'

/** @typedef {{text: string, value?: string, start: number, end: number, line: number}} Token */
/** @typedef {{method: string, path: string, handler: string, auth: string, line: number, file?: string, namespace?: string}} Route */
/** @param {string} text @param {number} max */
export const shortenGoText = (text, max) => {
  const clean = text.replace(/[\r\n\u2028\u2029]+/g, ' ').trim()
  return clean.length > max ? `${clean.slice(0, max - 1)}…` : clean
}

/** Tokens retain literal values separately, so strings never become executable syntax.
 * @param {string} source @returns {Token[]}
 */
export function goTokens(source) {
  /** @type {Token[]} */
  const tokens = []
  const pattern =
    /\/\/[^\n]*|\/\*[\s\S]*?\*\/|"(?:\\.|[^"\\])*"|`[^`]*`|'(?:\\.|[^'\\])*'|[\p{L}_][\p{L}\p{N}_]*|:=|[^\s]/gu
  let line = 1,
    end = 0
  for (const match of source.matchAll(pattern)) {
    line += (source.slice(end, match.index).match(/\n/g) ?? []).length
    const text = match[0]
    if (!text.startsWith('//') && !text.startsWith('/*')) {
      let value
      if (text.startsWith('`')) value = text.slice(1, -1)
      if (text.startsWith('"')) {
        try {
          value = JSON.parse(text)
        } catch {
          value = undefined
        }
      }
      tokens.push({
        text: value === undefined ? text : 'string',
        value,
        start: match.index,
        end: match.index + text.length,
        line,
      })
    }
    line += (text.match(/\n/g) ?? []).length
    end = match.index + text.length
  }
  return tokens
}

/** Blank comments and all literals, preserving offsets and lines. @param {string} source */
export function blankGoNoise(source) {
  /** @type {string[]} */
  const chars = source.split('').map((c) => (c === '\n' ? '\n' : ' '))
  for (const token of goTokens(source)) {
    if (token.value !== undefined || token.text.startsWith("'")) continue
    for (let i = token.start; i < token.end; i++) chars[i] = source[i]
  }
  return chars.join('')
}

/** @param {Token[]} tokens @param {number} start */
function closing(tokens, start) {
  const pairs = { '(': ')', '{': '}', '[': ']' }
  const want = pairs[tokens[start]?.text]
  if (!want) return start
  let depth = 1
  for (let i = start + 1; i < tokens.length; i++) {
    if (tokens[i].text === tokens[start].text) depth++
    if (tokens[i].text === want) {
      depth--
      if (depth === 0) return i
    }
  }
  return tokens.length - 1
}

/** @param {Token[]} tokens @param {number} start @param {number} end */
function argumentsAt(tokens, start, end) {
  /** @type {Token[][]} */
  const args = []
  let begin = start
  for (let i = start; i < end; i++) {
    if (['(', '{', '['].includes(tokens[i].text)) i = closing(tokens, i)
    else if (tokens[i].text === ',') {
      args.push(tokens.slice(begin, i))
      begin = i + 1
    }
  }
  args.push(tokens.slice(begin, end))
  return args
}

/** First matching selector, displayed by its last segment.
 * @param {Token[]} tokens @param {boolean} [callsOnly]
 */
function authName(tokens, callsOnly = false) {
  for (let i = 0; i < tokens.length; i++) {
    if (tokens[i].text === 'func') break
    if (
      !/^[\p{L}_]/u.test(tokens[i].text) ||
      !/auth|jwt|session|token|login|require/i.test(tokens[i].text)
    )
      continue
    while (tokens[i + 1]?.text === '.' && tokens[i + 2]) i += 2
    if (callsOnly && tokens[i + 1]?.text !== '(') continue
    return tokens[i].text === 'none' ? '' : shortenGoText(tokens[i].text, LABEL_CHARS)
  }
  return ''
}

/** A handler constructor is not middleware evidence. Only inspect a call around an
 * explicit handler-shaped argument; other expressions remain unknown.
 * @param {Token[]} tokens
 */
function wrapperAuth(tokens) {
  const open = tokens.findIndex((t) => t.text === '(')
  if (open < 1 || closing(tokens, open) !== tokens.length - 1) return ''
  const args = argumentsAt(tokens, open + 1, tokens.length - 1)
  const nestedAuth = args.map(wrapperAuth).find(Boolean) || ''
  const wrapsHandler = args.some(
    (arg) =>
      arg[0]?.text === 'func' ||
      (arg.length === 1 && /^(?:h|handler|next|serve)$/i.test(arg[0].text)),
  )
  return wrapsHandler || nestedAuth ? authName(tokens.slice(0, open)) || nestedAuth : ''
}

/** @param {string} prefix @param {string} suffix */
const joined = (prefix, suffix) =>
  prefix ? `${prefix.replace(/\/$/, '')}/${suffix.replace(/^\//, '')}` : suffix

/** Named functions receive routers with caller-owned prefixes and middleware.
 * @param {Token[]} tokens @param {number} start @param {Set<string>} routerTypes
 */
function routerHelperBody(tokens, start, routerTypes) {
  let name = start + 1
  if (tokens[name]?.text === '(') name = closing(tokens, name) + 1
  if (!/^[\p{L}_][\p{L}\p{N}_]*$/u.test(tokens[name]?.text ?? '')) return -1
  let open = name + 1
  if (tokens[open]?.text === '[') open = closing(tokens, open) + 1
  if (tokens[open]?.text !== '(') return -1
  const end = closing(tokens, open)
  if (!tokens.slice(open + 1, end).some((token) => routerTypes.has(token.text))) return -1
  for (let body = end + 1; body < tokens.length; body++) {
    if (['(', '['].includes(tokens[body].text)) body = closing(tokens, body)
    else if (tokens[body].text === '{') {
      if (['struct', 'interface'].includes(tokens[body - 1]?.text)) body = closing(tokens, body)
      else return body
    }
  }
  return -1
}

/** @param {string} source @returns {Route[]} */
export function goRoutes(source) {
  const tokens = goTokens(source)
  const imports = new Set()
  for (let i = 0; i < tokens.length; i++) {
    if (tokens[i].text !== 'import') continue
    const end =
      tokens[i + 1]?.text === '(' ? closing(tokens, i + 1) : i + (tokens[i + 1]?.value ? 1 : 2)
    for (let j = i + 1; j <= end; j++) if (tokens[j]?.value) imports.add(tokens[j].value)
  }
  const http = imports.has('net/http')
  const chi = imports.has('github.com/go-chi/chi') || imports.has('github.com/go-chi/chi/v5')
  const gin = imports.has('github.com/gin-gonic/gin')
  if (!http && !chi && !gin) return []
  const routerTypes = new Set([
    ...(http ? ['ServeMux'] : []),
    ...(chi ? ['Router', 'Mux'] : []),
    ...(gin ? ['RouterGroup', 'Engine', 'IRouter', 'IRoutes'] : []),
  ])
  /** @type {Route[]} */
  const routes = []
  /** @typedef {{prefix: string, auth: string}} Context */
  /** @param {Token[]} part @param {Map<string, Context>} inherited @param {Context} context */
  function walk(part, inherited, context) {
    const vars = new Map(inherited)
    /** @type {Map<number, {context: Context, start: number}>} */
    const groupCalls = new Map()
    const scopeAuth = context.auth
    for (let i = 0; i < part.length; i++) {
      if (part[i].text === 'func') {
        const body = routerHelperBody(part, i, routerTypes)
        if (body >= 0) {
          i = closing(part, body)
          continue
        }
      }
      if (part[i].text === '{') {
        const end = closing(part, i)
        walk(part.slice(i + 1, end), vars, { ...context, auth: scopeAuth })
        i = end
        continue
      }
      if (part[i].text !== '.' || part[i + 2]?.text !== '(') continue
      const name = part[i + 1].text,
        open = i + 2,
        end = closing(part, open)
      const args = argumentsAt(part, open + 1, end)
      let receiver = part[i - 1]?.text ?? ''
      let chainAuth = ''
      // A With(...) chain keeps its router's prefix and names its middleware.
      let receiverIndex = i - 1
      /** @type {Token[]} */
      let chain = []
      while (part[receiverIndex]?.text === ')') {
        let openIndex = receiverIndex,
          depth = 1
        while (--openIndex >= 0) {
          if (part[openIndex].text === ')') depth++
          if (part[openIndex].text === '(') {
            depth--
            if (depth === 0) break
          }
        }
        if (part[openIndex - 1]?.text !== 'With' || part[openIndex - 2]?.text !== '.') break
        chain = [...part.slice(openIndex + 1, receiverIndex), ...chain]
        receiverIndex = openIndex - 3
        receiver = part[receiverIndex]?.text ?? ''
      }
      chainAuth = authName(chain)
      const priorGroup = groupCalls.get(part[receiverIndex]?.start)
      let receiverStart = priorGroup?.start ?? receiverIndex
      while (part[receiverStart - 1]?.text === '.') receiverStart -= 2
      if (!priorGroup)
        receiver = part
          .slice(receiverStart, receiverIndex + 1)
          .map((t) => t.text)
          .join('')
      const ctx = priorGroup?.context ?? vars.get(receiver) ?? { ...context, auth: scopeAuth }
      const first = args[0]?.length === 1 ? args[0][0].value : undefined
      if (name === 'Use' && (chi || gin)) {
        const auth = authName(args.flat())
        if (auth) vars.set(receiver, { ...ctx, auth })
        i = end
        continue
      }
      if (
        chi &&
        ((name === 'Route' && first?.startsWith('/')) ||
          (name === 'Group' && args[0]?.[0]?.text === 'func'))
      ) {
        const callback = args[name === 'Route' ? 1 : 0] ?? []
        const body = callback.findIndex((t) => t.text === '{')
        if (body >= 0) {
          const child = {
            prefix: name === 'Route' ? joined(ctx.prefix, first) : ctx.prefix,
            auth: chainAuth || ctx.auth,
          }
          const childVars = new Map(vars)
          const param = callback[2]?.text
          if (param) childVars.set(param, child)
          walk(callback.slice(body + 1, closing(callback, body)), childVars, child)
        }
        i = end
        continue
      }
      if (name === 'Group' && gin && first?.startsWith('/')) {
        const child = {
          prefix: joined(ctx.prefix, first),
          auth: authName(args.slice(1).flat()) || ctx.auth,
        }
        groupCalls.set(part[end].start, { context: child, start: receiverStart })
        if ([':=', '='].includes(part[receiverStart - 1]?.text))
          vars.set(part[receiverStart - 2].text, child)
        i = end
        continue
      }
      let method = '',
        routePath = first,
        handlerArgs = args.slice(1)
      if ((http || chi) && ['Handle', 'HandleFunc'].includes(name)) method = 'ANY'
      if (chi && ['Get', 'Head', 'Post', 'Put', 'Patch', 'Delete', 'Options'].includes(name))
        method = name.toUpperCase()
      if (gin && ROUTE_METHODS.includes(name)) method = name
      if (gin && name === 'Any') method = 'ANY'
      if (
        (chi && ['Method', 'MethodFunc'].includes(name)) ||
        (gin && name === 'Handle' && ROUTE_METHODS.includes(first))
      ) {
        method = first ?? ''
        routePath = args[1]?.length === 1 ? args[1][0].value : undefined
        handlerArgs = args.slice(2)
      }
      if (chi && name === 'Mount') method = 'ANY'
      if (method && routePath) {
        if ((http || chi) && ['Handle', 'HandleFunc'].includes(name)) {
          const pattern = routePath.match(/^([A-Z]+)\s+(.+)$/)
          if (pattern) {
            method = pattern[1]
            routePath = pattern[2]
          }
        }
        const hostPattern = http && ['Handle', 'HandleFunc'].includes(name) && first !== method
        const validPath = hostPattern
          ? /^(?:[^/\s]+)?\//.test(routePath)
          : routePath.startsWith('/')
        if (ROUTE_METHODS.includes(method) && validPath && handlerArgs.some((arg) => arg.length)) {
          const rawPath = joined(ctx.prefix, routePath) + (name === 'Mount' ? '/*' : '')
          const hTokens = handlerArgs.flat()
          const handler = hTokens.length
            ? source.slice(hTokens[0].start, hTokens.at(-1).end).replace(/\s+/g, ' ').trim()
            : ''
          const middleware = gin ? authName(handlerArgs.slice(0, -1).flat()) : ''
          routes.push({
            method,
            path: rawPath,
            handler: name === 'Mount' ? `mounted router: ${handler}` : handler,
            auth:
              chainAuth ||
              middleware ||
              wrapperAuth(handlerArgs.at(-1) ?? []) ||
              ctx.auth ||
              'not detected',
            line: part[i + 1].line,
          })
        }
      }
      // With must remain visible so the following chained registration can read it.
      if (name !== 'With') i = end
    }
  }
  walk(tokens, new Map(), { prefix: '', auth: '' })
  return routes
}

/** @param {Route[]} base @param {Route[]} head */
export function routeChanges(base, head) {
  const keyed = (/** @type {Route[]} */ routes) =>
    new Map(routes.map((r) => [JSON.stringify([r.namespace ?? '', r.method, r.path]), r]))
  const before = keyed(base),
    after = keyed(head)
  return [...new Set([...after.keys(), ...before.keys()])].flatMap((key) => {
    const old = before.get(key),
      next = after.get(key)
    if (old && next && old.handler === next.handler && old.auth === next.auth) return []
    return [{ ...(next ?? old), change: !old ? 'added' : !next ? 'removed' : 'changed' }]
  })
}
