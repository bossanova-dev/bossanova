import assert from 'node:assert/strict'
import { test } from 'node:test'
import { findUnadmittedGolangci } from './check-golangci-admission.mjs'

test('raw analysis spawns are flagged while version probes and the admission helper pass', () => {
  const source = `import { spawnSync, execSync } from 'node:child_process'
spawnSync('golangci-lint', ['run', './...'])
execSync('golangci-lint run ./...')
spawnSync('golangci-lint', ['version'])`
  const findings = findUnadmittedGolangci([{ file: 'scripts/example.mjs', text: source }])
  assert.deepEqual(
    findings.map((f) => f.line),
    [2, 3],
  )
  assert.deepEqual(
    findUnadmittedGolangci([{ file: 'scripts/test-infra/golangci-admission.mjs', text: source }]),
    [],
  )
})
