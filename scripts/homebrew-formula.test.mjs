#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const formulaPath = path.join(repoRoot, 'infra/homebrew/bossanova.rb')
const formula = fs.readFileSync(formulaPath, 'utf8')

// Extracts the body of `def caveats ... end` so caveats-only assertions can't
// be satisfied (or silently broken) by unrelated text elsewhere in the
// formula. Returns '' if no caveats method is found.
function extractCaveats(source) {
  const match = source.match(/def caveats\b([\s\S]*?)\n {2}end\b/)
  return match ? match[1] : ''
}

test('BOS-627: formula defines no `service do` block (no autostart service)', () => {
  // The formula legitimately contains the bare word "service" (e.g. `service:`
  // keys and prose about the bossd service), so this must anchor on a
  // `service do` token at a statement position, not just the substring
  // "service". BOS-627's premise is that Homebrew never installs/enables a
  // background service for boss-mcp; a `service do` block appearing here would
  // silently reintroduce the autostart behavior the ticket says doesn't exist.
  assert.doesNotMatch(
    formula,
    /^\s*service\s+do\b/m,
    'BOS-627: found a `service do` block in the Homebrew formula. The formula ' +
      'must not define a launchd/systemd service — the MCP server is opt-in ' +
      'via `boss mcp install`, never auto-started by `brew install`.',
  )
})

test('BOS-627: formula defines no `plist` method (no legacy autostart)', () => {
  assert.doesNotMatch(
    formula,
    /def plist\b/,
    'BOS-627: found a `def plist` method in the Homebrew formula. A `plist` ' +
      'method is the legacy (pre-`service do`) way to make brew autostart a ' +
      'background service, which would reintroduce the false "Homebrew ' +
      'enables the MCP service" impression BOS-627 fixed.',
  )
})

test('BOS-627: caveats text contains no `boss mcp install` command to copy', () => {
  const caveats = extractCaveats(formula)

  assert.ok(
    caveats.trim().length > 0,
    'BOS-627: could not extract a non-empty `def caveats ... end` body from ' +
      'the formula — the extraction regex may need updating. Without this, ' +
      'the "no boss mcp install" assertion below would vacuously pass.',
  )

  assert.doesNotMatch(
    caveats,
    /boss mcp install/,
    'BOS-627: caveats still contains a copy-pasteable `boss mcp install` ' +
      'command. Users read that as "Homebrew requires this setup step", which ' +
      'is exactly the false impression BOS-627 fixes — the standalone MCP ' +
      'server should only be pointed at via the docs URL, not an imperative ' +
      'command in caveats.',
  )
})

test('BOS-696: caveats explain why `brew upgrade` requires a daemon restart', () => {
  const caveats = extractCaveats(formula)

  assert.match(
    caveats,
    /boss daemon restart/,
    'BOS-696: caveats lost the `boss daemon restart` command users need after ' +
      'a Homebrew upgrade.',
  )

  assert.match(
    caveats,
    /re-stages bossd at a\s+version-stable real path/,
    'BOS-696: caveats must explain that restarting re-stages bossd at a ' +
      'version-stable real path, which keeps macOS privacy permissions matched.',
  )
})
