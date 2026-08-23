#!/usr/bin/env node

// Ratchet for proto boolean field names. The database-design naming standard
// applies to proto/RPC fields too, but the v1 protos contain legacy bools that
// predate it. New bools must start with is_/has_/should_/can_; existing
// offenders live in a flat allowlist, and stale allowlist entries fail so the
// exemptions shrink when fields are renamed.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const PROTO_DIR = path.join('proto', 'bossanova', 'v1')
const ALLOWLIST_PATH = path.join('scripts', 'proto-bool-prefix-allowlist.json')
const REQUIRED_PREFIXES = ['is_', 'has_', 'should_', 'can_']

function stripLineComments(line, inBlockComment) {
  let out = ''
  let index = 0
  let inBlock = inBlockComment

  while (index < line.length) {
    if (inBlock) {
      const end = line.indexOf('*/', index)
      if (end === -1) return { line: out, inBlock: true }
      index = end + 2
      inBlock = false
      continue
    }

    const blockStart = line.indexOf('/*', index)
    const lineStart = line.indexOf('//', index)
    const nextComment = [blockStart, lineStart]
      .filter((value) => value !== -1)
      .sort((a, b) => a - b)[0]

    if (nextComment === undefined) {
      out += line.slice(index)
      break
    }

    out += line.slice(index, nextComment)
    if (nextComment === lineStart) break
    index = nextComment + 2
    inBlock = true
  }

  return { line: out, inBlock }
}

function readAllowlist(repoRoot = REPO_ROOT) {
  const allowlistFile = path.join(repoRoot, ALLOWLIST_PATH)
  if (!fs.existsSync(allowlistFile)) {
    return { entries: [], errors: [`Missing ${ALLOWLIST_PATH}`] }
  }

  let parsed
  try {
    parsed = JSON.parse(fs.readFileSync(allowlistFile, 'utf8'))
  } catch (error) {
    return { entries: [], errors: [`Invalid ${ALLOWLIST_PATH}: ${error.message}`] }
  }

  const entries = Array.isArray(parsed) ? parsed : parsed.allowed
  if (!Array.isArray(entries)) {
    return { entries: [], errors: [`${ALLOWLIST_PATH} must be an array or object.allowed array`] }
  }

  const errors = []
  const seen = new Set()
  for (const entry of entries) {
    if (
      typeof entry !== 'string' ||
      !/^[^:\n]+:[A-Za-z_][A-Za-z0-9_.]*\.[A-Za-z_][A-Za-z0-9_]*$/.test(entry)
    ) {
      errors.push(
        `${ALLOWLIST_PATH}: invalid entry ${JSON.stringify(entry)}; want "path:Message.field"`,
      )
      continue
    }
    if (seen.has(entry)) {
      errors.push(`${ALLOWLIST_PATH}: duplicate entry ${entry}`)
      continue
    }
    seen.add(entry)
  }

  return { entries, errors }
}

export function discoverProtoFiles(repoRoot = REPO_ROOT) {
  const protoRoot = path.join(repoRoot, PROTO_DIR)
  if (!fs.existsSync(protoRoot)) return []
  return fs
    .readdirSync(protoRoot, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith('.proto'))
    .map((entry) => path.join(protoRoot, entry.name))
    .sort()
}

export function extractBoolFields(protoSource, relativeFile) {
  const fields = []
  const lines = protoSource.split('\n')
  let inBlockComment = false
  let depth = 0
  const messageStack = []

  for (let index = 0; index < lines.length; index += 1) {
    const stripped = stripLineComments(lines[index], inBlockComment)
    inBlockComment = stripped.inBlock
    const messageMatch = /^\s*message\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{/.exec(stripped.line)
    if (messageMatch) {
      messageStack.push({ name: messageMatch[1], depth: depth + 1 })
    }

    const match = /^\s*(?:optional\s+)?bool\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*\d+\b/.exec(
      stripped.line,
    )
    if (match) {
      const message = messageStack.map((entry) => entry.name).join('.') || '<root>'
      fields.push({
        file: relativeFile,
        line: index + 1,
        message,
        name: match[1],
        key: `${relativeFile}:${message}.${match[1]}`,
      })
    }

    const opens = (stripped.line.match(/\{/g) || []).length
    const closes = (stripped.line.match(/\}/g) || []).length
    depth += opens - closes
    while (messageStack.length > 0 && messageStack[messageStack.length - 1].depth > depth) {
      messageStack.pop()
    }
  }

  return fields
}

export function checkProtoBoolPrefixes(repoRoot = REPO_ROOT) {
  const { entries, errors } = readAllowlist(repoRoot)
  const allowed = new Set(entries)
  const fields = []

  for (const file of discoverProtoFiles(repoRoot)) {
    const relativeFile = path.relative(repoRoot, file)
    fields.push(...extractBoolFields(fs.readFileSync(file, 'utf8'), relativeFile))
  }

  const liveKeys = new Set(fields.map((field) => field.key))
  const failures = [...errors]

  for (const field of fields) {
    if (REQUIRED_PREFIXES.some((prefix) => field.name.startsWith(prefix))) continue
    if (allowed.has(field.key)) continue
    failures.push(
      `${field.file}:${field.line}: bool field "${field.message}.${field.name}" must start with ${REQUIRED_PREFIXES.join(
        '/',
      )} or be listed in ${ALLOWLIST_PATH}`,
    )
  }

  for (const entry of entries) {
    if (!liveKeys.has(entry)) {
      failures.push(`${ALLOWLIST_PATH}: stale entry ${entry} does not match a live bool field`)
    }
  }

  if (failures.length > 0) {
    console.error('Proto bool prefix check failed:')
    for (const failure of failures) console.error(failure)
    return false
  }

  console.log(
    `Proto bool prefixes OK (${fields.length} bool fields checked, ${entries.length} legacy fields allowlisted)`,
  )
  return true
}

if (isMainModule(import.meta.url)) {
  if (!checkProtoBoolPrefixes()) process.exit(1)
}
