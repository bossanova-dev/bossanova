#!/usr/bin/env node

// Guardrail: when a Go exec.Cmd captures stdout or stderr into a non-*os.File
// writer, os/exec creates a pipe and Run/Wait does not return until every
// writer inherited by descendants closes. exec.CommandContext kills the direct
// child on cancellation, but it does not bound the pipe drain. A surviving
// grandchild can therefore make the caller hang unless cmd.WaitDelay is set.
//
// This is a deliberately conservative regex gate, not a Go type checker. It
// proves a WaitDelay assignment is present near the command construction; it
// does not prove the chosen duration is correct. It also cannot follow a command
// configured across functions, so command-like variables with captured output
// and no local construction are flagged rather than passed. Deliberate
// exceptions live in ALLOWLIST below, each with a non-empty reason, so the
// exception set is enumerable and reviewable.
//
// Exercised by scripts/check-exec-waitdelay.test.mjs and runnable via
// `node scripts/check-exec-waitdelay.mjs [root...]`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const scriptDir = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(scriptDir, '..')

const DEFAULT_ROOTS = [
  path.join(repoRoot, 'lib'),
  path.join(repoRoot, 'services'),
  path.join(repoRoot, 'plugins'),
]

const SKIP_DIRS = new Set(['.git', 'node_modules', 'vendor', 'gen', 'testdata', 'mock', 'mocks'])

const GENERATED_MARKERS = ['Code generated', 'DO NOT EDIT', 'protoc-gen-go', 'connect-go']

export const ALLOWLIST = []

const EXEC_CONSTRUCTION_RE =
  /\b(?<variable>[A-Za-z_][A-Za-z0-9_]*)\s*(?::=|=)\s*(?:(?:[A-Za-z_][A-Za-z0-9_]*\.)*)?exec\.Command(?:Context)?\s*\(/g

const UNKNOWN_CONSTRUCTION_RE =
  /\b(?<variable>[A-Za-z_][A-Za-z0-9_]*)\s*(?::=|=)\s*(?!range\b|append\b|make\b|new\b|len\b|cap\b|copy\b|delete\b|complex\b|real\b|imag\b|recover\b|panic\b|fmt\.|strings\.|bytes\.|errors\.|context\.|time\.|filepath\.|path\.|os\.|exec\.)[A-Za-z_][A-Za-z0-9_.]*\s*\(/g

const OUTPUT_ASSIGNMENT_RE =
  /\b(?<variable>[A-Za-z_][A-Za-z0-9_]*)\.(?<stream>Stdout|Stderr)\s*=\s*(?<value>[^\n]+)/g

const WAIT_DELAY_RE = /\b(?<variable>[A-Za-z_][A-Za-z0-9_]*)\.WaitDelay\s*=/

function relativePath(file) {
  return path.relative(repoRoot, file).split(path.sep).join('/')
}

function walk(dir, files) {
  let entries
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true })
  } catch {
    return
  }

  for (const entry of entries) {
    const full = path.join(dir, entry.name)
    if (entry.isDirectory()) {
      if (SKIP_DIRS.has(entry.name)) continue
      walk(full, files)
    } else if (entry.isFile() && entry.name.endsWith('.go') && !entry.name.endsWith('_test.go')) {
      files.push(full)
    }
  }
}

function isGenerated(contents) {
  const header = contents.split('\n').slice(0, 12).join('\n')
  return GENERATED_MARKERS.some((marker) => header.includes(marker))
}

function lineNumberAt(contents, index) {
  let line = 1
  for (let i = 0; i < index; i += 1) {
    if (contents.charCodeAt(i) === 10) line += 1
  }
  return line
}

