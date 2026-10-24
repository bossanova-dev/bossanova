#!/usr/bin/env node

// Ratchet proto field names against the shared baseline; existing naming debt only shrinks.
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { compareBaseline, readBaseline } from './baseline.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const PROTO_DIR = path.join('proto', 'bossanova', 'v1')
const BASELINE_PATH = 'scripts/guards/proto-field-names-baseline.json'
const REQUIRED_PREFIXES = ['is_', 'has_', 'should_', 'can_']

export function stripLineComments(line, inBlockComment) {
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

export function discoverProtoFiles(repoRoot = REPO_ROOT) {
  const protoRoot = path.join(repoRoot, PROTO_DIR)
  if (!fs.existsSync(protoRoot)) return []
  return fs
    .readdirSync(protoRoot, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith('.proto'))
    .map((entry) => path.join(protoRoot, entry.name))
    .sort()
}

export function extractFields(protoSource, relativeFile) {
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

    const match =
      /^\s*(?:(optional|repeated)\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*\d+\b/.exec(
        stripped.line,
      )
    if (match && !['option', 'reserved'].includes(match[2])) {
      const message = messageStack.map((entry) => entry.name).join('.') || '<root>'
      fields.push({
        file: relativeFile,
        line: index + 1,
        message,
        type: match[2],
        isRepeated: match[1] === 'repeated',
        name: match[3],
        key: `${relativeFile}:${message}.${match[3]}`,
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

const integerType = /^(?:u?int(?:32|64)|sint(?:32|64)|s?fixed(?:32|64))$/
const durationSegments = new Set([
  'timeout',
  'duration',
  'interval',
  'delay',
  'ttl',
  'elapsed',
  'latency',
  'backoff',
  'cooldown',
  'window',
])
const noncanonicalUnits = new Set([
  'sec',
  'secs',
  'millis',
  'milliseconds',
  'msec',
  'msecs',
  'us',
  'ns',
  'mins',
  'minutes',
  'hours',
  'days',
  'kb',
  'mb',
  'gb',
])

export function boolPrefix(field) {
  if (field.type === 'bool' && !REQUIRED_PREFIXES.some((prefix) => field.name.startsWith(prefix))) {
    return 'must start with is_/has_/should_/can_'
  }
}
export function timestampSuffix(field) {
  if (field.type !== 'google.protobuf.Timestamp' || /_(at|on)$/.test(field.name)) return
  const isBound =
    /^(since|until|before|after|older_than|newer_than)$/.test(field.name) ||
    /_(after|before|since|until)$/.test(field.name)
  if (/(Request|Command)$/.test(field.message) && isBound) return
  return 'must end in _at or _on (except request/command filter bounds)'
}
export function countSuffix(field) {
  if (!integerType.test(field.type) || field.isRepeated || field.name.endsWith('_count')) return
  const last = field.name.split('_').at(-1)
  if (['seconds', 'ms', 'bytes', 'cols', 'rows', 'width', 'height'].includes(last)) return
  if (field.name.startsWith('num_') || (/s$/.test(last) && !/(ss|us|is)$/.test(last)))
    return 'counts must end in _count'
}
export function unitSuffix(field) {
  if (!integerType.test(field.type) && !['double', 'float'].includes(field.type)) return
  const segments = field.name.split('_')
  if (noncanonicalUnits.has(segments.at(-1))) return 'use canonical _seconds, _ms or _bytes units'
  if (segments.some((part) => durationSegments.has(part)) && !/_(seconds|ms)$/.test(field.name))
    return 'durations must end in _seconds or _ms'
  if (segments.includes('size') && field.name !== 'page_size' && !field.name.endsWith('_bytes'))
    return 'sizes must end in _bytes'
}
const rules = {
  'bool-prefix': boolPrefix,
  'timestamp-suffix': timestampSuffix,
  'count-suffix': countSuffix,
  'unit-suffix': unitSuffix,
}

export function checkProtoFieldNames(repoRoot = REPO_ROOT) {
  const { entries, errors } = readBaseline(repoRoot, BASELINE_PATH)
  const fields = discoverProtoFiles(repoRoot).flatMap((file) =>
    extractFields(fs.readFileSync(file, 'utf8'), path.relative(repoRoot, file)),
  )
  const violations = new Map()
  const counts = Object.fromEntries(Object.keys(rules).map((rule) => [rule, 0]))
  for (const field of fields) {
    for (const [rule, check] of Object.entries(rules)) {
      const problem = check(field)
      if (!problem) continue
      counts[rule] += 1
      const message = `${field.file}:${field.line}: ${rule}: "${field.message}.${field.name}" ${problem}`
      const previous = violations.get(field.key) || []
      violations.set(field.key, [...previous, message])
    }
  }
  const comparison = compareBaseline({
    baselinePath: BASELINE_PATH,
    entries,
    offenders: violations.keys(),
    remedy: (key) => violations.get(key).join('\n'),
  })
  const failures = [
    ...errors,
    ...comparison.newViolations.flatMap((key) => violations.get(key)),
    ...comparison.failures.slice(comparison.newViolations.length),
  ]
  if (failures.length) {
    console.error('Proto field name check failed:')
    for (const failure of failures) console.error(failure)
    return false
  }
  console.log(
    `Proto field names OK (${fields.length} fields checked, ${entries.length} entries baselined; ${Object.entries(
      counts,
    )
      .map(([rule, count]) => `${rule}: ${count}`)
      .join(', ')})`,
  )
  return true
}
if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, strict: true, allowPositionals: false })
  if (!checkProtoFieldNames()) process.exitCode = 1
}
