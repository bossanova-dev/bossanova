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

// Guard the merge sequence. Pushing `public-mirror` leaves the mirror PR's
// required checks pending, so a merge issued straight afterwards fails, the job
// exits, and the `.last-mirror-sha` verification never runs — the production
// commit silently stays unpublished. The shape that fixes it is
// wait-for-checks -> synchronous merge -> verify marker, and every leg is load
// bearing:
//   - the wait must come *before* the merge, or the race is back;
//   - the merge must stay synchronous (no `--auto`), because a queued
//     auto-merge has not landed by the time the marker check runs;
//   - the marker verification must come *after* the merge, so it can only pass
//     on a merge that actually landed.

// Returns the whole shell command starting at `startIndex`, following trailing
// backslash line continuations, so flags on continuation lines are seen.
function extractShellCommand(content, startIndex) {
  const collected = []
  for (const line of content.slice(startIndex).split('\n')) {
    collected.push(line)
    if (!line.trimEnd().endsWith('\\')) break
  }
  return collected.join('\n')
}

const checksWaitIndex = mirror.indexOf('gh pr checks')
const mergeIndex = mirror.indexOf('gh pr merge')
// The marker path is also read *before* the replay, to find the resume point.
// Only the last occurrence can be the post-merge verification.
const markerVerifyIndex = mirror.lastIndexOf('public/main:.last-mirror-sha')

const sequenceProblems = []

if (mergeIndex === -1) {
  sequenceProblems.push(
    'no `gh pr merge` invocation found; the mirror pull request is never merged',
  )
} else {
  if (checksWaitIndex === -1) {
    sequenceProblems.push(
      'no `gh pr checks` wait found; the mirror pull request would be merged while its checks are still pending',
    )
  } else if (checksWaitIndex > mergeIndex) {
    sequenceProblems.push('the `gh pr checks` wait runs after `gh pr merge`; it must run before it')
  }

  if (extractShellCommand(mirror, mergeIndex).includes('--auto')) {
    sequenceProblems.push(
      'the `gh pr merge` invocation carries `--auto`; a queued auto-merge has not landed when the marker check runs, so the merge must stay synchronous',
    )
  }

  if (markerVerifyIndex < mergeIndex) {
    sequenceProblems.push(
      'the `public/main:.last-mirror-sha` verification does not run after `gh pr merge`; it must confirm a merge that actually landed',
    )
  }
}

if (sequenceProblems.length > 0) {
  console.error('Public mirror merge sequence is unsafe:')
  for (const problem of sequenceProblems) {
    console.error(`  - ${problem}`)
  }
  process.exit(1)
}

console.log(
  `Public mirror workflows OK (${requiredPublicWorkflows.length} workflows, ` +
    `${requiredMirrorClauses.length} leak-prevention clauses, ` +
    'wait-before-merge/no---auto/marker-after-merge sequence checked)',
)
