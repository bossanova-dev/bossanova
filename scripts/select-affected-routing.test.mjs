#!/usr/bin/env node

// Drift gate for workflow path filters versus the local affected-test selector.
//
// CI workflows and scripts/select-affected-tests.mjs are separate routing tables.
// Keep every intentional divergence in workflowRouteExemptions with a reason, and
// fail when a workflow adds a path class that neither routes locally nor is exempt.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { selectTargets } from './select-affected-tests.mjs'

const repoRoot = path.dirname(fileURLToPath(new URL('../Makefile', import.meta.url)))

export const workflowRouteRules = [
  { workflow: 'test-bosso-production-deployment.yml', requiredTarget: null },
  { workflow: 'test-docs.yml', requiredTarget: null },
  { workflow: 'test-marketing.yml', requiredTarget: 'test-web' },
  { workflow: 'test-plugin-distribution.yml', requiredTarget: null },
  { workflow: 'test-proto.yml', requiredTarget: null },
  { workflow: 'test-scripts.yml', requiredTarget: 'test-scripts' },
  { workflow: 'test-web.yml', requiredTarget: 'test-web' },
]

export const workflowRouteExemptions = [
  {
    workflow: 'test-bosso-production-deployment.yml',
    pattern: 'services/bosso/**',
    reason: 'deployment smoke has no worktree-local make target',
  },
  {
    workflow: 'test-docs.yml',
    pattern: 'services/docs/**',
    reason: 'docs workflow has no root make target; services/docs owns its suite-local make test',
  },
  {
    workflow: 'test-plugin-distribution.yml',
    pattern: 'plugins/**',
    reason: 'plugin distribution guard has no local make target; plugin code routes by module',
  },
  {
    workflow: 'test-proto.yml',
    pattern: 'proto/**',
    reason: 'proto workflow fans out to protoTargets plus web, not one required target',
  },
  {
    workflow: 'test-proto.yml',
    pattern: 'lib/bossalib/gen/**',
    reason: 'generated Go files route by owning module instead of the proto workflow fan-out',
  },
  {
    workflow: 'test-proto.yml',
    pattern: 'lib/bossalib/go.mod',
    reason: 'module graph file routes through graph-wide/module checks instead of one proto target',
  },
  {
    workflow: 'test-proto.yml',
    pattern: 'services/docs/openapi/**',
    reason: 'OpenAPI specs feed the bossalib apiversion ledger row, not a single proto target',
  },
  {
    workflow: 'test-proto.yml',
    pattern: 'services/web/src/gen/**',
    reason: 'web generated-code consumers route to test-web, not a single proto target',
  },
  {
    workflow: 'test-proto.yml',
    pattern: 'services/web/package.json',
    reason: 'web codegen package inputs route to test-web, not a single proto target',
  },
  {
    workflow: 'test-proto.yml',
    pattern: 'lib/bossalib/go.sum',
    reason: 'module graph file routes through graph-wide/module checks instead of one proto target',
  },
  {
    workflow: 'test-scripts.yml',
    pattern: 'Makefile',
    reason: 'Makefile is a graph-wide Bazel trigger; CI scripts coverage is a superset',
  },
  {
    workflow: 'test-scripts.yml',
    pattern: 'CLAUDE.md',
    reason: 'agent instructions route to test-manifest locally; CI scripts coverage is a superset',
  },
  {
    workflow: 'test-scripts.yml',
    pattern: 'AGENTS.md',
    reason: 'agent instructions route to test-manifest locally; CI scripts coverage is a superset',
  },
  {
    workflow: 'test-scripts.yml',
    pattern: 'docs/skills/**',
    reason:
      'docs skills route to boss skill parity locally; CI scripts also runs dispatch batching table gates',
  },
  {
    workflow: 'test-scripts.yml',
    pattern: '**/go.mod',
    reason: 'CI is deliberately broad; local mirroring would run scripts on every module edit',
  },
  {
    workflow: 'test-scripts.yml',
    pattern: '**/*_test.go',
    reason: 'CI is deliberately broad; local mirroring would run scripts on every Go test edit',
  },
]

test('workflow route rules cover every path-filtered workflow', () => {
  assert.deepEqual(pathFilteredWorkflows(), workflowRouteRules.map((rule) => rule.workflow).sort())
})

test('test-scripts push paths and paths-filter scripts list stay byte-identical', () => {
  const workflow = readWorkflow('test-scripts.yml')
  assert.deepEqual(
    extractPushPaths(workflow),
    extractPathsFilterList(workflow, 'scripts'),
    'test-scripts.yml duplicates its route list; edit both copies together',
  )
})

test('workflow path filters either route to the local target or carry a reasoned exemption', () => {
  assert.deepEqual(validateWorkflowRoutes(), [])
})

