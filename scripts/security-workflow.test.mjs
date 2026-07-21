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

test('security workflow runs release-branch scans when the nosec gate changes', () => {
  const pushTrigger = securityWorkflow.match(/  push:\n([\s\S]*?)  pull_request:/)?.[1] ?? ''

  assert.match(pushTrigger, /^      - 'scripts\/nosec-metadata-check\.sh'$/m)
})

test('security workflow scans Linux and Darwin Go build constraints', () => {
  assert.match(securityWorkflow, /goos: \[linux, darwin\]/)
  assert.match(securityWorkflow, /GOOS: \$\{\{ matrix\.goos \}\}/)
})
