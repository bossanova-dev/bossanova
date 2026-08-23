#!/usr/bin/env node

// Guardrail: live docs that describe the local MCP server's total tool count
// must derive that count from the registered bossmcp tool set. A green run
// proves the allowlisted full-catalog count claims match the registered set. It
// does not prove gateway partition counts or the surrounding prose; those stay
// covered by their own tests and human review.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { readRegisteredToolNames } from './mcp-tool-registry.mjs'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

const LIVE_COUNT_DOCS = [
  'README.md',
  'docs/mcp.md',
  path.join('services', 'docs', 'docs', 'guides', 'mcp.md'),
]

const FULL_CATALOG_PATTERNS = [
  /\bMCP server exposes\s+\**(\d+)\s+tools\**\b/i,
  /\bserver has\s+\**(\d+)\s+tools\**\b/i,
  /\bserver exposes\s+\**(\d+)\s+tools\**\b/i,
]

export function extractFullCatalogToolCountClaims(markdown) {
  const claims = []
  const lines = markdown.split('\n')
  let inFence = false

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]
    if (/^\s*```/.test(line)) {
      inFence = !inFence
      continue
    }
    if (inFence) continue

    for (const pattern of FULL_CATALOG_PATTERNS) {
      const match = pattern.exec(line)
      if (match) {
        claims.push({ count: Number(match[1]), line: index + 1, text: line.trim() })
        break
      }
    }
  }

  return claims
}

export function checkDocsToolCounts(repoRoot = REPO_ROOT, liveDocs = LIVE_COUNT_DOCS) {
  const missingSources = []
  const registered = readRegisteredToolNames(repoRoot, missingSources)
  if (missingSources.length > 0) {
    for (const relativeSource of missingSources) {
      console.error(`Missing MCP tool source ${relativeSource}; cannot check docs tool counts.`)
    }
    return false
  }

  const expected = registered.size
  const misses = []
  let checked = 0

  for (const relativeDoc of liveDocs) {
    const docPath = path.join(repoRoot, relativeDoc)
    if (!fs.existsSync(docPath)) {
      misses.push(`${relativeDoc}: allowlisted document is missing`)
      continue
    }

    const claims = extractFullCatalogToolCountClaims(fs.readFileSync(docPath, 'utf8'))
    if (claims.length === 0) {
      misses.push(`${relativeDoc}: no recognisable full MCP tool-count claim`)
      continue
    }

    for (const claim of claims) {
      checked += 1
      if (claim.count !== expected) {
        misses.push(
          `${relativeDoc}:${claim.line}: claims ${claim.count} tools; registered set has ${expected}`,
        )
      }
    }
  }

  if (misses.length > 0) {
    console.error('Docs contain stale or missing MCP tool-count claims:')
    for (const miss of misses) console.error(miss)
    console.error(
      'Fix the live-doc claim or update the allowlist if this is not a full local-catalog count.',
    )
    return false
  }

  console.log(
    `Docs MCP tool counts OK (${checked} claims checked against ${expected} registered tools)`,
  )
  return true
}

if (isMainModule(import.meta.url)) {
  if (!checkDocsToolCounts()) process.exit(1)
}
