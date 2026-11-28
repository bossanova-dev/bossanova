#!/usr/bin/env node

// guard-id: feature-surfaces

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import {
  NAMED_SURFACES,
  REACH_SURFACES,
} from '../../.claude/skills/verify/lib/feature-surfaces.mjs'
import { readRegisteredToolNames } from '../skills/mcp-tool-registry.mjs'
import { compareBaseline, formatBaseline, readBaseline } from './baseline.mjs'
import { loadCheckedMap } from './feature-map.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const MAP = 'docs/features'
export const REFERENCES_DIR = 'lib/bossalib/contract/plugin/skillpayload/skills/boss/references'
export const BASELINE = 'scripts/guards/feature-surfaces-baseline.json'

const UNSUPPORTED_NAMED_SURFACES = new Set(['macos', 'ios'])
const FENCE = /^\s*(```|~~~)/
const HEADING = /^###\s+`([^`]+)`\s*$/
const CMD_TOKEN = /^[a-z][a-z0-9-]*$/
const USAGE_TAIL = /^[<[(]/

const OFFENDER_REMEDY =
  'declare this surface with named commands/tools in the feature frontmatter, or add a reasoned exemption under exemptions:'

/**
 * Extract visible `boss …` command paths from a generated `/boss` skill reference doc.
 *
 * @param {string} markdown
 * @returns {Set<string>}
 */
export function parseCliCommands(markdown) {
  /** @type {Set<string>} */
  const commands = new Set()
  /** @type {string | null} */
  let fence = null
  for (const line of markdown.split(/\r?\n/)) {
    const fenceMatch = FENCE.exec(line)
    if (fenceMatch) {
      if (fence === null) fence = fenceMatch[1]
      else if (fence === fenceMatch[1]) fence = null
      continue
    }
    if (fence !== null) continue
    const match = HEADING.exec(line)
    if (!match) continue
    const tokens = match[1].trim().split(/\s+/)
    if (tokens[0] !== 'boss') continue
    const parts = ['boss']
    for (let i = 1; i < tokens.length; i += 1) {
      const token = tokens[i]
      if (USAGE_TAIL.test(token) || !CMD_TOKEN.test(token)) break
      parts.push(token)
    }
    commands.add(parts.join(' '))
  }
  return commands
}

/**
 * Read every visible `boss …` command path from the generated CLI reference docs.
 *
 * @param {string} repoRoot
 * @returns {{commands: Set<string>, errors: string[]}}
 */
export function readCliCommands(repoRoot) {
  const refDir = path.join(repoRoot, REFERENCES_DIR)
  if (!fs.existsSync(refDir) || !fs.statSync(refDir).isDirectory()) {
    return {
      commands: new Set(),
      errors: [
        `Missing CLI references directory ${REFERENCES_DIR}; cannot check feature surfaces.`,
      ],
    }
  }
  const files = fs
    .readdirSync(refDir, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith('.md'))
    .map((entry) => entry.name)
    .sort()
  /** @type {Set<string>} */
  const commands = new Set()
  for (const name of files) {
    const text = fs.readFileSync(path.join(refDir, name), 'utf8')
    for (const command of parseCliCommands(text)) commands.add(command)
  }
  if (commands.size === 0) {
    return {
      commands,
      errors: [
        `Read ${files.length} reference file(s) under ${REFERENCES_DIR} but found zero boss commands.`,
      ],
    }
  }
  return { commands, errors: [] }
}

/**
 * Evaluate feature surface reachability, resolution, exemptions, withheld records and baseline.
 *
 * @param {{root?: string, update?: boolean}} [options]
 * @returns {{
 *   ok: boolean,
 *   messages: string[],
 *   offenders: string[],
 *   featuresChecked: number,
 *   commandsResolved: number,
 *   toolsResolved: number,
 * }}
 */
export function checkFeatureSurfaces({ root = ROOT, update = false } = {}) {
  const { map, failures: mapFailures } = loadCheckedMap(root)
  if (mapFailures.length > 0) {
    return {
      ok: false,
      messages: mapFailures,
      offenders: [],
      featuresChecked: 0,
      commandsResolved: 0,
      toolsResolved: 0,
    }
  }

  const { commands, errors: cliErrors } = readCliCommands(root)
  /** @type {string[]} */
  const missingToolSources = []
  const tools = readRegisteredToolNames(root, missingToolSources, { includeHosted: true })
  /** @type {string[]} */
  const tripwireErrors = [...cliErrors]
  for (const source of missingToolSources) {
    tripwireErrors.push(`Missing MCP tool source ${source}; cannot check feature surfaces.`)
  }
  if (missingToolSources.length === 0 && tools.size === 0) {
    tripwireErrors.push('Found zero registered MCP tools; cannot check feature surfaces.')
  }
  if (tripwireErrors.length > 0) {
    return {
      ok: false,
      messages: tripwireErrors,
      offenders: [],
      featuresChecked: map.features.length,
      commandsResolved: commands.size,
      toolsResolved: tools.size,
    }
  }

  /** @type {string[]} */
  const hardFailures = []
  /** @type {string[]} */
  const offenders = []
  /** @type {readonly string[]} */
  const namedSurfaceSet = NAMED_SURFACES

  for (const feature of map.features) {
    const file = `${MAP}/${feature.file}`
    /** @type {string[]} */
    const surfaces = feature.surfaces
    /** @type {Record<string, string[]>} */
    const names = feature.reach.names
    /** @type {Record<string, {exempt: string, reviewed: string}>} */
    const exemptions = feature.reach.exemptions
    /** @type {Record<string, {surface: string, command: string, exempt: string, reviewed: string}>} */
    const withheld = feature.reach.withheld

    for (const surface of REACH_SURFACES) {
      const satisfied =
        (surfaces.includes(surface) && (names[surface]?.length ?? 0) > 0) ||
        Object.hasOwn(exemptions, surface)
      if (!satisfied) offenders.push(`${feature.id} needs ${surface}`)
    }

    for (const surface of NAMED_SURFACES) {
      const surfaceNames = names[surface] ?? []
      if (surfaceNames.length === 0) continue
      if (!surfaces.includes(surface)) {
        hardFailures.push(`${file}: ${surface} names declared without ${surface} in surfaces`)
      }
      if (UNSUPPORTED_NAMED_SURFACES.has(surface)) {
        hardFailures.push(
          `${file}: ${surface} name lists are rejected until BOS-2672 adds a native resolver`,
        )
      }
    }

    for (const surface of Object.keys(exemptions)) {
      if (surfaces.includes(surface) || (names[surface]?.length ?? 0) > 0) {
        hardFailures.push(
          `${file}: exemptions.${surface} cannot be set when ${surface} is declared or named`,
        )
      }
    }

    for (const command of names.cli ?? []) {
      if (!commands.has(command)) {
        hardFailures.push(
          `${file}: cli command ${JSON.stringify(command)} is not a rendered boss command`,
        )
      }
    }

    for (const tool of names.mcp ?? []) {
      if (!tools.has(tool)) {
        hardFailures.push(
          `${file}: mcp tool ${JSON.stringify(tool)} is not a registered bossmcp tool`,
        )
      }
    }

    const ownCli = new Set(names.cli ?? [])
    for (const [slug, entry] of Object.entries(withheld)) {
      if (!namedSurfaceSet.includes(entry.surface)) {
        hardFailures.push(
          `${file}: withheld.${slug}.surface ${JSON.stringify(entry.surface)} must be one of ${NAMED_SURFACES.join(', ')}`,
        )
      }
      if (!ownCli.has(entry.command)) {
        hardFailures.push(
          `${file}: withheld.${slug}.command ${JSON.stringify(entry.command)} is not in this feature's cli list`,
        )
      }
    }
  }

  const sortedOffenders = [...new Set(offenders)].sort()

  if (update) {
    if (hardFailures.length > 0) {
      return {
        ok: false,
        messages: hardFailures,
        offenders: sortedOffenders,
        featuresChecked: map.features.length,
        commandsResolved: commands.size,
        toolsResolved: tools.size,
      }
    }
    const baselineFile = path.join(root, BASELINE)
    fs.mkdirSync(path.dirname(baselineFile), { recursive: true })
    fs.writeFileSync(baselineFile, formatBaseline(sortedOffenders))
    return {
      ok: true,
      messages: [],
      offenders: sortedOffenders,
      featuresChecked: map.features.length,
      commandsResolved: commands.size,
      toolsResolved: tools.size,
    }
  }

  const baseline = readBaseline(root, BASELINE)
  const comparison = compareBaseline({
    baselinePath: BASELINE,
    entries: baseline.entries,
    offenders: sortedOffenders,
    remedy: OFFENDER_REMEDY,
  })
  const messages = [...hardFailures, ...baseline.errors, ...comparison.failures]
  return {
    ok: messages.length === 0,
    messages,
    offenders: sortedOffenders,
    featuresChecked: map.features.length,
    commandsResolved: commands.size,
    toolsResolved: tools.size,
  }
}

/**
 * CLI entry point.
 *
 * @param {string[]} [args]
 * @returns {number}
 */
export function main(args = process.argv.slice(2)) {
  const { values } = parseArgs({
    args,
    strict: true,
    allowPositionals: false,
    options: {
      root: { type: 'string', default: ROOT },
      check: { type: 'boolean', default: false },
      update: { type: 'boolean', default: false },
    },
  })
  if (values.check && values.update) {
    throw new Error('usage: feature-surfaces.mjs [--check|--update] [--root <path>]')
  }
  const root = path.resolve(typeof values.root === 'string' ? values.root : ROOT)
  const update = Boolean(values.update)
  const result = checkFeatureSurfaces({ root, update })
  if (!result.ok) {
    for (const message of result.messages) console.error(message)
    return 1
  }
  if (update) {
    console.log(
      `Wrote ${BASELINE} (${result.offenders.length} entries; ${result.featuresChecked} features checked, ${result.commandsResolved} CLI commands resolved, ${result.toolsResolved} MCP tools resolved)`,
    )
  } else {
    console.log(
      `Feature surfaces OK (${result.featuresChecked} features checked, ${result.commandsResolved} CLI commands resolved, ${result.toolsResolved} MCP tools resolved, ${result.offenders.length} baselined)`,
    )
  }
  return 0
}

if (isMainModule(import.meta.url)) {
  try {
    process.exitCode = main()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
