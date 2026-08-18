#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  checkDocsMcpProps,
  discoverDocFiles,
  extractMcpProps,
  parseToolNames,
} from './check-docs-mcp-props.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// Capture what the gate prints so a test can assert on the failure text a
// contributor actually sees, not just the boolean.
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

// Write the three bossmcp registration files a temp repo needs, registering
// exactly `names` through the plain struct-literal form.
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
  const docPath = path.join(repoRoot, 'services', 'docs', 'docs', relativePath)
  fs.mkdirSync(path.dirname(docPath), { recursive: true })
  fs.writeFileSync(docPath, contents)
}

test('extractMcpProps reads the multi-line CommandTabs form', () => {
  const doc = ['<CommandTabs', '  cli="boss notes ls"', '  mcp="list_notes"', '/>'].join('\n')

  assert.deepEqual(extractMcpProps(doc), [{ value: 'list_notes', line: 3 }])
})

test('extractMcpProps reads the single-line CommandTabs form', () => {
  const doc = ['# Notes', '', '<CommandTabs cli="boss notes get" mcp="get_note" />'].join('\n')

  assert.deepEqual(extractMcpProps(doc), [{ value: 'get_note', line: 3 }])
})

test('extractMcpProps ignores mcp props inside a fenced code block', () => {
  const doc = [
    '<CommandTabs mcp="list_notes" />',
    '',
    '```mdx',
    '<CommandTabs mcp="illustrative_only" />',
    '```',
    '',
    '<CommandTabs mcp="get_note" />',
  ].join('\n')

  assert.deepEqual(extractMcpProps(doc), [
    { value: 'list_notes', line: 1 },
    { value: 'get_note', line: 7 },
  ])
})

test('extractMcpProps records every prop on a line with its line number', () => {
  const doc = ['intro', '<CommandTabs mcp="list_notes" /> <CommandTabs mcp="get_note" />'].join(
    '\n',
  )

  assert.deepEqual(extractMcpProps(doc), [
    { value: 'list_notes', line: 2 },
    { value: 'get_note', line: 2 },
  ])
})

test('extractMcpProps does not treat a longer attribute ending in mcp as the prop', () => {
  const doc = '<Thing data_mcp="not_a_prop" mcp="list_notes" />'

  assert.deepEqual(extractMcpProps(doc), [{ value: 'list_notes', line: 1 }])
})

test('parseToolNames reads the struct-literal registration form', () => {
  const source = [
    'addTool(server, opts, &mcp.Tool{',
    '\tName:        "list_sessions",',
    '\tDescription: "List sessions.",',
    '})',
    'addTool(server, opts, &mcp.Tool{',
    '\tName: "list_broadcasts",',
    '})',
  ].join('\n')

  assert.deepEqual([...parseToolNames(source)].sort(), ['list_broadcasts', 'list_sessions'])
})

test('parseToolNames ignores DisplayName and non-literal Name fields', () => {
  const source = [
    'return &pb.Repo{Id: r.GetId(), DisplayName: r.GetDisplayName()}',
    'req := &pb.CreateCronJobRequest{',
    '\tName:        args.Name,',
    '\tDisplayName: "Human Readable Thing",',
    '}',
    'addTool(server, opts, &mcp.Tool{',
    '\tName:        "create_cron_job",',
    '})',
  ].join('\n')

  assert.deepEqual([...parseToolNames(source)], ['create_cron_job'])
})

test('parseToolNames reads the name argument of a register…Tool helper call', () => {
  const source = [
    'registerSessionStateTool(server, opts, "stop_session", "Stop a running session.", false, backend.StopSession)',
    'registerDestructiveSessionTool(server, opts, "close_session", "Close a session (abandons its work). Destructive — requires confirm:true.", backend.CloseSession)',
    'func registerSessionStateTool(server *mcp.Server, opts Options, name, desc string) {',
  ].join('\n')

  assert.deepEqual([...parseToolNames(source)].sort(), ['close_session', 'stop_session'])
})

// The gate parses Go registration sites, which is brittle by construction: a
// registration written in a form the patterns miss would shrink the tool set
// and turn a correct prop into a false miss. services/mcp's contract test holds
// the authoritative name list, so pinning the parsed set against it makes that
// drift fail here, loudly, instead of in the docs gate.
test('parseToolNames over the real bossmcp sources equals the contract test tool list', () => {
  const sources = [
    'lib/bossalib/bossmcp/tools.go',
    'lib/bossalib/bossmcp/tools_mutating.go',
    'lib/bossalib/bossmcp/tools_destructive.go',
  ]

  const parsed = new Set()
  for (const source of sources) {
    for (const name of parseToolNames(fs.readFileSync(path.join(REPO_ROOT, source), 'utf8'))) {
      parsed.add(name)
    }
  }

  const contractSource = fs.readFileSync(
    path.join(REPO_ROOT, 'services', 'mcp', 'internal', 'serve', 'contract_test.go'),
    'utf8',
  )
  const block = /var expectedTools = \[\]string\{([\s\S]*?)\n\}/.exec(contractSource)
  assert.ok(block, 'expectedTools declaration not found in contract_test.go')
  const expected = [...block[1].matchAll(/"([^"]+)"/g)].map((match) => match[1])

  assert.equal(expected.length, 69, 'contract_test.go should list 69 tools')
  assert.deepEqual([...parsed].sort(), [...expected].sort())
})

test('checkDocsMcpProps passes when every prop names a registered tool', () => {
  const repoRoot = makeTempRepo('check-docs-mcp-props')
  writeToolSources(repoRoot, ['list_notes', 'get_note'])
  writeDoc(repoRoot, 'guides/notes.md', '<CommandTabs\n  mcp="list_notes"\n/>\n')
  writeDoc(repoRoot, 'guides/more.mdx', '<CommandTabs mcp="get_note" />\n')

  const { result, out } = captureConsole(() => checkDocsMcpProps(repoRoot))

  assert.equal(result, true)
  assert.match(out.join('\n'), /Docs MCP props OK \(2 props checked against 2 registered tools\)/)
})