function findFunctionBodies(contents) {
  const bodies = []
  const funcRe = /\bfunc\s+(?:\([^)]*\)\s*)?(?<name>[A-Za-z_][A-Za-z0-9_]*)\s*\([^)]*\)[^{]*\{/g
  let match
  while ((match = funcRe.exec(contents)) !== null) {
    const open = contents.indexOf('{', match.index)
    const end = findMatchingBrace(contents, open)
    if (end === -1) continue
    bodies.push({
      name: match.groups.name,
      start: match.index,
      startLine: lineNumberAt(contents, open + 1),
      bodyStart: open + 1,
      end,
      text: contents.slice(open + 1, end),
    })
    funcRe.lastIndex = end + 1
  }
  return bodies
}

function findMatchingBrace(contents, open) {
  let depth = 0
  let quote = null
  let escaped = false
  for (let i = open; i < contents.length; i += 1) {
    const ch = contents[i]
    const next = contents[i + 1]
    if (quote) {
      if (quote !== '`' && escaped) {
        escaped = false
      } else if (quote !== '`' && ch === '\\') {
        escaped = true
      } else if (ch === quote) {
        quote = null
      }
      continue
    }
    if (ch === '"' || ch === "'" || ch === '`') {
      quote = ch
      continue
    }
    if (ch === '/' && next === '/') {
      const newline = contents.indexOf('\n', i + 2)
      if (newline === -1) break
      i = newline
      continue
    }
    if (ch === '/' && next === '*') {
      const close = contents.indexOf('*/', i + 2)
      if (close === -1) break
      i = close + 1
      continue
    }
    if (ch === '{') depth += 1
    if (ch === '}') {
      depth -= 1
      if (depth === 0) return i
    }
  }
  return -1
}

function isFileWriter(value) {
  const trimmed = value
    .trim()
    .replace(/\/\/.*$/, '')
    .trim()
  return (
    trimmed === 'nil' ||
    trimmed === 'os.Stdout' ||
    trimmed === 'os.Stderr' ||
    trimmed === 'os.Stdin' ||
    /^os\.(NewFile|Open|OpenFile|Create)\b/.test(trimmed) ||
    /^&?os\.File\b/.test(trimmed)
  )
}

function collectCommandFacts(body) {
  const events = []
  for (const match of body.text.matchAll(EXEC_CONSTRUCTION_RE)) {
    events.push({
      type: 'construction',
      construction: 'exec',
      variable: match.groups.variable,
      index: match.index,
      line: lineNumberAt(body.text, match.index),
    })
  }

  for (const match of body.text.matchAll(UNKNOWN_CONSTRUCTION_RE)) {
    events.push({
      type: 'construction',
      construction: 'unresolved',
      variable: match.groups.variable,
      index: match.index,
      line: lineNumberAt(body.text, match.index),
    })
  }

  for (const match of body.text.matchAll(OUTPUT_ASSIGNMENT_RE)) {
    const value = match.groups.value
    if (isFileWriter(value)) continue
    events.push({
      type: 'assignment',
      variable: match.groups.variable,
      index: match.index,
      stream: match.groups.stream,
      value: value.trim(),
      line: lineNumberAt(body.text, match.index),
    })
  }

  for (const match of body.text.matchAll(new RegExp(WAIT_DELAY_RE.source, 'g'))) {
    events.push({
      type: 'waitdelay',
      variable: match.groups.variable,
      index: match.index,
      line: lineNumberAt(body.text, match.index),
    })
  }

  events.sort((left, right) => left.index - right.index)

  const facts = []
  const current = new Map()
  const startFact = (event) => {
    const existing = current.get(event.variable)
    if (existing) facts.push(existing)
    const fact = {
      variable: event.variable,
      construction: event.construction,
      constructionLine: event.line,
      assignments: [],
      hasWaitDelay: false,
    }
    current.set(event.variable, fact)
    return fact
  }
  const ensure = (variable) => {
    if (!current.has(variable)) {
      current.set(variable, {
        variable,
        construction: 'unresolved',
        constructionLine: null,
        assignments: [],
        hasWaitDelay: false,
      })
    }
    return current.get(variable)
  }

  for (const event of events) {
    if (event.type === 'construction') {
      startFact(event)
      continue
    }
    const fact = ensure(event.variable)
    if (event.type === 'assignment') {
      fact.assignments.push({
        stream: event.stream,
        value: event.value,
        line: event.line,
      })
    } else {
      fact.hasWaitDelay = true
    }
  }
  facts.push(...current.values())

  return facts.filter((fact) => fact.assignments.length > 0)
}

function allowlistKey({ file, functionName, variable }) {
  return `${file}\0${functionName}\0${variable}`
}

export function validateAllowlist(allowlist = ALLOWLIST) {
  const errors = []
  const seen = new Set()
  for (const entry of allowlist) {
    const missing = ['file', 'function', 'variable'].filter((key) => !entry[key])
    if (missing.length > 0) {
      errors.push(`allowlist entry is missing ${missing.join(', ')}`)
      continue
    }
    if (!entry.reason || entry.reason.trim() === '') {
      errors.push(`${entry.file}:${entry.function}:${entry.variable} has an empty reason`)
    }
    const key = allowlistKey({
      file: entry.file,
      functionName: entry.function,
      variable: entry.variable,
    })
    if (seen.has(key))
      errors.push(`${entry.file}:${entry.function}:${entry.variable} is duplicated`)
    seen.add(key)
  }
  return errors
}

function buildAllowlistMap(allowlist) {
  return new Map(
    allowlist.map((entry) => [
      allowlistKey({
        file: entry.file,
        functionName: entry.function,
        variable: entry.variable,
      }),
      entry,
    ]),
  )
}

export function scanExecWaitDelay({ roots = DEFAULT_ROOTS, allowlist = ALLOWLIST } = {}) {
  const offenders = []
  const allowlistErrors = validateAllowlist(allowlist)
  for (const error of allowlistErrors) {
    offenders.push({ kind: 'allowlist', file: '(allowlist)', line: 0, message: error })
  }
  const allowed = buildAllowlistMap(allowlist)

  const files = []
  for (const root of roots) {
    walk(root, files)
  }

  for (const file of files.sort()) {
    const contents = fs.readFileSync(file, 'utf8')
    if (isGenerated(contents)) continue
    const rel = relativePath(file)
    for (const body of findFunctionBodies(contents)) {
      for (const fact of collectCommandFacts(body)) {
        if (fact.assignments.length === 0 || fact.hasWaitDelay) continue
        const key = allowlistKey({ file: rel, functionName: body.name, variable: fact.variable })
        if (allowed.has(key)) continue
        const assignment = fact.assignments[0]
        const unresolved = fact.construction !== 'exec'
        offenders.push({
          kind: unresolved ? 'unresolved' : 'missing-waitdelay',
          file: rel,
          function: body.name,
          variable: fact.variable,
          line: body.startLine + assignment.line - 1,
          message: unresolved
            ? `${rel}:${body.startLine + assignment.line - 1} ${body.name}: ${fact.variable}.${assignment.stream} captures output, but ${fact.variable} is not locally resolved to exec.Command* and has no WaitDelay`
            : `${rel}:${body.startLine + assignment.line - 1} ${body.name}: ${fact.variable}.${assignment.stream} captures output without ${fact.variable}.WaitDelay`,
        })
      }
    }
  }

  return offenders
}

export function checkExecWaitDelay(options = {}) {
  const offenders = scanExecWaitDelay(options)
  if (offenders.length > 0) {
    console.error('Found exec.Cmd captured-output sites without a recorded WaitDelay verdict:')
    for (const offender of offenders) {
      console.error(`  - ${offender.message}`)
    }
    console.error(
      'Remedy: set cmd.WaitDelay where cancellation must bound Run/Wait, or add a narrow allowlist entry with a reason.',
    )
    return false
  }
  console.log('OK: exec.Cmd captured-output sites have WaitDelay or an allowlist verdict')
  return true
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  const roots = process.argv.slice(2)
  if (!checkExecWaitDelay({ roots: roots.length > 0 ? roots : DEFAULT_ROOTS })) process.exit(1)
}
