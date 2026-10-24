import fs from 'node:fs'
import path from 'node:path'

// The bossmcp registration files. `manifest.go` holds no `Name:` entries; the
// tools are registered from these three siblings, split by mutability class.
export const TOOL_SOURCE_FILES = [
  path.join('lib', 'bossalib', 'bossmcp', 'tools.go'),
  path.join('lib', 'bossalib', 'bossmcp', 'tools_mutating.go'),
  path.join('lib', 'bossalib', 'bossmcp', 'tools_destructive.go'),
]

// The hosted-only tool families. They register only on the hosted Bossanova
// Cloud MCP endpoint (bossmcp Options.IncludeHostedTools), so they sit outside
// TOOL_SOURCE_FILES: the local catalog and every doc that states its size count
// the default surface alone. A caller opts in with { includeHosted: true } when
// a hosted tool is a legitimate answer, as a docs `mcp` prop can be.
export const HOSTED_TOOL_SOURCE_FILES = [
  path.join('lib', 'bossalib', 'bossmcp', 'tools_triggers.go'),
  path.join('lib', 'bossalib', 'bossmcp', 'tools_session_webhooks.go'),
]

// MCP tool names are snake_case. Used to tell a tool name apart from the other
// string literals sitting beside it (descriptions, display names, ids).
const TOOL_NAME_PATTERN = /^[a-z][a-z0-9_]*$/

// Collect the MCP tool names a bossmcp registration file registers.
//
// Form 1: the tool struct's own `Name: "list_sessions"` field.
// Form 2: the name argument passed to a `register...Tool(...)` helper.
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

// Every tool name the default surface registers, plus the hosted-only tier when
// includeHosted is set. A missing source file is pushed to `missing` (hosted
// ones included) so a caller can fail loudly rather than check against a
// silently shrunken set.
export function readRegisteredToolNames(repoRoot, missing = [], { includeHosted = false } = {}) {
  const sources = includeHosted
    ? [...TOOL_SOURCE_FILES, ...HOSTED_TOOL_SOURCE_FILES]
    : TOOL_SOURCE_FILES
  const registered = new Set()
  for (const relativeSource of sources) {
    const sourcePath = path.join(repoRoot, relativeSource)
    if (!fs.existsSync(sourcePath)) {
      missing.push(relativeSource)
      continue
    }
    for (const name of parseToolNames(fs.readFileSync(sourcePath, 'utf8'))) {
      registered.add(name)
    }
  }
  return registered
}
