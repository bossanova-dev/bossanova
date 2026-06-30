#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')

// Host service Go modules live under services/. Out-of-process plugins talk to
// the host over gRPC and must never take a source dependency on these modules
// (the module boundary documented in CLAUDE.md). The `plugins-no-host-imports`
// depguard rule in .golangci.yml enforces this — but only for the module paths
// it explicitly denies. Go's internal/ rule already blocks cross-module
// internal imports; depguard catches the importable (non-internal) host
// packages it does not. If a new service module is added (or an existing one
// renamed) and the deny list is not updated to match, the boundary silently
// stops being enforced for that module. This test pins the invariant so that
// drift fails loudly in CI instead of going unnoticed.

function hostServiceModulePaths() {
  const servicesDir = path.join(repoRoot, 'services')
  const modules = []
  for (const entry of fs.readdirSync(servicesDir, { withFileTypes: true })) {
    if (!entry.isDirectory()) continue
    const goMod = path.join(servicesDir, entry.name, 'go.mod')
    // JS-only services (web, docs, marketing) have no go.mod and host no Go
    // packages a plugin could import, so they are not part of the boundary.
    if (!fs.existsSync(goMod)) continue
    const text = fs.readFileSync(goMod, 'utf8')
    const match = text.match(/^module\s+(\S+)/m)
    assert.ok(match, `services/${entry.name}/go.mod is missing a module declaration`)
    modules.push(match[1])
  }
  return modules
}

// Extract the body of the plugins-no-host-imports depguard rule. The rule key
// is indented under settings.depguard.rules; its body is indented deeper and
// ends at the next line indented at or above the rule key's own depth (a
// sibling rule or a parent key).
function pluginsRuleBody(config) {
  const lines = config.split('\n')
  const startIndex = lines.findIndex((line) => line.includes('plugins-no-host-imports:'))
  assert.notEqual(
    startIndex,
    -1,
    '.golangci.yml is missing the plugins-no-host-imports depguard rule',
  )
  const ruleIndent = lines[startIndex].length - lines[startIndex].trimStart().length
  const body = [lines[startIndex]]
  for (let i = startIndex + 1; i < lines.length; i++) {
    const line = lines[i]
    if (line.trim() === '') {
      body.push(line)
      continue
    }
    const indent = line.length - line.trimStart().length
    if (indent <= ruleIndent) break
    body.push(line)
  }
  return body.join('\n')
}

test('depguard denies plugin imports of every host service module', () => {
  const config = fs.readFileSync(path.join(repoRoot, '.golangci.yml'), 'utf8')
  const ruleBody = pluginsRuleBody(config)
  const deniedPkgs = [...ruleBody.matchAll(/-\s*pkg:\s*(\S+)/g)].map((m) => m[1].replace(/\/$/, ''))

  const modules = hostServiceModulePaths()
  assert.ok(
    modules.length >= 3,
    `expected at least the boss, bossd, and bosso host modules, found ${modules.length}`,
  )

  for (const mod of modules) {
    assert.ok(
      deniedPkgs.includes(mod),
      `.golangci.yml plugins-no-host-imports must deny plugin imports of host ` +
        `module "${mod}" (add "- pkg: ${mod}/" to its deny list)`,
    )
  }
})