test('every path-filtered workflow self-edit runs the routing gate', () => {
  for (const { workflow } of workflowRouteRules) {
    const selected = selectTargets([`.github/workflows/${workflow}`]).map(({ target }) => target)
    assert.ok(selected.includes('test-scripts'), `${workflow} must select test-scripts`)
  }
})

test('push path extraction ignores pull_request paths that appear first', () => {
  const workflow = [
    'name: synthetic',
    '',
    'on:',
    '  pull_request:',
    '    paths:',
    '      - pull-request-only/**',
    '  push:',
    '    branches-ignore:',
    "      - 'main'",
    '    paths:',
    '      - push-only/**',
    '',
  ].join('\n')

  assert.deepEqual(extractPushPaths(workflow), ['push-only/**'])
})

test('workflow route validation fails when a new pattern has no route or exemption', () => {
  const failures = validateWorkflowRoutes({
    workflows: new Map([['synthetic.yml', ['scripts/**', 'unrouted/new-input/**']]]),
    rules: [{ workflow: 'synthetic.yml', requiredTarget: 'test-scripts' }],
    exemptions: [],
  })

  assert.deepEqual(failures, [
    'synthetic.yml unrouted/new-input/** -> test-smoke, expected test-scripts or an exemption',
  ])
})

test('workflow route validation fails when an exemption points at a removed pattern', () => {
  const failures = validateWorkflowRoutes({
    workflows: new Map([['synthetic.yml', ['scripts/**']]]),
    rules: [{ workflow: 'synthetic.yml', requiredTarget: 'test-scripts' }],
    exemptions: [
      {
        workflow: 'synthetic.yml',
        pattern: 'gone/**',
        reason: 'proves removed-pattern exemptions fail',
      },
    ],
  })

  assert.deepEqual(failures, ['synthetic.yml gone/** exemption is stale because pattern is absent'])
})

test('workflow route validation fails when an exemption now routes correctly', () => {
  const failures = validateWorkflowRoutes({
    workflows: new Map([['synthetic.yml', ['scripts/**']]]),
    rules: [{ workflow: 'synthetic.yml', requiredTarget: 'test-scripts' }],
    exemptions: [
      {
        workflow: 'synthetic.yml',
        pattern: 'scripts/**',
        reason: 'proves routing exemptions fail once the selector covers them',
      },
    ],
  })

  assert.deepEqual(failures, [
    'synthetic.yml scripts/** exemption is stale because it now routes to test-scripts',
  ])
})

test('workflow route validation fails when a null-target exemption now routes to test-scripts', () => {
  const failures = validateWorkflowRoutes({
    workflows: new Map([['synthetic.yml', ['scripts/**']]]),
    rules: [{ workflow: 'synthetic.yml', requiredTarget: null }],
    exemptions: [
      {
        workflow: 'synthetic.yml',
        pattern: 'scripts/**',
        reason: 'proves null-target routing exemptions fail once covered by the scripts gate',
      },
    ],
  })

  assert.deepEqual(failures, [
    'synthetic.yml scripts/** exemption is stale because it now routes to test-scripts',
  ])
})

export function validateWorkflowRoutes({
  workflows = realWorkflowPaths(),
  rules = workflowRouteRules,
  exemptions = workflowRouteExemptions,
} = {}) {
  const failures = []
  const exemptionByKey = new Map()

  for (const exemption of exemptions) {
    assert.equal(typeof exemption.reason, 'string', 'workflow route exemptions need a reason')
    assert.notEqual(
      exemption.reason.trim(),
      '',
      'workflow route exemptions need a non-empty reason',
    )
    exemptionByKey.set(exemptionKey(exemption.workflow, exemption.pattern), exemption)
  }

  for (const rule of rules) {
    const patterns = workflows.get(rule.workflow)
    assert.ok(patterns, `${rule.workflow} must be present in the workflow path set`)
    for (const pattern of patterns) {
      const key = exemptionKey(rule.workflow, pattern)
      if (exemptionByKey.has(key)) continue
      const requiredTarget = requiredTargetForPattern(rule, pattern)
      if (requiredTarget === null) {
        continue
      }
      const selected = targetsForPattern(pattern)
      if (!selected.includes(requiredTarget)) {
        failures.push(
          `${rule.workflow} ${pattern} -> ${selected.join(',')}, expected ${requiredTarget} or an exemption`,
        )
      }
    }
  }

  for (const exemption of exemptions) {
    const patterns = workflows.get(exemption.workflow)
    if (!patterns?.includes(exemption.pattern)) {
      failures.push(
        `${exemption.workflow} ${exemption.pattern} exemption is stale because pattern is absent`,
      )
      continue
    }
    const rule = rules.find((candidate) => candidate.workflow === exemption.workflow)
    if (!rule) continue
    const selected = targetsForPattern(exemption.pattern)
    const requiredTarget = requiredTargetForPattern(rule, exemption.pattern)
    const staleTarget =
      requiredTarget !== null
        ? requiredTarget
        : selected.includes('test-scripts')
          ? 'test-scripts'
          : null
    if (staleTarget !== null && selected.includes(staleTarget)) {
      failures.push(
        `${exemption.workflow} ${exemption.pattern} exemption is stale because it now routes to ${staleTarget}`,
      )
    }
  }

  return failures.sort()
}

