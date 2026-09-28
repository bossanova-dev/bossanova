#!/usr/bin/env node

// Guards the preconditions of the PostHog anonymous-to-identified merge on the
// marketing -> signup path (BOS-1314, docs/analytics/events.md "Verifying the
// merge").
//
// The merge holds by construction: posthog-js keeps the anonymous id in a
// cross-subdomain cookie by default, so the id the marketing site assigned is
// the one the web app later merges into user:<WorkOS ID> on identify(). Two
// kinds of change break it silently, with no error anywhere:
//
//   1. Either PostHog init source sets one of the persistence options below,
//      which moves or disables the shared anonymous id.
//   2. A release workflow gives the marketing and web builds different PostHog
//      project tokens or hosts, which puts the two halves in different projects.
//
// The workflow rule is per file: production and staging deliberately use
// different projects, and that is fine because each deploys its own pair.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(scriptDirectory, '..')

export const MARKETING_ANALYTICS_PATH = 'services/marketing/src/components/Analytics.astro'
export const WEB_PROVIDER_PATH = 'services/web/src/analytics/PostHogProvider.tsx'
export const WORKFLOWS_DIRECTORY = '.github/workflows'

// PostHog init options that change where, or whether, the anonymous id is kept.
export const FORBIDDEN_INIT_OPTIONS = [
  'cross_subdomain_cookie',
  'persistence',
  'persistence_name',
  'disable_persistence',
]

// Each group names the marketing (Astro) and web (Vite) variables that must
// carry one value within a single workflow file.
export const WORKFLOW_VARIABLE_GROUPS = [
  {
    label: 'project token',
    names: ['PUBLIC_POSTHOG_PROJECT_TOKEN', 'VITE_PUBLIC_POSTHOG_PROJECT_TOKEN'],
  },
  { label: 'host', names: ['PUBLIC_POSTHOG_HOST', 'VITE_PUBLIC_POSTHOG_HOST'] },
]

// An object-literal key, bare or quoted, followed by a colon. The leading
// boundary keeps `persistence` from matching inside `disable_persistence`, and
// the colon keeps it from matching inside `persistence_name`.
const initOptionPattern = new RegExp(
  `(?:^|[^A-Za-z0-9_$])['"]?(${FORBIDDEN_INIT_OPTIONS.join('|')})['"]?\\s*:`,
  'gm',
)

export function forbiddenInitOptions(source) {
  const found = new Set()
  for (const match of source.matchAll(initOptionPattern)) {
    found.add(match[1])
  }
  return [...found].sort()
}

function normalizeWorkflowValue(raw) {
  let value = raw.trim()
  if (!value.startsWith('"') && !value.startsWith("'")) {
    value = value.replace(/\s+#.*$/, '')
  }
  return value.replace(/^(['"])(.*)\1$/, '$2').trim()
}

// posthogWorkflowAssignments maps each PostHog variable name to the distinct
// values a workflow file assigns it, as `NAME: value` YAML entries.
export function posthogWorkflowAssignments(contents) {
  const names = WORKFLOW_VARIABLE_GROUPS.flatMap((group) => group.names)
  const assignments = new Map()
  for (const line of contents.split('\n')) {
    const match = /^\s*-?\s*([A-Z_]+)\s*:\s*(.+)$/.exec(line)
    if (!match || !names.includes(match[1])) {
      continue
    }
    const value = normalizeWorkflowValue(match[2])
    if (value === '') {
      continue
    }
    if (!assignments.has(match[1])) {
      assignments.set(match[1], new Set())
    }
    assignments.get(match[1]).add(value)
  }
  return assignments
}

// identityMergeViolations returns one message per broken precondition. Inputs
// are file contents: marketingAnalytics and webProvider are the two PostHog
// init sources, and workflows maps a workflow path to its contents.
export function identityMergeViolations({ marketingAnalytics, webProvider, workflows }) {
  const violations = []
  for (const [label, source] of [
    [MARKETING_ANALYTICS_PATH, marketingAnalytics],
    [WEB_PROVIDER_PATH, webProvider],
  ]) {
    for (const option of forbiddenInitOptions(source ?? '')) {
      violations.push(
        `${label}: PostHog init sets ${option}, which moves or drops the anonymous id the marketing-to-signup merge relies on`,
      )
    }
  }
  for (const [workflowPath, contents] of Object.entries(workflows ?? {})) {
    const assignments = posthogWorkflowAssignments(contents)
    for (const group of WORKFLOW_VARIABLE_GROUPS) {
      const values = new Set()
      for (const name of group.names) {
        for (const value of assignments.get(name) ?? []) {
          values.add(value)
        }
      }
      if (values.size > 1) {
        violations.push(
          `${workflowPath}: ${group.names.join(' and ')} carry ${values.size} distinct PostHog ${group.label} values; the marketing and web builds must share one`,
        )
      }
    }
  }
  return violations
}

// loadRepoSources reads the real init sources and every workflow file.
export function loadRepoSources(root = repoRoot) {
  const read = (relative) => fs.readFileSync(path.join(root, relative), 'utf8')
  const workflows = {}
  for (const entry of fs.readdirSync(path.join(root, WORKFLOWS_DIRECTORY)).sort()) {
    if (entry.endsWith('.yml') || entry.endsWith('.yaml')) {
      const relative = path.posix.join(WORKFLOWS_DIRECTORY, entry)
      workflows[relative] = read(relative)
    }
  }
  return {
    marketingAnalytics: read(MARKETING_ANALYTICS_PATH),
    webProvider: read(WEB_PROVIDER_PATH),
    workflows,
  }
}

function main() {
  const violations = identityMergeViolations(loadRepoSources())
  if (violations.length === 0) {
    console.log('check-posthog-identity-merge: merge preconditions hold')
    return 0
  }
  for (const violation of violations) {
    console.error(`check-posthog-identity-merge: ${violation}`)
  }
  return 1
}

if (isMainModule(import.meta.url)) {
  process.exitCode = main()
}