test('checkDocsMcpProps names the file, line, and value of an unregistered tool', () => {
  const repoRoot = makeTempRepo('check-docs-mcp-props')
  writeToolSources(repoRoot, ['list_notes'])
  writeDoc(
    repoRoot,
    'guides/notes.md',
    ['# Notes', '', '<CommandTabs', '  cli="boss notes ls"', '  mcp="not_a_tool"', '/>', ''].join(
      '\n',
    ),
  )

  const { result, err } = captureConsole(() => checkDocsMcpProps(repoRoot))

  assert.equal(result, false)
  assert.ok(
    err.includes(
      'services/docs/docs/guides/notes.md:5: mcp="not_a_tool" is not a registered MCP tool',
    ),
    `expected a file:line:value miss, got:\n${err.join('\n')}`,
  )
})

test('checkDocsMcpProps ignores an empty mcp prop', () => {
  const repoRoot = makeTempRepo('check-docs-mcp-props')
  writeToolSources(repoRoot, ['list_notes'])
  writeDoc(repoRoot, 'guides/notes.md', '<CommandTabs cli="boss doctor" mcp="" />\n')

  const { result, out } = captureConsole(() => checkDocsMcpProps(repoRoot))

  assert.equal(result, true)
  assert.match(out.join('\n'), /0 props checked/)
})

test('checkDocsMcpProps fails loudly when a tool source file is missing', () => {
  const repoRoot = makeTempRepo('check-docs-mcp-props')
  writeToolSources(repoRoot, ['list_notes'])
  fs.rmSync(path.join(repoRoot, 'lib', 'bossalib', 'bossmcp', 'tools_mutating.go'))
  writeDoc(repoRoot, 'guides/notes.md', '<CommandTabs mcp="list_notes" />\n')

  const { result, err } = captureConsole(() => checkDocsMcpProps(repoRoot))

  assert.equal(result, false)
  assert.match(err.join('\n'), /Missing MCP tool source lib\/bossalib\/bossmcp\/tools_mutating\.go/)
})

test('checkDocsMcpProps passes the real repository over a non-empty prop set', () => {
  const { result, out } = captureConsole(() => checkDocsMcpProps(REPO_ROOT))

  assert.equal(result, true)
  // Coverage floor, not decoration. `true` is also what a zero-coverage run
  // returns, so asserting the boolean alone cannot tell today's 57-prop run
  // from a gate that checked nothing at all — which is precisely what a moved
  // docs tree would produce. Pin both counts above zero so that reds here.
  const counts = /Docs MCP props OK \((\d+) props checked against (\d+) registered tools\)/.exec(
    out.join('\n'),
  )
  assert.ok(counts, `expected the OK line with counts, got:\n${out.join('\n')}`)
  assert.ok(Number(counts[1]) > 0, `expected a non-zero prop count, got ${counts[1]}`)
  assert.ok(Number(counts[2]) > 0, `expected a non-zero tool count, got ${counts[2]}`)
})

test('checkDocsMcpProps fails when the docs site is present but the docs tree has moved', () => {
  const repoRoot = makeTempRepo('check-docs-mcp-props')
  writeToolSources(repoRoot, ['list_notes'])
  // The site is here, but the tree DOCS_DIR names is not — a relocation or a
  // stale constant. Without the tripwire this returned true after checking
  // nothing.
  fs.mkdirSync(path.join(repoRoot, 'services', 'docs', 'content'), { recursive: true })

  const { result, err } = captureConsole(() => checkDocsMcpProps(repoRoot))

  assert.equal(result, false)
  assert.match(err.join('\n'), /no \.md\/\.mdx under services\/docs\/docs/)
})

test('checkDocsMcpProps fails when the docs tree yields no mcp prop at all', () => {
  const repoRoot = makeTempRepo('check-docs-mcp-props')
  writeToolSources(repoRoot, ['list_notes'])
  // Discovery works, extraction does not — a prop rename, an MDX component
  // swap, or a spelling extractMcpProps stopped matching. Without the second
  // tripwire this printed "0 props checked" and returned true.
  writeDoc(repoRoot, 'guides/notes.md', '# Notes\n\nNo command tabs here.\n')

  const { result, err } = captureConsole(() => checkDocsMcpProps(repoRoot))

  assert.equal(result, false)
  assert.match(err.join('\n'), /found no mcp prop/)
})

test('checkDocsMcpProps still passes a checkout with no docs site at all', () => {
  const repoRoot = makeTempRepo('check-docs-mcp-props')
  writeToolSources(repoRoot, ['list_notes'])

  const { result } = captureConsole(() => checkDocsMcpProps(repoRoot))

  assert.equal(result, true)
})

test('discoverDocFiles walks nested docs and returns [] for a missing tree', () => {
  const repoRoot = makeTempRepo('check-docs-mcp-props')
  writeDoc(repoRoot, 'guides/notes.md', 'a\n')
  writeDoc(repoRoot, 'reference/cli.mdx', 'b\n')
  writeDoc(repoRoot, 'guides/image.png', 'c\n')

  const docsDir = path.join(repoRoot, 'services', 'docs', 'docs')
  assert.deepEqual(
    discoverDocFiles(docsDir).map((docPath) => path.relative(docsDir, docPath)),
    [path.join('guides', 'notes.md'), path.join('reference', 'cli.mdx')],
  )
  assert.deepEqual(discoverDocFiles(path.join(repoRoot, 'nope')), [])
})