function requiredTargetForPattern(rule, pattern) {
  if (pattern === `.github/workflows/${rule.workflow}`) return 'test-scripts'
  return rule.requiredTarget
}

function realWorkflowPaths() {
  return new Map(
    workflowRouteRules.map(({ workflow }) => [workflow, extractPushPaths(readWorkflow(workflow))]),
  )
}

function pathFilteredWorkflows() {
  const workflowsDir = path.join(repoRoot, '.github', 'workflows')
  return fs
    .readdirSync(workflowsDir)
    .filter((name) => name.endsWith('.yml') || name.endsWith('.yaml'))
    .filter((name) => extractPushPaths(readWorkflow(name), { required: false }).length > 0)
    .sort()
}

function readWorkflow(name) {
  return fs.readFileSync(path.join(repoRoot, '.github', 'workflows', name), 'utf8')
}

export function extractPushPaths(text, { required = true } = {}) {
  const lines = text.split('\n')
  const pushLine = lines.findIndex((line) => /^\s{2}push:\s*$/.test(line))
  if (pushLine === -1 && !required) return []
  assert.notEqual(pushLine, -1, 'workflow push block not found')
  const pushIndent = indentOf(lines[pushLine])
  let pathsLine = -1
  for (let i = pushLine + 1; i < lines.length; i++) {
    const line = lines[i]
    if (line.trim() === '' || line.trimStart().startsWith('#')) continue
    if (indentOf(line) <= pushIndent) break
    if (/^\s{4}paths:\s*$/.test(line)) {
      pathsLine = i
      break
    }
  }
  if (pathsLine === -1 && !required) return []
  assert.notEqual(pathsLine, -1, 'workflow push.paths block not found')
  return extractDashList(lines, pathsLine)
}

function extractPathsFilterList(text, filterName) {
  const lines = text.split('\n')
  const filterLine = lines.findIndex((line) => new RegExp(`^\\s{12}${filterName}:\\s*$`).test(line))
  assert.notEqual(filterLine, -1, `${filterName} paths-filter block not found`)
  return extractDashList(lines, filterLine)
}

function extractDashList(lines, headingLine) {
  const headingIndent = indentOf(lines[headingLine])
  const items = []
  for (let i = headingLine + 1; i < lines.length; i++) {
    const line = lines[i]
    if (line.trim() === '' || line.trimStart().startsWith('#')) continue
    if (indentOf(line) <= headingIndent) break
    const item = line.match(/^\s*-\s+(.+?)\s*$/)
    if (item) items.push(unquoteYamlScalar(item[1]))
  }
  return items
}

function targetsForPattern(pattern) {
  return selectTargets([probePathForPattern(pattern)]).map(({ target }) => target)
}

function probePathForPattern(pattern) {
  if (pattern === '**/go.mod') return 'services/bosso/go.mod'
  if (pattern === '**/*_test.go') return 'services/bosso/internal/server/__probe___test.go'
  if (pattern === '.claude/skills/**') return '.claude/skills/__probe__/SKILL.md'
  if (pattern === '.codex/skills/**') return '.codex/skills/__probe__/SKILL.md'
  if (pattern === 'plugins/**') return 'plugins/bossd-plugin-claude/main.go'
  if (pattern === 'services/boss/internal/skillinstall/skills/**') {
    return 'services/boss/internal/skillinstall/skills/boss-build/SKILL.md'
  }
  if (pattern.endsWith('/**')) return `${pattern.slice(0, -3)}/__probe__`
  if (pattern.includes('*'))
    return pattern.replaceAll('**', '__probe__').replaceAll('*', '__probe__')
  return pattern
}

function exemptionKey(workflow, pattern) {
  return `${workflow}\0${pattern}`
}

function indentOf(line) {
  return line.match(/^\s*/)[0].length
}

function unquoteYamlScalar(value) {
  const trimmed = value.trim()
  if (
    (trimmed.startsWith('"') && trimmed.endsWith('"')) ||
    (trimmed.startsWith("'") && trimmed.endsWith("'"))
  ) {
    return trimmed.slice(1, -1)
  }
  return trimmed
}
