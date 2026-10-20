#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { compareBaseline, readBaseline } from './guards/baseline.mjs'
import record from './guards/registry/handwritten-grpc-glue.mjs'
import { scannedFiles } from './guards/registry.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// Tokenize comments and strings together so commented examples and strings containing
// ServiceDesc are not confused with executable glue. Go raw strings are method paths too.
const tokens =
  /\/\/[^\n]*|\/\*[\s\S]*?\*\/|"(?:\\.|[^"\\])*"|`[^`]*`|\bgrpc\s*\.\s*ServiceDesc\s*\{/g
const methodPath =
  /^\/bossanova\.v1\.(?:AgentRunnerService|TaskSourceService|WorkflowService|HostService)\//

export function findGlue(text) {
  const violations = []
  for (const match of text.matchAll(tokens)) {
    const token = match[0]
    if (token.startsWith('//') || token.startsWith('/*')) continue
    let kind
    if (token.startsWith('"') || token.startsWith('`')) {
      let value
      try {
        value = token.startsWith('`') ? token.slice(1, -1) : JSON.parse(token)
      } catch {
        continue
      }
      if (methodPath.test(value)) kind = 'literal-method-path'
    } else {
      kind = 'service-descriptor'
    }
    if (kind) {
      violations.push({ line: text.slice(0, match.index).split('\n').length, kind })
    }
  }
  return violations
}

export function checkGlue({ repoRoot = REPO_ROOT } = {}) {
  const files = scannedFiles(repoRoot, record.scans).filter(
    (file) => !file.startsWith('lib/bossalib/gen/') && fs.existsSync(path.join(repoRoot, file)),
  )
  const { entries, errors } = readBaseline(repoRoot, record.baseline)
  const failures = [...errors]
  if (files.length === 0) failures.push('no Go files scanned')
  // The migration starts without exceptions. No future baseline addition may waive glue.
  if (entries.length > 0) failures.push(`${record.baseline}: empty baseline cannot grow`)
  const offenders = files.flatMap((file) =>
    findGlue(fs.readFileSync(path.join(repoRoot, file), 'utf8')).map(
      ({ line, kind }) => `${file}:${line}:${kind}`,
    ),
  )
  failures.push(
    ...compareBaseline({
      baselinePath: record.baseline,
      entries,
      offenders,
      remedy: 'use the generated gRPC registration or FullMethodName constant',
    }).failures,
  )
  return { failures, filesScanned: files.length }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { root: { type: 'string' } },
    allowPositionals: false,
    strict: true,
  })
  const { failures, filesScanned } = checkGlue({ repoRoot: values.root })
  console.log(`Handwritten gRPC glue: ${filesScanned} files scanned`)
  for (const failure of failures) console.error(failure)
  if (failures.length > 0) process.exitCode = 1
}
