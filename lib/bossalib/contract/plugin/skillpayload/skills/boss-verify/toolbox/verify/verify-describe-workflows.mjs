import path from 'node:path'

import { describerMain, envelopeDescriber, item, listed, runCli } from './describer-kit.mjs'
import { capText, MAX_TEXT_CHARS } from './verify-proof.mjs'

/** @typedef {Map<string, string> & {depth?: number}} Filters */
/** @typedef {Map<string, Filters> & {unreadable?: boolean, raw?: string}} Triggers */

/** @param {string} line */
function stripComment(line) {
  let quote = ''
  for (let at = 0; at < line.length; at += 1) {
    const char = line[at]
    if (quote) {
      if (quote === '"' && char === '\\') at += 1
      else if (char === quote) {
        if (quote === "'" && line[at + 1] === "'") at += 1
        else quote = ''
      }
    } else if ((char === '"' || char === "'") && (at === 0 || /[\s[{,:?-]/.test(line[at - 1]))) {
      quote = char
    } else if (char === '#' && (at === 0 || /\s/.test(line[at - 1]))) {
      return line.slice(0, at).trimEnd()
    }
  }
  return line.trimEnd()
}

/**
 * Trigger -> (filter key -> normalised text). Inline, list and block forms are recognised;
 * list-valued triggers such as schedule keep their list under the empty filter key.
 * @param {string} source
 * @returns {Triggers}
 */
export function workflowTriggers(source) {
  const lines = String(source ?? '')
    .split(/\r?\n/)
    .filter((line) => !/^\s*#/.test(line))
    .map(stripComment)
  /** @type {Triggers} */
  const triggers = new Map()
  const start = lines.findIndex((line) => /^["']?on["']?\s*:/.test(line))
  if (start < 0) return triggers

  const inline = lines[start].replace(/^["']?on["']?\s*:\s*/, '')
  if (inline) {
    if (inline.startsWith('{') || (inline.startsWith('[') && !inline.endsWith(']'))) {
      triggers.unreadable = true
      const raw = [inline]
      for (const line of lines.slice(start + 1)) {
        if (line.trim() && !/^\s|^[}\]]/.test(line)) break
        if (line.trim()) raw.push(line.trim())
      }
      triggers.raw = raw.join('\n')
      return triggers
    }
    for (const name of inline.replace(/[[\]]/g, '').split(',')) {
      const event = name.trim().replace(/^["']|["']$/g, '')
      if (/^[\w-]+$/.test(event)) triggers.set(event, new Map())
    }
    return triggers
  }

  /** @type {string | null} */
  let trigger = null
  /** @type {{depth: number, key: string} | null} */
  let filter = null
  for (const line of lines.slice(start + 1)) {
    if (!line.trim()) continue
    if (!/^\s/.test(line) && !(trigger === null && /^-\s/.test(line))) break
    const depth = line.match(/^(\s*)/)[1].length
    const key = line.trim().match(/^(["']?)([\w-]+)\1\s*:/)?.[2]
    const listItem = line.trim().match(/^-\s*(.*)$/)?.[1]

    if (trigger === null && listItem !== undefined) {
      const event = listItem.replace(/^["']|["']$/g, '')
      if (/^[\w-]+$/.test(event)) triggers.set(event, Object.assign(new Map(), { depth }))
      continue
    }
    if (trigger !== null && filter === null && listItem !== undefined) {
      const filters = triggers.get(trigger)
      filters.set('', [filters.get(''), line.trim()].filter(Boolean).join('\n'))
      continue
    }
    if (trigger === null || (key && depth <= triggers.get(trigger).depth)) {
      if (!key) continue
      trigger = key
      filter = null
      triggers.set(trigger, Object.assign(new Map(), { depth }))
      const inlineFilters = line.trim().replace(/^(["']?)[\w-]+\1\s*:\s*/, '')
      if (inlineFilters) triggers.get(trigger).set('', inlineFilters)
    } else if (key && (filter === null || depth <= filter.depth)) {
      filter = { depth, key }
      triggers.get(trigger).set(key, line.trim())
    } else if (filter) {
      const filters = triggers.get(trigger)
      filters.set(filter.key, `${filters.get(filter.key)}\n${line.trim()}`)
    }
  }
  return triggers
}

/**
 * One workflow's trigger item, or none. Values are compared but never emitted.
 * @param {string} file @param {string} baseSource @param {string} headSource
 * @param {'A' | 'M' | 'D' | 'R'} status
 * @returns {import('./describer-kit.mjs').Fact[]}
 */
export function workflowItems(file, baseSource, headSource, status) {
  const name = path.posix.basename(file)
  /** @param {string} text */
  const fact = (text) => item('infra', capText(text, MAX_TEXT_CHARS), [file])
  if (status === 'D') return [fact(`Workflow ${name} removed`)]
  const before = workflowTriggers(baseSource)
  const after = workflowTriggers(headSource)
  if (status === 'A') {
    return [fact(`New workflow ${name} (triggers: ${listed([...after.keys()]) || 'none read'})`)]
  }

  // Unparsed configuration is unknown, not evidence that previously read triggers disappeared.
  if (before.unreadable || after.unreadable) {
    return before.unreadable && after.unreadable && before.raw === after.raw
      ? []
      : [fact(`Workflow ${name} trigger configuration changed; triggers could not be fully read`)]
  }

  const added = [...after.keys()].filter((key) => !before.has(key))
  const removed = [...before.keys()].filter((key) => !after.has(key))
  const filters = [...after.keys()]
    .filter((key) => before.has(key))
    .flatMap((key) => {
      const from = before.get(key)
      const to = after.get(key)
      return [...new Set([...from.keys(), ...to.keys()])]
        .filter((filter) => from.get(filter) !== to.get(filter))
        .map((filter) => (filter ? `${key} ${filter}` : key))
    })
  const parts = [
    ...(added.length > 0 ? [`added ${listed(added)}`] : []),
    ...(removed.length > 0 ? [`removed ${listed(removed)}`] : []),
    ...(filters.length > 0 ? [`changed ${listed(filters)} filters`] : []),
  ]
  return parts.length > 0 ? [fact(`Workflow ${name} triggers: ${parts.join('; ')}`)] : []
}

/** @param {string | undefined} file */
const isWorkflow = (file) =>
  typeof file === 'string' && /^\.github\/workflows\/[^/]+\.ya?ml$/.test(file)

/**
 * Collect only changed YAML files directly under the workflow directory. A rename across that
 * boundary adds or removes a workflow, while a rename within it compares against its old path.
 * @type {import('./describer-kit.mjs').CollectItems}
 */
export function collectWorkflowFacts({ base, changes, git, head }) {
  return changes
    .filter((change) => isWorkflow(change.path) || isWorkflow(change.previousPath))
    .flatMap((change) => {
      const previous = change.previousPath ?? change.path
      const atBase = change.status !== 'A' && isWorkflow(previous)
      const atHead = change.status !== 'D' && isWorkflow(change.path)
      const file = atHead ? change.path : previous
      const status = !atHead ? 'D' : !atBase ? 'A' : change.status
      return workflowItems(
        file,
        atBase ? git.fileAt(base, previous) : '',
        atHead ? git.fileAt(head, change.path) : '',
        status,
      ).map((fact) => ({ ...fact, paths: [...new Set([change.path, previous])] }))
    })
}

export const describeWorkflows = envelopeDescriber(collectWorkflowFacts)
export const main = () => describerMain(describeWorkflows)

runCli(import.meta.url, 'boss-verify-workflows', collectWorkflowFacts)
