import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const securityWorkflow = fs.readFileSync(
  path.join(repoRoot, '.github/workflows/security.yml'),
  'utf8',
)

test('security workflow stays dispatch-only', () => {
  // Replaces an assertion that the push trigger's path filter covered
  // nosec-metadata-check.sh. That intent is void: as of 2026-08-07 the scans were
  // pulled off the release PRs, off the release-branch pushes, AND off the weekly
  // cron, by an explicit owner decision — so there is no automatic trigger left for
  // a path filter to live on.
  //
  // Pinned rather than deleted because each trigger costs something specific to
  // re-add by accident: `pull_request`/`push` put blocking govulncheck and
  // nosec-metadata gates back in front of every release, and `schedule` is the one
  // that sweeps unchanged code (most advisories land in the vuln DB, not in a diff).
  // Re-adding any of them should be a deliberate edit that also updates this test.
  const onBlock = securityWorkflow.match(/^on:\n((?:[ \t].*\n|\n)*)/m)?.[1] ?? ''

  assert.notEqual(onBlock, '', 'could not locate the security.yml `on:` block')
  assert.match(onBlock, /^ {2}workflow_dispatch:$/m)

  for (const trigger of ['push', 'pull_request', 'schedule']) {
    assert.doesNotMatch(
      onBlock,
      new RegExp(`^ {2}${trigger}:`, 'm'),
      `security.yml is deliberately dispatch-only, but a '${trigger}:' trigger is back`,
    )
  }
})

test('security workflow scans Linux and Darwin Go build constraints', () => {
  assert.match(securityWorkflow, /goos: \[linux, darwin\]/)
  assert.match(securityWorkflow, /GOOS: \$\{\{ matrix\.goos \}\}/)
})
