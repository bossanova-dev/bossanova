#!/usr/bin/env node

import fs from 'node:fs'

const mirrorWorkflow = '.github/workflows/mirror-public.yml'

if (!fs.existsSync(mirrorWorkflow)) {
  console.log('Public mirror workflow check skipped; mirror workflow not present.')
  process.exit(0)
}

const mirror = fs.readFileSync(mirrorWorkflow, 'utf8')

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function includesFilenameToken(content, filename) {
  const pattern = new RegExp(`(^|[^A-Za-z0-9_.-])${escapeRegExp(filename)}($|[^A-Za-z0-9_.-])`)
  return pattern.test(content)
}

// BOS-343 consolidated the 13 per-module Go workflows into a private-only
// bazel.yml. The public mirror does not get Bazel (its go.work omits private
// modules), so the restored public workflow set is the native Go CI (test-go.yml)
// plus the shared always-public workflows.
const requiredPublicWorkflows = [
  '.github/workflows/ci.yml',
  '.github/workflows/test-proto.yml',
  '.github/workflows/test-scripts.yml',
  '.github/workflows/test-go.yml',
]

const missing = requiredPublicWorkflows.filter((workflow) => !mirror.includes(workflow))

if (missing.length > 0) {
  console.error('Public mirror is missing test workflows for public repo code:')
  for (const workflow of missing) {
    console.error(`  - ${workflow}`)
  }
  process.exit(1)
}

// Guard the mirror's own safety mechanisms against regression: the denylist
// must exclude the files that previously leaked, the curated public env
// example must be restored, the leak guard must run, and the push must use a
// lease. The leak-guard *logic* is unit-tested in check-mirror-leaks.test.mjs;
// these checks ensure the workflow actually wires it up.
const requiredMirrorClauses = [
  { clause: 'AGENTS.md' },
  { clause: 'CLAUDE.md' },
  { clause: '.env.example', match: includesFilenameToken },
  { clause: 'bossd-plugin-repair' },
  { clause: '.env.example.public', match: includesFilenameToken },
  { clause: 'scripts/check-mirror-leaks.sh' },
  { clause: '--force-with-lease' },
  { clause: '--state open' },
  { clause: 'public/main:.last-mirror-sha' },
]

const missingClauses = requiredMirrorClauses
  .filter(
    ({ clause, match = (content, value) => content.includes(value) }) => !match(mirror, clause),
  )
  .map(({ clause }) => clause)

if (missingClauses.length > 0) {
  console.error('Public mirror is missing required leak-prevention clauses:')
  for (const clause of missingClauses) {
    console.error(`  - ${clause}`)
  }
  process.exit(1)
}

console.log(
  `Public mirror workflows OK (${requiredPublicWorkflows.length} workflows, ` +
    `${requiredMirrorClauses.length} leak-prevention clauses checked)`,
)
