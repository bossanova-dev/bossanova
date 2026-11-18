// A conservative SDL reader: unsupported or malformed syntax fails closed at the caller.
/**
 * @typedef {{text: string, kind: 'name'|'number'|'string'|'punct'}} Token
 * @typedef {{type: string, defaultValue?: string, args: Record<string, Field>, directives?: string[]}} Field
 * @typedef {{kind: string, fields: Record<string, Field>, members: string[], interfaces: string[], repeatable?: boolean}} Definition
 * @typedef {{types: Record<string, Definition>, roots: Record<string, string>}} GraphqlSchema
 */

/** @param {string} source @returns {Token[]} */
function tokenize(source) {
  /** @type {Token[]} */
  const tokens = []
  let offset = 0
  while (offset < source.length) {
    const rest = source.slice(offset)
    const trivia = /^(?:[\s,\uFEFF]+|#[^\r\n]*)/.exec(rest)
    if (trivia) {
      offset += trivia[0].length
      continue
    }
    let text = ''
    /** @type {Token['kind']} */
    let kind = 'punct'
    if (rest.startsWith('"""')) {
      let end = 3
      while (end < rest.length && !(rest.startsWith('"""', end) && rest[end - 1] !== '\\')) end++
      if (end === rest.length) throw new Error('Unterminated block string')
      text = rest.slice(0, end + 3)
      kind = 'string'
    } else if (rest[0] === '"') {
      const match = /^"(?:[^"\\\r\n\x00-\x1f]|\\(?:["\\/bfnrt]|u[0-9a-fA-F]{4}))*"/.exec(rest)
      if (!match) throw new Error('Invalid string')
      text = match[0]
      kind = 'string'
    } else if (/^[_A-Za-z]/.test(rest)) {
      text = /^[_A-Za-z][_0-9A-Za-z]*/.exec(rest)?.[0] ?? ''
      kind = 'name'
    } else if (/^[-0-9]/.test(rest)) {
      text = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/.exec(rest)?.[0] ?? ''
      if (!text || /[._0-9A-Za-z]/.test(rest[text.length] ?? '')) throw new Error('Invalid number')
      kind = 'number'
    } else if (rest.startsWith('...')) text = '...'
    else if ('!$&():=@[]{|}'.includes(rest[0])) text = rest[0]
    else throw new Error(`Unexpected character at ${offset}`)
    tokens.push({ text, kind })
    offset += text.length
  }
  return tokens
}

/** Repeated definitions may contribute fields/arguments, but contradictory declarations are unreadable.
 * @param {Record<string, Field>} fields @param {string} name @param {Field} field */
function mergeField(fields, name, field) {
  const prior = fields[name]
  if (!prior) {
    fields[name] = field
    return
  }
  if (prior.type !== field.type || prior.defaultValue !== field.defaultValue)
    throw new Error(`Conflicting field ${name}`)
  for (const [argument, value] of Object.entries(field.args))
    mergeField(prior.args, argument, value)
}

class Reader {
  /** @param {Token[]} tokens */
  constructor(tokens) {
    this.tokens = tokens
    this.index = 0
  }
  peek() {
    return this.tokens[this.index]?.text ?? ''
  }
  /** @param {string} text */
  take(text) {
    if (this.peek() !== text) return false
    this.index++
    return true
  }
  /** @param {string} text */
  need(text) {
    if (!this.take(text)) throw new Error(`Expected ${text}, got ${this.peek() || 'end'}`)
  }
  name() {
    const token = this.tokens[this.index++]
    if (token?.kind !== 'name') throw new Error('Expected name')
    return token.text
  }
  description() {
    if (this.tokens[this.index]?.kind === 'string') this.index++
  }
  /** @returns {string} */
  type() {
    let type
    if (this.take('[')) {
      type = `[${this.type()}]`
      this.need(']')
    } else type = this.name()
    return type + (this.take('!') ? '!' : '')
  }
  /** Validate a value recursively and retain token spelling (including string whitespace).
   * @param {boolean} [variables] @returns {string} */
  value(variables = false) {
    const start = this.index
    if (this.take('$')) {
      if (!variables) throw new Error('Variable in constant value')
      this.name()
    } else if (this.take('[')) {
      while (!this.take(']')) this.value(variables)
    } else if (this.take('{')) {
      while (!this.take('}')) {
        this.name()
        this.need(':')
        this.value(variables)
      }
    } else {
      const token = this.tokens[this.index++]
      if (!token || token.kind === 'punct') throw new Error('Expected value')
    }
    return this.tokens
      .slice(start, this.index)
      .map((token) => token.text)
      .join(' ')
  }
  /** @param {boolean} [variables] @returns {string[]} */
  directives(variables = false) {
    const directives = []
    while (this.take('@')) {
      const name = this.name()
      const args = this.arguments(variables)
      directives.push(`@${name}${args ? `(${args})` : ''}`.replace(/\s+/g, ' '))
    }
    return directives
  }
  /** @param {boolean} variables */
  arguments(variables) {
    if (!this.take('(')) return ''
    const args = []
    if (this.peek() === ')') throw new Error('Empty arguments')
    do {
      const name = this.name()
      this.need(':')
      args.push(`${name}: ${this.printValue(variables)}`)
    } while (!this.take(')'))
    return args.join(', ')
  }
  /** Display values without changing the spelling retained for default comparisons.
   * @param {boolean} [variables] @returns {string} */
  printValue(variables = false) {
    if (this.take('[')) {
      const entries = []
      while (!this.take(']')) entries.push(this.printValue(variables))
      return `[${entries.join(', ')}]`
    }
    if (this.take('{')) {
      const entries = []
      while (!this.take('}')) {
        const name = this.name()
        this.need(':')
        entries.push(`${name}: ${this.printValue(variables)}`)
      }
      return `{${entries.join(', ')}}`
    }
    return this.value(variables)
  }
  /** @returns {Field} */
  input() {
    this.need(':')
    const type = this.type()
    const defaultValue = this.take('=') ? this.value() : undefined
    const directives = this.directives()
    return {
      type,
      args: Object.create(null),
      ...(directives.length ? { directives } : {}),
      ...(defaultValue === undefined ? {} : { defaultValue }),
    }
  }
  /** @returns {Record<string, Field>} */
  inputs() {
    /** @type {Record<string, Field>} */
    const args = Object.create(null)
    if (!this.take('(')) return args
    if (this.peek() === ')') throw new Error('Empty argument definitions')
    do {
      this.description()
      const name = this.name()
      args[name] = this.input()
      delete args[name].directives
    } while (!this.take(')'))
    return args
  }
  /** @param {Definition} definition */
  fields(definition) {
    this.need('{')
    if (this.peek() === '}') throw new Error('Empty fields')
    do {
      this.description()
      const name = this.name()
      if (definition.kind === 'input') mergeField(definition.fields, name, this.input())
      else {
        const args = this.inputs()
        this.need(':')
        const type = this.type()
        const directives = this.directives()
        mergeField(definition.fields, name, {
          type,
          args,
          ...(directives.length ? { directives } : {}),
        })
      }
    } while (!this.take('}'))
  }
  selection() {
    this.need('{')
    if (this.peek() === '}') throw new Error('Empty selection')
    do {
      if (this.take('...')) {
        if (this.take('on')) this.name()
        else if (this.peek() !== '@' && this.peek() !== '{') {
          this.name()
          this.directives(true)
          continue
        }
        this.directives(true)
        this.selection()
      } else {
        this.name()
        if (this.take(':')) this.name()
        this.arguments(true)
        this.directives(true)
        if (this.peek() === '{') this.selection()
      }
    } while (!this.take('}'))
  }
  /** @param {string} kind */
  executable(kind) {
    if (kind === 'fragment') {
      this.name()
      this.need('on')
      this.name()
    } else {
      if (this.tokens[this.index]?.kind === 'name') this.name()
      if (this.take('(')) {
        if (this.peek() === ')') throw new Error('Empty variables')
        do {
          this.need('$')
          this.name()
          this.input()
        } while (!this.take(')'))
      }
    }
    this.directives(true)
    this.selection()
  }
  /** @param {GraphqlSchema} schema */
  definition(schema) {
    this.description()
    if (this.peek() === '{') {
      this.selection()
      return
    }
    const extended = this.take('extend')
    const kind = this.name()
    if (['query', 'mutation', 'subscription', 'fragment'].includes(kind)) {
      if (extended) throw new Error('Cannot extend executable definition')
      this.executable(kind)
      return
    }
    if (kind === 'schema') {
      const start = this.index
      this.directives()
      if (this.take('{')) {
        if (this.peek() === '}') throw new Error('Empty schema')
        do {
          const operation = this.name()
          if (!['query', 'mutation', 'subscription'].includes(operation))
            throw new Error('Invalid schema operation')
          this.need(':')
          schema.roots[operation] = this.name()
        } while (!this.take('}'))
      } else if (!extended || start === this.index) throw new Error('Expected schema operations')
      return
    }
    if (!['scalar', 'type', 'interface', 'union', 'enum', 'input', 'directive'].includes(kind))
      throw new Error(`Unknown definition ${kind}`)
    if (kind === 'directive') this.need('@')
    const name = (kind === 'directive' ? '@' : '') + this.name()
    const prior = schema.types[name]
    if (prior && prior.kind !== kind) throw new Error(`Conflicting kind for ${name}`)
    /** @type {Definition} */
    const definition = prior ?? { kind, fields: Object.create(null), members: [], interfaces: [] }
    schema.types[name] = definition
    const start = this.index
    if (kind === 'directive') {
      for (const [argument, field] of Object.entries(this.inputs()))
        mergeField(definition.fields, argument, field)
      const repeatable = this.take('repeatable')
      if (prior && prior.repeatable !== repeatable)
        throw new Error(`Conflicting repeatability for ${name}`)
      definition.repeatable = repeatable
      this.need('on')
      this.take('|')
      definition.members.push(this.name())
      while (this.take('|')) definition.members.push(this.name())
      return
    }
    if (['type', 'interface'].includes(kind) && this.take('implements')) {
      this.take('&')
      definition.interfaces.push(this.name())
      while (this.take('&')) definition.interfaces.push(this.name())
    }
    this.directives()
    if (['type', 'interface', 'input'].includes(kind) && this.peek() === '{')
      this.fields(definition)
    else if (kind === 'enum' && this.take('{')) {
      if (this.peek() === '}') throw new Error('Empty enum')
      do {
        this.description()
        definition.members.push(this.name())
        this.directives()
      } while (!this.take('}'))
    } else if (kind === 'union' && this.take('=')) {
      this.take('|')
      definition.members.push(this.name())
      while (this.take('|')) definition.members.push(this.name())
    } else if (!extended && kind !== 'scalar') throw new Error(`Missing ${kind} body`)
    if (extended && start === this.index) throw new Error('Empty extension')
  }
}

/** @param {string} text
 * @returns {{ok: true, schema: GraphqlSchema}|{ok: false, error: string}} */
export function parseGraphqlSchema(text) {
  try {
    const reader = new Reader(tokenize(text))
    /** @type {GraphqlSchema} */
    const schema = { types: Object.create(null), roots: Object.create(null) }
    while (reader.peek()) reader.definition(schema)
    return { ok: true, schema }
  } catch (error) {
    return { ok: false, error: String(error instanceof Error ? error.message : error) }
  }
}

/** Nullability may strengthen at any output layer, and weaken at any input layer.
 * @param {string} before @param {string} after @param {boolean} input @returns {boolean} */
function compatibleType(before, after, input) {
  const oldRequired = before.endsWith('!')
  const newRequired = after.endsWith('!')
  if (input ? !oldRequired && newRequired : oldRequired && !newRequired) return false
  const oldCore = oldRequired ? before.slice(0, -1) : before
  const newCore = newRequired ? after.slice(0, -1) : after
  if (oldCore.startsWith('[') && newCore.startsWith('['))
    return compatibleType(oldCore.slice(1, -1), newCore.slice(1, -1), input)
  return oldCore === newCore
}

/**
 * @typedef {'breaking'|'dangerous'|'safe'} SchemaClass
 * @typedef {'type-removed'|'type-added'|'kind-changed'|'field-removed'|'field-added'|
 * 'field-type-changed'|'default-changed'|'argument-removed'|'argument-added'|
 * 'argument-changed'|'member-removed'|'member-added'|'interface-removed'|
 * 'interface-added'|'repeatable-changed'|'root-changed'|'root-added'} FindingKind
 * @typedef {{class: SchemaClass, kind: FindingKind, type: string, field?: string,
 * member?: string, text: string, pass: number, typeIndex: number, fieldIndex: number}} SchemaFinding
 */

/** The gate and the proof page consume the same classification, in comparator walk order.
 * @param {GraphqlSchema} base @param {GraphqlSchema} head @returns {SchemaFinding[]} */
export function graphqlSchemaFindings(base, head) {
  /** @type {SchemaFinding[]} */
  const findings = []
  const baseNames = Object.keys(base.types)
  const headNames = Object.keys(head.types)
  const positions = [baseNames, headNames].map(
    (names) => new Map(names.map((name, index) => [name, index])),
  )
  /** @param {string} type @param {number} pass @param {number} fieldIndex
   * @param {SchemaClass} risk @param {FindingKind} kind @param {string} text
   * @param {{field?: string, member?: string}} [detail] */
  const add = (type, pass, fieldIndex, risk, kind, text, detail = {}) =>
    findings.push({
      class: risk,
      kind,
      type,
      ...detail,
      text,
      pass,
      typeIndex: positions[pass].get(type) ?? -1,
      fieldIndex,
    })

  /** @param {string[]} before @param {string[]} after @param {string} type
   * @param {boolean} interfaces @param {boolean} dangerous */
  function members(before, after, type, interfaces, dangerous) {
    const path = interfaces ? `${type} implements` : type
    for (const name of new Set(before))
      if (!after.includes(name))
        add(
          type,
          0,
          before.indexOf(name),
          'breaking',
          interfaces ? 'interface-removed' : 'member-removed',
          `${path}.${name} removed`,
          interfaces ? {} : { member: name },
        )
    for (const name of new Set(after))
      if (!before.includes(name))
        add(
          type,
          1,
          after.indexOf(name),
          dangerous ? 'dangerous' : 'safe',
          interfaces ? 'interface-added' : 'member-added',
          `${path}.${name} added`,
          interfaces ? {} : { member: name },
        )
  }
  /** @param {Record<string, Field>} before @param {Record<string, Field>} after
   * @param {string} type @param {boolean} input @param {string} [owner]
   * @param {number} [ownerIndex] */
  function fields(before, after, type, input, owner, ownerIndex = 0) {
    const path = owner ? `${type}.${owner}` : type
    for (const [index, [name, oldField]] of Object.entries(before).entries()) {
      const next = after[name]
      const label = `${path}.${name}`
      const at = owner ? ownerIndex : index
      const detail = { field: owner ?? name }
      if (!next) {
        add(
          type,
          0,
          at,
          'breaking',
          owner ? 'argument-removed' : 'field-removed',
          `${label} removed`,
          detail,
        )
        continue
      }
      if (oldField.type !== next.type) {
        const safe = compatibleType(oldField.type, next.type, input)
        const message =
          input && !oldField.type.endsWith('!') && next.type.endsWith('!')
            ? `${label} became non-null`
            : `${label} type changed (${oldField.type} → ${next.type})`
        add(
          type,
          0,
          at,
          safe ? 'safe' : 'breaking',
          owner ? 'argument-changed' : 'field-type-changed',
          message,
          detail,
        )
      }
      if (oldField.defaultValue !== next.defaultValue)
        add(
          type,
          0,
          at,
          oldField.defaultValue === undefined ? 'safe' : 'breaking',
          owner ? 'argument-changed' : 'default-changed',
          `${label} default changed`,
          detail,
        )
      fields(oldField.args, next.args, type, true, owner ?? name, at)
    }
    for (const [index, [name, next]] of Object.entries(after).entries())
      if (!before[name]) {
        const required = input && next.type.endsWith('!') && next.defaultValue === undefined
        add(
          type,
          owner ? 0 : 1,
          owner ? ownerIndex : index,
          required ? 'breaking' : 'safe',
          owner ? 'argument-added' : 'field-added',
          `${path}.${name} added${required ? ' (required)' : ''}`,
          { field: owner ?? name },
        )
      }
  }
  for (const [name, before] of Object.entries(base.types)) {
    const after = head.types[name]
    if (!after) {
      add(name, 0, 0, 'breaking', 'type-removed', `${name} removed`)
      continue
    }
    if (before.kind !== after.kind) {
      add(name, 0, 0, 'breaking', 'kind-changed', `${name} kind changed`)
      continue
    }
    if (before.repeatable !== after.repeatable)
      add(
        name,
        0,
        0,
        before.repeatable ? 'breaking' : 'safe',
        'repeatable-changed',
        `${name} repeatability ${after.repeatable ? 'added' : 'removed'}`,
      )
    fields(
      before.fields,
      after.fields,
      name,
      before.kind === 'input' || before.kind === 'directive',
    )
    members(before.members, after.members, name, false, ['enum', 'union'].includes(before.kind))
    members(before.interfaces, after.interfaces, name, true, false)
  }
  for (const name of headNames)
    if (!base.types[name]) add(name, 1, 0, 'safe', 'type-added', `${name} added`)
  const oldRoots = operationRoots(base)
  const newRoots = operationRoots(head)
  for (const [operation, before] of Object.entries(oldRoots))
    if (newRoots[operation] !== before)
      add(
        'schema',
        0,
        Object.keys(oldRoots).indexOf(operation),
        'breaking',
        'root-changed',
        `schema.${operation} changed`,
      )
  for (const operation of Object.keys(newRoots))
    if (!oldRoots[operation])
      add(
        'schema',
        1,
        Object.keys(newRoots).indexOf(operation),
        'safe',
        'root-added',
        `schema.${operation} added`,
      )
  return findings
}

/** @param {GraphqlSchema} base @param {GraphqlSchema} head
 * @returns {{breaking: string[], additive: string[]}} */
export function graphqlSchemaChanges(base, head) {
  const findings = graphqlSchemaFindings(base, head)
  return {
    breaking: findings
      .filter((finding) => finding.class === 'breaking')
      .map((finding) => finding.text),
    additive: findings
      .filter((finding) => finding.class !== 'breaking')
      .map((finding) => finding.text),
  }
}

/** @param {GraphqlSchema} schema @returns {Record<string, string>} */
function operationRoots(schema) {
  if (Object.keys(schema.roots).length) return schema.roots
  return Object.fromEntries(
    ['query', 'mutation', 'subscription'].flatMap((operation) => {
      const name = operation[0].toUpperCase() + operation.slice(1)
      return schema.types[name]?.kind === 'type' ? [[operation, name]] : []
    }),
  )
}
