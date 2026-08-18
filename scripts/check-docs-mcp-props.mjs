#!/usr/bin/env node

// Guardrail: every `<CommandTabs mcp="…">` prop on the docs site must name a
// tool the MCP server actually registers. A docs page that offers an MCP tool
// as the equivalent of a `boss` CLI command is making a claim no build, lint,
// or test used to check at all — a renamed or imagined tool name shipped clean.
//
// NECESSARY BUT NOT SUFFICIENT. A green run here proves only that the named
// tool EXISTS. It does not prove the tool does what the CLI example beside it
// does: a real tool with the wrong parameters satisfies this gate while still
// documenting a confidently wrong equivalence (BOS-794 was filed after
// `update_settings` was documented as the MCP equivalent of the
// account-rotation kill switch — the tool exists, but carries no
// managed-accounts field). The author of an `mcp` prop must still read the
// named tool's parameters and confirm they express the CLI example's effect.
// Nothing mechanical can do that half; this gate closes the other half.
//
// The tool names are parsed from the bossmcp registration sites rather than a
// hand-maintained list, so the gate stays truthful to the server. Two
// registration forms exist and both are read: the `Name: "…"` field of an
// `&mcp.Tool{…}` literal, and the tool name passed as a call-site argument to a
// `register…Tool(…)` helper (which is how the session-lifecycle tools land).
// scripts/check-docs-mcp-props.test.mjs pins the parsed set against the
// authoritative list in services/mcp/internal/serve/contract_test.go, so a
// registration written in a third form fails loudly there instead of silently
// shrinking the set here.
//
// Exercised by scripts/check-docs-mcp-props.test.mjs and runnable via
// `node scripts/check-docs-mcp-props.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// Docs tree whose `mcp` props are checked. `DOCS_DIR` is derived from
// `DOCS_SITE_DIR` rather than spelled out again: the two are used as a pair
// below to tell "no docs site in this checkout" from "the docs site is here but
// its tree moved", and two independent literals could drift into a state where
// the site check looks at the old path while the tree check looks at the new
// one — silently restoring the very no-op this gate guards against.
const DOCS_SITE_DIR = path.join('services', 'docs')
const DOCS_DIR = path.join(DOCS_SITE_DIR, 'docs')
const DOC_EXTENSIONS = ['.md', '.mdx']

// The bossmcp registration files. `manifest.go` holds no `Name:` entries — the
// tools are registered from these three siblings, split by mutability class.
const TOOL_SOURCE_FILES = [
  path.join('lib', 'bossalib', 'bossmcp', 'tools.go'),
  path.join('lib', 'bossalib', 'bossmcp', 'tools_mutating.go'),
  path.join('lib', 'bossalib', 'bossmcp', 'tools_destructive.go'),
]

// MCP tool names are snake_case. Used to tell a tool name apart from the other
// string literals sitting beside it (descriptions, display names, ids).
const TOOL_NAME_PATTERN = /^[a-z][a-z0-9_]*$/

