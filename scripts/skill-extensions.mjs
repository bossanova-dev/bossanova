#!/usr/bin/env node
// Compatibility entrypoint for repository-local extension documentation.
// The existing helper remains authoritative for established roles. Notes is
// registered here because this repository-local entrypoint owns the extension
// contract used by end-of-run consumers.
import fs from 'node:fs'
import path from 'node:path'
import { isMainModule } from '../skills-toolbox/main-module.mjs'
import {
  ROLE_SCHEMAS as toolboxRoleSchemas,
  discoverExtensions as discoverToolboxExtensions,
  main as toolboxMain,
  validateResult as validateToolboxResult,
} from '../skills-toolbox/skill-extensions.mjs'

const KNOWN_EXTENSION_ROLES = new Set([
  'lens',
  'round',
  'surface',
  'plan-reviewer',
  'agent-driver',
  'draft',
  'methodology',
  'notes',
])

export const ROLE_SCHEMAS = {
  ...toolboxRoleSchemas,
  notes: ['tag', 'body', 'noteId'],
}

export function discoverExtensions({ core, root, role }) {
  if (typeof role !== 'string' || role === '' || !KNOWN_EXTENSION_ROLES.has(role)) {
    return discoverToolboxExtensions({ core, root, role })
  }

  const result = discoverToolboxExtensions({ core, root })
  const extensions = []
  const skipped = [...result.skipped]
  for (const extension of result.extensions) {
    if (extension.role === role) {
      extensions.push(extension)
    } else if (!KNOWN_EXTENSION_ROLES.has(extension.role)) {
      skipped.push({ name: extension.name, reason: `role "${extension.role}", not "${role}"` })
    }
  }
  return { extensions, skipped }
}

export function validateResult(envelope, role) {
  if (role !== 'notes') return validateToolboxResult(envelope, role)

  const errors = []
  if (!envelope || typeof envelope !== 'object') {
    return { ok: false, errors: ['envelope is not an object'] }
  }
  if (typeof envelope.ok !== 'boolean') {
    errors.push('ok is not a boolean')
  } else if (envelope.ok === false) {
    const reason =
      typeof envelope.error === 'string' && envelope.error.trim() !== ''
        ? envelope.error.trim()
        : 'no error detail provided'
    errors.push(`extension reported failure (ok:false): ${reason}`)
  }
  if (typeof envelope.extension !== 'string' || envelope.extension === '') {
    errors.push('extension is not a non-empty string')
  }
  if (envelope.role !== role) {
    errors.push(`envelope role "${envelope.role}" does not match expected "${role}"`)
  }
  if (!Array.isArray(envelope.items)) {
    errors.push('items is not an array')
    return { ok: false, errors }
  }
  envelope.items.forEach((item, idx) => {
    if (!item || typeof item !== 'object') {
      errors.push(`item ${idx} is not an object`)
      return
    }
    for (const key of ROLE_SCHEMAS.notes) {
      if (!(key in item)) errors.push(`item ${idx} missing "${key}"`)
    }
  })
  return { ok: errors.length === 0, errors }
}

function parseArgs(argv) {
  const args = {}
  for (let i = 0; i < argv.length; i += 1) {
    const token = argv[i]
    if (!token.startsWith('--')) continue
    const key = token.slice(2)
    const next = argv[i + 1]
    if (next === undefined || next.startsWith('--')) {
      args[key] = true
    } else {
      args[key] = next
      i += 1
    }
  }
  return args
}

export function main(argv) {
  const [subcommand, ...rest] = argv
  const args = parseArgs(rest)

  if (
    subcommand === 'discover' &&
    typeof args.role === 'string' &&
    KNOWN_EXTENSION_ROLES.has(args.role)
  ) {
    if (typeof args.core !== 'string' || args.core === '') {
      process.stderr.write('discover: --core <name> is required\n')
      return 2
    }
    const result = discoverExtensions({
      core: args.core,
      root: typeof args.root === 'string' ? args.root : process.cwd(),
      role: args.role,
    })
    if (args.json) process.stdout.write(`${JSON.stringify(result)}\n`)
    else
      for (const ext of result.extensions)
        process.stdout.write(`${ext.name}\t${ext.role}\t${ext.order}\n`)
    return 0
  }
  if (args.role !== 'notes') return toolboxMain(argv)
  if (subcommand === 'validate') {
    let source
    try {
      source =
        typeof args.file === 'string'
          ? fs.readFileSync(args.file, 'utf8')
          : fs.readFileSync(0, 'utf8')
    } catch (err) {
      process.stdout.write(
        `${JSON.stringify({ ok: false, errors: [`cannot read input: ${err.message}`] })}\n`,
      )
      return 1
    }
    let envelope
    try {
      envelope = JSON.parse(source)
    } catch (err) {
      process.stdout.write(
        `${JSON.stringify({ ok: false, errors: [`invalid JSON: ${err.message}`] })}\n`,
      )
      return 1
    }
    const result = validateResult(envelope, 'notes')
    process.stdout.write(`${JSON.stringify(result)}\n`)
    return result.ok ? 0 : 1
  }
  return toolboxMain(argv)
}

if (isMainModule(import.meta.url)) {
  process.exit(main(process.argv.slice(2)))
}
