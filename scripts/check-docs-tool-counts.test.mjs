#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  checkDocsToolCounts,
  extractFullCatalogToolCountClaims,
} from './check-docs-tool-counts.mjs'

function captureConsole(run) {
  const out = []
  const err = []
  const originalLog = console.log
  const originalError = console.error
  console.log = (...args) => out.push(args.join(' '))
  console.error = (...args) => err.push(args.join(' '))
  try {
    const result = run()
    return { result, out, err }
  } finally {
    console.log = originalLog
    console.error = originalError
  }
}

function makeTempRepo(name) {
  return fs.mkdtempSync(path.join(os.tmpdir(), `${name}-`))
}

function writeToolSources(repoRoot, names) {
  const bossmcp = path.join(repoRoot, 'lib', 'bossalib', 'bossmcp')
  fs.mkdirSync(bossmcp, { recursive: true })
  const body = (registered) =>
    registered
      .map((name) => `\taddTool(server, opts, &mcp.Tool{\n\t\tName:        "${name}",\n\t})\n`)
      .join('')
  fs.writeFileSync(path.join(bossmcp, 'tools.go'), body(names))
  fs.writeFileSync(path.join(bossmcp, 'tools_mutating.go'), '')
  fs.writeFileSync(path.join(bossmcp, 'tools_destructive.go'), '')
}

function writeDoc(repoRoot, relativePath, contents) {
  const docPath = path.join(repoRoot, relativePath)
  fs.mkdirSync(path.dirname(docPath), { recursive: true })
  fs.writeFileSync(docPath, contents)
}

test('extractFullCatalogToolCountClaims reads plain and bold forms', () => {
  assert.deepEqual(
    extractFullCatalogToolCountClaims(
      [
        'The MCP server exposes 69 tools across read-only, mutating, and destructive tiers.',
        'The server exposes **69 tools** in three tiers:',
        'The server has **69 tools** in three tiers:',
      ].join('\n'),
    ).map(({ count, line }) => ({ count, line })),
    [
      { count: 69, line: 1 },
      { count: 69, line: 2 },
      { count: 69, line: 3 },
    ],
  )
})

test('extractFullCatalogToolCountClaims ignores fenced examples and partition counts', () => {
  const markdown = [
    '```md',
    'The server exposes 12 tools in three tiers.',
    '```',
    '',
    'The gateway registers a 50-tool proxiable subset.',
    'The 19 tools that remain local-only are documented elsewhere.',
    'The server exposes **2 tools** in three tiers:',
  ].join('\n')

  assert.deepEqual(extractFullCatalogToolCountClaims(markdown), [
    { count: 2, line: 7, text: 'The server exposes **2 tools** in three tiers:' },
  ])
})

test('checkDocsToolCounts passes when allowlisted claims match the registered count', () => {
  const repoRoot = makeTempRepo('check-docs-tool-counts')
  writeToolSources(repoRoot, ['list_sessions', 'create_session'])
  writeDoc(repoRoot, 'README.md', 'The MCP server exposes 2 tools across tiers.\n')

  const { result, out } = captureConsole(() => checkDocsToolCounts(repoRoot, ['README.md']))

  assert.equal(result, true)
  assert.match(out.join('\n'), /1 claims checked against 2 registered tools/)
})

test('checkDocsToolCounts fails stale claims with file and line', () => {
  const repoRoot = makeTempRepo('check-docs-tool-counts')
  writeToolSources(repoRoot, ['list_sessions', 'create_session'])
  writeDoc(
    repoRoot,
    'README.md',
    ['intro', 'The MCP server exposes 44 tools across tiers.'].join('\n'),
  )

  const { result, err } = captureConsole(() => checkDocsToolCounts(repoRoot, ['README.md']))

  assert.equal(result, false)
  assert.match(err.join('\n'), /README\.md:2: claims 44 tools; registered set has 2/)
})

test('checkDocsToolCounts ignores non-allowlisted documents', () => {
  const repoRoot = makeTempRepo('check-docs-tool-counts')
  writeToolSources(repoRoot, ['list_sessions', 'create_session'])
  writeDoc(repoRoot, 'README.md', 'The MCP server exposes 2 tools across tiers.\n')
  writeDoc(repoRoot, 'docs/plans/old.md', 'The MCP server exposes 44 tools across tiers.\n')

  const { result } = captureConsole(() => checkDocsToolCounts(repoRoot, ['README.md']))

  assert.equal(result, true)
})

test('checkDocsToolCounts fails when an allowlisted document has no count claim', () => {
  const repoRoot = makeTempRepo('check-docs-tool-counts')
  writeToolSources(repoRoot, ['list_sessions'])
  writeDoc(repoRoot, 'README.md', 'The MCP server is available.\n')

  const { result, err } = captureConsole(() => checkDocsToolCounts(repoRoot, ['README.md']))

  assert.equal(result, false)
  assert.match(err.join('\n'), /README\.md: no recognisable full MCP tool-count claim/)
})