// The repo root relative to this file, so the gate works from any cwd — it runs
// from the repo root via `node scripts/check-docs-mcp-props.mjs` and from
// `scripts/` via the scripts Makefile `lint` target.
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// Pull every `mcp="…"` prop out of a Markdown/MDX doc, with its 1-based line
// number. Handles both the multi-line `<CommandTabs\n  mcp="x"\n/>` form used
// across the docs and the single-line form, since the prop is matched on its
// own rather than by parsing the JSX element. Fenced code blocks are skipped:
// a doc that SHOWS a `<CommandTabs>` snippet inside a fence is illustrating
// markup, not claiming an equivalence.
export function extractMcpProps(markdown) {
  const props = []
  const lines = markdown.split('\n')
  let inFence = false

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]

    if (/^\s*```/.test(line)) {
      inFence = !inFence
      continue
    }
    if (inFence) continue

    // The lookbehind keeps a longer attribute ending in `mcp` (or a stray
    // `foo_mcp="…"`) from registering as the prop.
    for (const match of line.matchAll(/(?<![A-Za-z0-9_])mcp="([^"]*)"/g)) {
      props.push({ value: match[1], line: index + 1 })
    }
  }

  return props
}

// Collect the MCP tool names a bossmcp registration file registers.
//
// Form 1 — the tool struct's own field:
//   addTool(server, opts, &mcp.Tool{
//     Name:        "list_sessions",
// The lookbehind excludes `DisplayName:` and friends; the snake_case filter
// excludes `Name: args.Name` style non-literals and prose values.
//
// Form 2 — the name passed to a registration helper:
//   registerSessionStateTool(server, opts, "stop_session", "Stop a running session.", …)
// Matching any `register…Tool(` call rather than the two helper names that
// exist today means a new helper is picked up without editing this gate. The
// first snake_case literal in the argument list is the tool name; the
// description that follows is prose and never matches.
export function parseToolNames(goSource) {
  const names = new Set()

  for (const match of goSource.matchAll(/(?<![A-Za-z0-9_])Name:\s*"([^"]*)"/g)) {
    if (TOOL_NAME_PATTERN.test(match[1])) names.add(match[1])
  }

  for (const match of goSource.matchAll(/(?<![A-Za-z0-9_])register[A-Za-z0-9]*Tool\s*\(([^)]*)/g)) {
    for (const argument of match[1].matchAll(/"([^"]*)"/g)) {
      if (TOOL_NAME_PATTERN.test(argument[1])) {
        names.add(argument[1])
        break
      }
    }
  }

  return names
}

// Every Markdown/MDX doc under `docsDir`, as absolute paths in stable (sorted)
// order. A missing directory yields [] so a partial checkout without the docs
// site is not a failure. (Not the public mirror — mirror-public.yml's
// PRIVATE_PATHS strips top-level `docs`, not `services/docs`, so the mirror
// carries the docs site and this gate runs there in full.)
export function discoverDocFiles(docsDir) {
  if (!fs.existsSync(docsDir)) return []

  const docs = []
  const walk = (dir) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const entryPath = path.join(dir, entry.name)
      if (entry.isDirectory()) {
        walk(entryPath)
      } else if (entry.isFile() && DOC_EXTENSIONS.some((ext) => entry.name.endsWith(ext))) {
        docs.push(entryPath)
      }
    }
  }
  walk(docsDir)

  return docs.sort()
}

export function checkDocsMcpProps(repoRoot = REPO_ROOT) {
  const registered = new Set()
  for (const relativeSource of TOOL_SOURCE_FILES) {
    const sourcePath = path.join(repoRoot, relativeSource)
    if (!fs.existsSync(sourcePath)) {
      // Bail loudly rather than checking props against a half-built tool set,
      // which would report every prop in that class as unregistered.
      console.error(`Missing MCP tool source ${relativeSource}; cannot check docs mcp props.`)
      return false
    }
    for (const name of parseToolNames(fs.readFileSync(sourcePath, 'utf8'))) {
      registered.add(name)
    }
  }

  const docsDir = path.join(repoRoot, DOCS_DIR)
  const docFiles = discoverDocFiles(docsDir)

  // Narrowing tripwire, mirroring the missing-tool-source bail above. Without
  // it this gate has no lower bound on its own input: `DOCS_DIR` is a hardcoded
  // path, and a docs-tree relocation, a Docusaurus layout change, or a typo in
  // that constant leaves `docFiles` empty — every prop then goes unchecked
  // behind a green `Docs MCP props OK (0 props checked …)`, which is exactly
  // what a passing run looks like. An empty success is not a pass.
  //
  // The two cases are told apart by the docs SITE, not the docs tree: no
  // `services/docs` at all is a partial checkout with nothing to check (the
  // public mirror is not this case — it carries `services/docs`), while a site
  // that is present with no Markdown under `DOCS_DIR` means this gate has gone
  // stale and must say so.
  const docsSite = path.join(repoRoot, DOCS_SITE_DIR)
  if (docFiles.length === 0 && fs.existsSync(docsSite)) {
    console.error(`Found ${DOCS_SITE_DIR} but no .md/.mdx under ${DOCS_DIR}.`)
    console.error(
      'The docs tree this gate checks has moved; update DOCS_SITE_DIR/DOCS_DIR in ' +
        'scripts/check-docs-mcp-props.mjs so it stops passing without checking anything.',
    )
    return false
  }

  const misses = []
  let checked = 0
  let extracted = 0
  for (const docFile of docFiles) {
    const relativeDoc = path.relative(repoRoot, docFile)
    for (const { value, line } of extractMcpProps(fs.readFileSync(docFile, 'utf8'))) {
      extracted += 1
      // An empty prop is CommandTabs' explicit "no equivalent on this
      // interface" spelling, not a tool claim, so there is nothing to check.
      if (value === '') continue
      checked += 1
      if (!registered.has(value)) {
        misses.push(`${relativeDoc}:${line}: mcp="${value}" is not a registered MCP tool`)
      }
    }
  }

  // The second half of the narrowing tripwire. The docs-tree check above bounds
  // the FILE count; this bounds the PROP count, because the same empty success
  // reappears one layer up when discovery works and extraction does not — a
  // `CommandTabs` prop rename, an MDX component swap, or a prop spelling this
  // extractor stops matching would leave a full docs tree yielding zero props
  // and print a green `0 props checked`. `extracted` counts the empty `mcp=""`
  // props too — none exist under DOCS_DIR today, but if a future tree spells
  // absence that way those props still prove the extractor is matching, and
  // they must not read as zero coverage.
  if (docFiles.length > 0 && extracted === 0) {
    console.error(`Read ${docFiles.length} doc file(s) under ${DOCS_DIR} but found no mcp prop.`)
    console.error(
      'The prop spelling this gate matches has changed; update extractMcpProps in ' +
        'scripts/check-docs-mcp-props.mjs so it stops passing without checking anything.',
    )
    return false
  }

  if (misses.length > 0) {
    console.error('Docs reference MCP tools that lib/bossalib/bossmcp does not register:')
    for (const miss of misses) {
      console.error(miss)
    }
    console.error('Fix the mcp prop or register the tool so documented equivalences stay real.')
    return false
  }

  console.log(
    `Docs MCP props OK (${checked} props checked against ${registered.size} registered tools)`,
  )
  return true
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) {
  if (!checkDocsMcpProps()) process.exit(1)
}
