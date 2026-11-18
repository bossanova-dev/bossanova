import path from 'node:path'
import {
  blockFact,
  describerMain,
  envelopeDescriber,
  item,
  listed,
  MAX_ITEMS,
  MAX_TEXT_CHARS,
  plural,
  runCli,
} from './describer-kit.mjs'
import { MAX_STEPS } from './verify-describe-blocks.mjs'
import { capText } from './verify-proof.mjs'

/** @typedef {Parameters<import('./describer-kit.mjs').CollectItems>[0]} Input */
/** @typedef {{blocks: Map<string, string>, moved: {from: string, to: string}[], removed: Map<string, boolean>}} Blocks */
/** @typedef {ReturnType<typeof terraformDiff>} Diff */
const BLOCK =
  /^(resource|data)\s+"([^"]+)"\s+"([^"]+)"\s*\{|^(module|variable|output)\s+"([^"]+)"\s*\{|^(moved|removed|locals)\s*\{|^([a-z_]\w*)(?:\s+"[^"]*")*\s*\{/

/** End of a quoted template, including strings nested inside interpolation expressions.
 * @param {string} source @param {number} start
 */
function templateEnd(source, start) {
  const stack = [0] // Zero is a string; positive values count expression braces.
  for (let at = start + 1; at < source.length; at += 1) {
    const char = source[at]
    const top = stack.length - 1
    if (stack[top] === 0) {
      if (char === '\\') at += 1
      else if (char === '"') {
        stack.pop()
        if (!stack.length) return at + 1
      } else if (char === '$' || char === '%') {
        if (source[at + 1] === char) at += 1
        else if (source[at + 1] === '{') {
          stack.push(1)
          at += 1
        }
      }
    } else if (char === '"') stack.push(0)
    else if (char === '{') stack[top] += 1
    else if (char === '}' && --stack[top] === 0) stack.pop()
  }
  return source.length
}

/**
 * The top-level blocks of one Terraform source. `blocks` maps an address (`aws_x.name`,
 * `data.aws_x.name`, `module.name`, `var.name`, `output.name`) to its normalised body; `moved`
 * holds `{from, to}` pairs and `removed` maps addresses to their destruction setting.
 */
/** @param {string} source @returns {Blocks} */
export function terraformBlocks(source) {
  const lines = cleanComments(source).split(/\r?\n/)
  /** @type {Blocks} */
  const result = { blocks: new Map(), moved: [], removed: new Map() }
  const locals = []
  /** @type {Map<string, string[]>} */
  const configuration = new Map()
  for (let index = 0; index < lines.length; index += 1) {
    const header = lines[index].match(BLOCK)
    if (!header) continue
    const body = []
    let depth = 1
    let heredoc = ''
    let fragment = lines[index].slice(header[0].length)
    while (true) {
      if (heredoc) {
        body.push(fragment)
        if (fragment.trim() === heredoc) heredoc = ''
      } else {
        let end = fragment.length
        for (let at = 0; at < fragment.length; at += 1) {
          const char = fragment[at]
          if (char === '"') {
            at = templateEnd(fragment, at) - 1
            continue
          }
          if (char === '{') depth += 1
          if (char === '}') {
            depth -= 1
            if (depth === 0) {
              end = at
              break
            }
          }
        }
        body.push(fragment.slice(0, end))
        heredoc = fragment.match(/<<-?\s*([A-Za-z_]\w*)\s*$/)?.[1] ?? ''
      }
      if (depth === 0 || index >= lines.length - 1) break
      index += 1
      fragment = lines[index]
    }
    const normalized = body
      .map((line) => line.trimEnd())
      .filter(Boolean)
      .join('\n')
    /** @param {string} name */
    const attribute = (name) => {
      const assignment = normalized.match(new RegExp('(?:^|\\s)' + name + '\\s*=\\s*', 'm'))
      if (!assignment) return undefined
      return normalized
        .slice((assignment.index ?? 0) + assignment[0].length)
        .match(/^(?:[^\s"]|"(?:\\.|[^"\\])*")+/)?.[0]
    }
    if (header[1])
      result.blocks.set(
        header[1] === 'data' ? `data.${header[2]}.${header[3]}` : `${header[2]}.${header[3]}`,
        normalized,
      )
    else if (header[4])
      result.blocks.set(
        `${{ module: 'module', variable: 'var', output: 'output' }[header[4]]}.${header[5]}`,
        normalized,
      )
    else if (header[7]) {
      configuration.set(header[7], [
        ...(configuration.get(header[7]) ?? []),
        `${header[0]}\n${normalized}`,
      ])
    } else if (header[6] === 'locals') {
      locals.push(normalized)
    } else if (header[6] === 'moved') {
      const from = attribute('from'),
        to = attribute('to')
      if (from && to) result.moved.push({ from, to })
    } else {
      const from = attribute('from')
      if (from)
        result.removed.set(
          from,
          normalized.match(/\blifecycle\s*\{[^}]*\bdestroy\s*=\s*(true|false)\b/s)?.[1] !== 'false',
        )
    }
  }
  if (locals.length) result.blocks.set('locals', locals.filter(Boolean).sort().join('\n'))
  for (const [kind, bodies] of configuration)
    result.blocks.set(`configuration.${kind}`, bodies.filter(Boolean).sort().join('\n'))
  return result
}

/** @param {string} source */
function cleanComments(source) {
  let block = false
  let heredoc = ''
  return String(source ?? '')
    .split(/\r?\n/)
    .map((line) => {
      if (heredoc) {
        if (line.trim() === heredoc) heredoc = ''
        return line
      }
      let clean = ''
      for (let at = 0; at < line.length; at += 1) {
        const char = line[at],
          next = line[at + 1]
        if (block) {
          if (char === '*' && next === '/') {
            block = false
            at += 1
            clean += ' '
          }
          continue
        }
        if (char === '#' || (char === '/' && next === '/')) break
        if (char === '/' && next === '*') {
          block = true
          at += 1
          clean += ' '
          continue
        }
        if (char === '"') {
          const end = templateEnd(line, at)
          clean += line.slice(at, end)
          at = end - 1
          continue
        }
        clean += char
      }
      heredoc = clean.match(/<<-?\s*([A-Za-z_]\w*)\s*$/)?.[1] ?? ''
      return clean
    })
    .join('\n')
}

/** Only a variable's own assignment establishes its default.
 * @param {string} source
 */
function hasDefault(source) {
  let depth = 0
  let heredoc = ''
  const visible = source.split('\n').map((line) => {
    if (heredoc) {
      if (line.trim() === heredoc) heredoc = ''
      return ''
    }
    let text = ''
    for (let at = 0; at < line.length; at += 1) {
      const char = line[at]
      if (char === '"') {
        at = templateEnd(line, at) - 1
        text += ' '
        continue
      }
      const marker = line.slice(at).match(/^<<-?\s*([A-Za-z_]\w*)\s*$/)?.[1]
      if (marker) {
        heredoc = marker
        break
      }
      if ('{[('.includes(char)) depth += 1
      else if ('}])'.includes(char)) depth -= 1
      text += depth === 0 && !'}])'.includes(char) ? char : ' '
    }
    return text
  })
  return /(?:^|\s)default\s*=/m.test(visible.join('\n'))
}

/** @param {string} address */
const kindOf = (address) =>
  address.startsWith('configuration.')
    ? 'configuration'
    : address === 'locals'
      ? 'local values'
      : address.startsWith('var.')
        ? 'variable'
        : address.startsWith('output.')
          ? 'output'
          : address.startsWith('module.')
            ? 'module'
            : address.startsWith('data.')
              ? 'data source'
              : 'resource'

/** Split only at address separators, keeping string-keyed instances intact.
 * @param {string} address
 */
function addressParts(address) {
  let depth = 0
  let quoted = false
  let last = -1
  for (let at = 0; at < address.length; at += 1) {
    const char = address[at]
    if (quoted) {
      if (char === '\\') at += 1
      else if (char === '"') quoted = false
    } else if (char === '"') quoted = true
    else if (char === '[') depth += 1
    else if (char === ']') depth -= 1
    else if (char === '.' && depth === 0) last = at
  }
  return { type: address.slice(0, Math.max(0, last)), name: address.slice(last + 1) }
}

/** @param {string} address */
const typeOf = (address) => addressParts(address).type

/** @param {string} address */
const nameOf = (address) => addressParts(address).name

/** @param {string} address @param {{from: string}[]} moves */
const containsMovedAddress = (address, moves) =>
  moves.some(({ from }) => from.startsWith(`${address}.`) || from.startsWith(`${address}[`))

/**
 * Block movement between two versions of one Terraform directory (every `.tf` file in it,
 * concatenated). Renames are `moved` pairs, or a removal and an addition of the same type with an
 * identical body, which Terraform plans as a destroy and a create.
 */
/** @param {string} baseSource @param {string} headSource */
export function terraformDiff(baseSource, headSource) {
  const base = terraformBlocks(baseSource)
  const head = terraformBlocks(headSource)
  const added = [...head.blocks.keys()].filter((key) => !base.blocks.has(key))
  const removed = [...base.blocks.keys()].filter((key) => !head.blocks.has(key))
  const changed = [...head.blocks.keys()].filter(
    (key) => base.blocks.has(key) && base.blocks.get(key) !== head.blocks.get(key),
  )
  const renamed = []
  const unmovedRenames = []

  // State addresses may name child-module resources or individual instances without a direct
  // block declaration in this directory. Existing migrations are not new changes.
  for (const { from, to } of head.moved) {
    if (!base.moved.some((pair) => pair.from === from && pair.to === to)) renamed.push({ from, to })
  }

  for (const { from, to } of renamed) {
    if (
      base.blocks.has(from) &&
      head.blocks.has(to) &&
      (base.blocks.get(from) !== head.blocks.get(to) || typeOf(from) !== typeOf(to)) &&
      !changed.includes(to)
    )
      changed.push(to)
  }

  for (const from of removed) {
    if (
      renamed.some((pair) => pair.from === from) ||
      containsMovedAddress(from, renamed) ||
      head.removed.has(from)
    )
      continue

    const to = added.find(
      (candidate) =>
        !renamed.some((pair) => pair.to === candidate) &&
        !unmovedRenames.some((pair) => pair.to === candidate) &&
        kindOf(candidate) === kindOf(from) &&
        !['variable', 'output', 'data source', 'configuration'].includes(kindOf(from)) &&
        typeOf(candidate) === typeOf(from) &&
        head.blocks.get(candidate) === base.blocks.get(from),
    )

    if (to) unmovedRenames.push({ from, to })
  }

  const paired = new Set([...renamed, ...unmovedRenames].flatMap(({ from, to }) => [from, to]))

  return {
    added: added.filter((key) => !paired.has(key)),
    changed,
    removed: removed.filter((key) => !paired.has(key)),
    removedByBlock: head.removed,
    removedBlocks: [...new Set([...base.removed.keys(), ...head.removed.keys()])].flatMap(
      (address) => {
        const before = base.removed.get(address)
        const after = head.removed.get(address)
        return before === after ? [] : [{ address, before, after }]
      },
    ),
    renamed,
    requiredVariables: new Set(
      [...added, ...changed].filter(
        (key) =>
          kindOf(key) === 'variable' &&
          !hasDefault(head.blocks.get(key)) &&
          (!base.blocks.has(key) || hasDefault(base.blocks.get(key))),
      ),
    ),
    unmovedRenames,
  }
}

const GROUPS = ['resource', 'data source', 'module']

/** One directory's Terraform items. */
/** @param {string} dir @param {Diff} diff @param {string[]} paths @param {boolean} configurationOnly */
export function terraformItems(dir, diff, paths, configurationOnly = false) {
  /** @type {import('./describer-kit.mjs').Fact[]} */
  const items = []
  /** @param {string} text */
  const add = (text) => items.push(item('infra', capText(`${dir}: ${text}`, MAX_TEXT_CHARS), paths))
  /** @param {string[]} list @param {string} kind */
  const ofKind = (list, kind) => list.filter((key) => kindOf(key) === kind)

  for (const kind of GROUPS) {
    const added = ofKind(diff.added, kind)
    const removed = ofKind(diff.removed, kind)
    const changed = ofKind(diff.changed, kind)
    const migrated = removed.filter((key) => containsMovedAddress(key, diff.renamed))
    const plain = removed.filter((key) => !diff.removedByBlock.has(key) && !migrated.includes(key))
    const byBlock = removed.filter((key) => diff.removedByBlock.has(key))

    if (added.length > 0)
      add(
        `${plural(added.length, `${kind}${configurationOnly ? ' configuration' : ''} added`, `${kind}${configurationOnly ? ' configurations' : 's'} added`)} (${listed(added)})`,
      )

    // A data source manages nothing, so removing one destroys nothing.
    if (plain.length > 0 && configurationOnly && kind !== 'data source') {
      add(
        `${plural(plain.length, `${kind} configuration removed`, `${kind} configurations removed`)} with no moved or removed block; if state addresses continue, the plan destroys ${plain.length === 1 ? 'it' : 'them'} (${listed(plain)})`,
      )
    } else if (plain.length > 0 && kind === 'data source') {
      add(`${plural(plain.length, `${kind} removed`, `${kind}s removed`)} (${listed(plain)})`)
    } else if (plain.length > 0) {
      add(
        `${plural(plain.length, `${kind} removed`, `${kind}s removed`)} with no moved or removed block, so the plan destroys ${plain.length === 1 ? 'it' : 'them'} (${listed(plain)})`,
      )
    }

    if (byBlock.length > 0) {
      add(
        `${plural(byBlock.length, `${kind} dropped`, `${kind}s dropped`)} through a removed block (${listed(byBlock)})`,
      )
    }

    if (migrated.length > 0)
      add(
        `${plural(migrated.length, `${kind} removed`, `${kind}s removed`)} with moved instance or child addresses; review the plan for remaining instances (${listed(migrated)})`,
      )

    if (changed.length > 0) {
      add(
        `${plural(changed.length, `${kind} configuration changed`, `${kind} configurations changed`)} (${listed(changed)})`,
      )
    }
  }

  if (diff.renamed.length > 0) {
    add(
      `renamed with a moved block: ${listed(diff.renamed.map(({ from, to }) => `${from} -> ${typeOf(to) === typeOf(from) ? nameOf(to) : to}`))}`,
    )
  }

  if (diff.unmovedRenames.length > 0) {
    add(
      `renamed with no moved block${configurationOnly ? '; if state addresses continue, the plan' : ', so the plan'} destroys and recreates ${diff.unmovedRenames.length === 1 ? 'it' : 'them'}: ${listed(diff.unmovedRenames.map(({ from, to }) => `${from} -> ${nameOf(to)}`))}`,
    )
  }

  for (const { address, before, after } of diff.removedBlocks) {
    if (diff.removed.includes(address)) continue
    if (after === undefined) add(`removed block removed for ${address}; review prior state`)
    else
      add(
        `removed block ${before === undefined ? 'added' : 'changed'} for ${address} (destroy=${after})`,
      )
  }

  const variables = ofKind(diff.added, 'variable').map((key) =>
    diff.requiredVariables.has(key) ? `${nameOf(key)} (no default)` : nameOf(key),
  )
  const outputs = ofKind(diff.added, 'output').map(nameOf)
  const goneVariables = ofKind(diff.removed, 'variable').map(nameOf)
  const goneOutputs = ofKind(diff.removed, 'output').map(nameOf)
  const changedVariables = ofKind(diff.changed, 'variable').map((key) =>
    diff.requiredVariables.has(key) ? `${nameOf(key)} (no default)` : nameOf(key),
  )
  const changedOutputs = ofKind(diff.changed, 'output').map(nameOf)
  const io = [
    ...(variables.length > 0 ? [`variables added: ${listed(variables)}`] : []),
    ...(goneVariables.length > 0 ? [`variables removed: ${listed(goneVariables)}`] : []),
    ...(changedVariables.length > 0 ? [`variables changed: ${listed(changedVariables)}`] : []),
    ...(outputs.length > 0 ? [`outputs added: ${listed(outputs)}`] : []),
    ...(goneOutputs.length > 0 ? [`outputs removed: ${listed(goneOutputs)}`] : []),
    ...(changedOutputs.length > 0 ? [`outputs changed: ${listed(changedOutputs)}`] : []),
  ]

  if (io.length > 0) add(io.join('; '))

  /** @type {Array<'added' | 'changed' | 'removed'>} */
  const localOperations = ['added', 'changed', 'removed']
  for (const operation of localOperations) {
    if (diff[operation].includes('locals')) add(`local values ${operation}`)
    for (const key of ofKind(diff[operation], 'configuration'))
      add(`${nameOf(key)} configuration ${operation}`)
  }

  return items
}

/** @param {Input} input */
function terraformDirs({ base, changes, git, head }) {
  /** @type {Map<string, string[]>} */
  const dirs = new Map()
  for (const change of changes) {
    for (const file of [change.path, change.previousPath]) {
      if (!file?.endsWith('.tf')) continue
      const dir = path.posix.dirname(file)
      dirs.set(dir, [
        ...new Set([
          ...(dirs.get(dir) ?? []),
          change.path,
          ...(change.previousPath ? [change.previousPath] : []),
        ]),
      ])
    }
  }
  if (dirs.size === 0) return []
  const ordered = [...dirs.keys()].sort()
  /** @param {string} ref */
  const sources = (ref) => {
    const files = git
      .filesAt(ref, ordered)
      .filter((file) => file.endsWith('.tf') && dirs.has(path.posix.dirname(file)))
    const contents = git.readFiles(ref, files)
    return new Map(
      ordered.map((dir) => {
        const inDir = files.filter((file) => path.posix.dirname(file) === dir).sort()
        return [
          dir,
          {
            source: inDir.map((file) => contents.get(file)).join('\n'),
            hasFiles: inDir.length > 0,
            managedFiles: new Set(
              inDir.filter((file) =>
                [...terraformBlocks(contents.get(file)).blocks.keys()].some((address) =>
                  ['resource', 'module'].includes(kindOf(address)),
                ),
              ),
            ),
          },
        ]
      }),
    )
  }
  const before = sources(base)
  const after = sources(head)
  const crossDirectory = changes.flatMap((change) => {
    if (
      change.status !== 'R' ||
      !change.previousPath?.endsWith('.tf') ||
      !change.path.endsWith('.tf')
    )
      return []
    const from = path.posix.dirname(change.previousPath),
      to = path.posix.dirname(change.path)
    return from === to
      ? []
      : [
          {
            from,
            to,
            managed:
              before.get(from).managedFiles.has(change.previousPath) &&
              after.get(to).managedFiles.has(change.path),
          },
        ]
  })
  /** @type {Map<string, Set<string>>} */
  const destinations = new Map(),
    origins = new Map()
  for (const { from, to } of crossDirectory) {
    destinations.set(from, new Set([...(destinations.get(from) ?? []), to]))
    origins.set(to, new Set([...(origins.get(to) ?? []), from]))
  }
  /** @type {Map<string, string>} */
  const moves = new Map(),
    previousDirs = new Map()
  for (const [from, targets] of destinations) {
    const to = [...targets][0]
    if (
      targets.size === 1 &&
      origins.get(to).size === 1 &&
      !after.get(from).hasFiles &&
      !before.get(to).hasFiles &&
      crossDirectory.some((pair) => pair.from === from && pair.to === to && pair.managed)
    ) {
      moves.set(from, to)
      previousDirs.set(to, from)
    }
  }
  /** @type {Map<string, string[]>} */
  const migrations = new Map()
  for (const { from, to } of crossDirectory) {
    if (moves.get(from) === to) continue
    // A boilerplate rename alone cannot merge an otherwise deleted and added root.
    if (
      !after.get(from).hasFiles &&
      !before.get(to).hasFiles &&
      destinations.get(from).size === 1 &&
      origins.get(to).size === 1
    )
      continue
    for (const dir of [from, to])
      migrations.set(dir, [...new Set([...(migrations.get(dir) ?? []), `${from} -> ${to}`])])
  }
  return ordered
    .filter((dir) => !moves.has(dir))
    .map((dir) => ({
      dir,
      paths: [...new Set([...(dirs.get(dir) ?? []), ...(dirs.get(previousDirs.get(dir)) ?? [])])],
      diff: terraformDiff(before.get(previousDirs.get(dir) ?? dir).source, after.get(dir).source),
      deleted: !after.get(dir).hasFiles,
      moveFrom: previousDirs.get(dir) ?? '',
      migrations: migrations.get(dir) ?? [],
      reviewOnly: migrations.has(dir),
    }))
}

/**
 * @param {Input['git']} git @param {string} ref
 * @returns {Map<string, string[]>}
 */
function localCallers(git, ref) {
  const files = git
    .grep(['-l', '-z', '-w', '-F', 'source', ref, '--', '*.tf'])
    .split('\0')
    .filter(Boolean)
    .map((file) => file.slice(ref.length + 1))
  const callers = new Map()
  for (const [file, source] of git.readFiles(ref, files)) {
    const caller = path.posix.dirname(file)
    for (const [address, body] of terraformBlocks(source).blocks) {
      if (!address.startsWith('module.')) continue
      const local = body.match(/^\s*source\s*=\s*"(\.\.?\/[^"\r\n]*)"/m)?.[1]
      if (!local) continue
      const child = path.posix.normalize(path.posix.join(caller, local)).replace(/\/$/, '')
      callers.set(child, [...new Set([...(callers.get(child) ?? []), caller])].sort())
    }
  }
  return callers
}

/**
 * Roots reachable through local callers. A cycle or depth-limited branch yields no guessed root.
 * @param {string} dir @param {Map<string, string[]>} callers @returns {string[]}
 */
export function terraformRoots(dir, callers) {
  const roots = new Set()
  /** @param {string} current @param {Set<string>} seen @param {number} depth */
  const visit = (current, seen, depth) => {
    if (seen.has(current) || depth > 8) return
    const parents = callers.get(current) ?? []
    if (parents.length === 0) {
      roots.add(current)
      return
    }
    for (const parent of parents) visit(parent, new Set([...seen, current]), depth + 1)
  }
  visit(dir, new Set(), 0)
  return [...roots].sort()
}

/** @param {string} value */
const quoted = (value) => `\`${value}\``

/** @param {Diff} diff @param {boolean} configurationOnly */
function stepDetails(diff, configurationOnly = false) {
  const destroyed = diff.removed.filter(
    (key) =>
      ['resource', 'module'].includes(kindOf(key)) &&
      !containsMovedAddress(key, diff.renamed) &&
      diff.removedByBlock.get(key) !== false,
  )
  const stateDestroyed = diff.removedBlocks.filter(
    ({ address, after }) =>
      after === true &&
      !diff.removed.includes(address) &&
      ['resource', 'module'].includes(kindOf(address)),
  )
  /** @type {[number, string][]} */
  const counts = [
    [
      diff.added.filter((key) => !key.startsWith('var.') && !key.startsWith('output.')).length,
      'added',
    ],
    [
      diff.changed.filter((key) => !key.startsWith('var.') && !key.startsWith('output.')).length,
      'changed',
    ],
    [
      configurationOnly ? destroyed.length + stateDestroyed.length : stateDestroyed.length,
      'potentially destroyed',
    ],
    [configurationOnly ? 0 : destroyed.length, 'destroyed'],
    [diff.renamed.length, 'moved'],
    [diff.unmovedRenames.length, configurationOnly ? 'potentially recreated' : 'recreated'],
    [
      diff.removedBlocks.length,
      diff.removedBlocks.length === 1 ? 'removed block change' : 'removed block changes',
    ],
  ]
  const parts = counts.filter(([count]) => count > 0).map(([count, word]) => `${count} ${word}`)
  const required = [...diff.requiredVariables].map(nameOf).map(quoted)
  return { counts, parts, required }
}

/** @param {string} dir @param {Diff} diff @param {string[]} roots @param {string[]} deletedRoots @param {string} moveFrom */
export function terraformStep(dir, diff, roots = [dir], deletedRoots = [], moveFrom = '') {
  if (
    !moveFrom &&
    ![
      diff.added,
      diff.changed,
      diff.removed,
      diff.renamed,
      diff.unmovedRenames,
      diff.removedBlocks,
    ].some((list) => list.length)
  )
    return null
  const { parts, required } = stepDetails(diff, Boolean(moveFrom))
  const child = roots.length !== 1 || roots[0] !== dir
  const liveRoots = roots.filter((root) => !deletedRoots.includes(root))
  const target = liveRoots.length
    ? liveRoots.map(quoted).join(' and ')
    : `the root calling ${quoted(dir)} (local callers contain a cycle or exceed depth 8)`
  const actions = [
    ...(deletedRoots.length
      ? [
          `Decommission deleted Terraform ${deletedRoots.length === 1 ? 'root' : 'roots'} ${deletedRoots.map(quoted).join(' and ')} using previous configuration and state; review its Terraform plan first`,
        ]
      : []),
    ...(liveRoots.length || roots.length === 0
      ? [
          `Run terraform apply in ${target}${child && liveRoots.length ? ` for module ${quoted(dir)}` : ''}`,
        ]
      : []),
  ]
  const text = `${moveFrom ? `Review state/backend continuity and module source paths for configuration moved from ${quoted(moveFrom)} to ${quoted(dir)}; ` : ''}${diff.removedBlocks.length ? 'Review the Terraform plan and prior state for removed block changes; ' : ''}${actions.join('; ')}${parts.length ? ` (${parts.join(', ')})` : ''}${required.length && liveRoots.length ? `, ${child ? 'callers must set' : 'passing'} ${listed(required)}` : ''}`
  if (text.length <= MAX_TEXT_CHARS) return text
  const safety = [
    ...parts,
    ...(required.length ? [`set ${required.length} required variables (see facts)`] : []),
    ...(moveFrom ? ['review state/backend continuity and module source paths'] : []),
    ...(diff.removedBlocks.length ? ['review plan and prior state'] : []),
  ].join('; ')
  return capText([safety, ...actions].filter(Boolean).join('; '), MAX_TEXT_CHARS)
}

/** @type {import('./describer-kit.mjs').CollectItems} */
export function collectTerraformFacts(input) {
  const dirs = terraformDirs(input)
  if (dirs.length === 0) return []
  const callers = localCallers(input.git, input.head)
  const baseCallers = dirs.some((entry) => entry.deleted || entry.moveFrom)
    ? localCallers(input.git, input.base)
    : new Map()
  const facts = dirs.flatMap(({ dir, diff, paths, moveFrom, migrations, reviewOnly }) => [
    ...(moveFrom
      ? [
          item(
            'infra',
            capText(
              `Terraform configuration directory moved: ${moveFrom} -> ${dir}`,
              MAX_TEXT_CHARS,
            ),
            paths,
          ),
        ]
      : []),
    ...(reviewOnly
      ? [
          item(
            'infra',
            capText(
              `Terraform configuration file migrations: ${listed(migrations)}; review state/backend continuity`,
              MAX_TEXT_CHARS,
            ),
            paths,
          ),
        ]
      : []),
    ...terraformItems(dir, diff, paths, reviewOnly || Boolean(moveFrom)),
  ])
  const movedDirs = new Map(
    dirs.filter((entry) => entry.moveFrom).map((entry) => [entry.moveFrom, entry.dir]),
  )
  const actions = dirs.map((entry) => {
    const previousCaller =
      entry.moveFrom && !callers.has(entry.dir) && baseCallers.has(entry.moveFrom)
    const roots = terraformRoots(
      previousCaller ? entry.moveFrom : entry.dir,
      entry.deleted || previousCaller ? baseCallers : callers,
    ).map((root) => movedDirs.get(root) ?? root)
    return { ...entry, roots }
  })
  const rootDirs = [...new Set(actions.flatMap((entry) => entry.roots))]
  const liveRoots = new Set(
    (rootDirs.length ? input.git.filesAt(input.head, rootDirs) : [])
      .filter((file) => file.endsWith('.tf'))
      .map((file) => path.posix.dirname(file)),
  )
  const steps = actions.flatMap(({ dir, diff, roots, moveFrom, reviewOnly }) => {
    const deletedRoots = roots.filter((root) => !liveRoots.has(root))
    const text = reviewOnly
      ? capText(
          `Review Terraform plans and state/backend continuity for configuration file migrations involving ${quoted(dir)} before choosing migration or apply actions`,
          MAX_TEXT_CHARS,
        )
      : terraformStep(dir, diff, roots, deletedRoots, moveFrom)
    return text ? [{ dir, roots, deletedRoots, reviewOnly, text, diff, moveFrom }] : []
  })
  const allPaths = [...new Set(dirs.flatMap((entry) => entry.paths))]
  if (facts.length > MAX_ITEMS) {
    const omitted = facts.slice(MAX_ITEMS - 1)
    const names = dirs
      .filter((entry) =>
        omitted.some((fact) => fact.paths.some((file) => entry.paths.includes(file))),
      )
      .map((entry) => quoted(entry.dir))
    facts.splice(
      MAX_ITEMS - 1,
      Infinity,
      item(
        'infra',
        capText(
          `${omitted.length} more Terraform changes in ${names.join(', ')} not shown`,
          MAX_TEXT_CHARS,
        ),
        allPaths,
      ),
    )
  }
  if (steps.length > MAX_STEPS) {
    const omitted = steps.slice(MAX_STEPS - 1)
    const actionable = omitted.filter((entry) => !entry.reviewOnly)
    const targets = [
      ...new Set(
        actionable.flatMap(({ dir, roots, deletedRoots }) =>
          roots.length
            ? roots.filter((root) => !deletedRoots.includes(root)).map(quoted)
            : [`the root calling ${quoted(dir)}`],
        ),
      ),
    ]
    const deletedTargets = [...new Set(actionable.flatMap((entry) => entry.deletedRoots))].map(
      quoted,
    )
    const reviewTargets = omitted
      .filter((entry) => entry.reviewOnly)
      .map((entry) => quoted(entry.dir))
    const mixed = deletedTargets.length > 0 || reviewTargets.length > 0
    const counts = new Map()
    const required = new Set()
    for (const entry of omitted) {
      const details = stepDetails(entry.diff, Boolean(entry.moveFrom) || entry.reviewOnly)
      for (const [count, label] of details.counts)
        if (count) counts.set(label, (counts.get(label) ?? 0) + count)
      for (const name of details.required) required.add(name)
    }
    const safety = [
      ...[...counts].map(([label, count]) => `${count} ${label}`),
      ...(required.size ? [`set ${required.size} required variables (see facts)`] : []),
      ...(omitted.some((entry) => entry.moveFrom || entry.reviewOnly)
        ? ['review state/backend continuity']
        : []),
    ].join('; ')
    const summary = [
      ...(targets.length
        ? [
            mixed
              ? `apply in roots: ${targets.join(', ')}`
              : `Run terraform apply for ${omitted.length} more directories in roots: ${targets.join(', ')}`,
          ]
        : []),
      ...(deletedTargets.length
        ? [
            `decommission deleted Terraform roots ${deletedTargets.join(', ')} using previous configuration and state`,
          ]
        : []),
      ...(reviewTargets.length
        ? [
            `review Terraform plans and state/backend continuity for migrated configuration in ${reviewTargets.join(', ')}`,
          ]
        : []),
    ].join('; ')
    steps.splice(MAX_STEPS - 1, Infinity, {
      ...omitted[0],
      text: capText(
        `${omitted.length} more directories${safety ? ` (${safety})` : ''}: ${summary}`,
        MAX_TEXT_CHARS,
      ),
    })
  }
  if (steps.length)
    facts.push(
      blockFact('infra', { kind: 'steps', steps: steps.map((entry) => entry.text) }, allPaths),
    )
  return facts
}

export const describeTerraform = envelopeDescriber(collectTerraformFacts)
export const main = () => describerMain(describeTerraform)
runCli(import.meta.url, 'boss-verify-terraform', collectTerraformFacts)
