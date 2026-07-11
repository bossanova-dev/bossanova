// Guard: every CI workflow that installs golangci-lint must pin the same
// version as the Makefile's GOLANGCI_LINT_VERSION. There is no single-source
// derivation for `golangci-lint-action` inputs, so this assertion is the honest
// mechanism keeping local `make lint` and CI on the identical binary (BOS-340).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

function makefilePin() {
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
  const match = makefile.match(/^GOLANGCI_LINT_VERSION\s*:?=\s*(\S+)/m)
  assert.ok(match, 'GOLANGCI_LINT_VERSION not found in Makefile')
  return match[1]
}

// Collect the `version:` pinned to each golangci-lint-action usage across a
// workflow's YAML, as { file, version } rows.
function workflowGolangciPins() {
  const workflowsDir = path.join(repoRoot, '.github', 'workflows')
  const pins = []
  for (const name of fs.readdirSync(workflowsDir)) {
    if (!name.endsWith('.yml') && !name.endsWith('.yaml')) continue
    const lines = fs.readFileSync(path.join(workflowsDir, name), 'utf8').split('\n')
    for (let i = 0; i < lines.length; i++) {
      if (!/golangci\/golangci-lint-action/.test(lines[i])) continue
      // Scan the following lines (the `with:` block) for the first `version:`.
      for (let j = i + 1; j < Math.min(i + 12, lines.length); j++) {
        const m = lines[j].match(/^\s*version:\s*(\S+)/)
        if (m) {
          pins.push({ file: name, version: m[1] })
          break
        }
      }
    }
  }
  return pins
}

test('every workflow golangci-lint pin matches the Makefile pin', () => {
  const pin = makefilePin()
  const pins = workflowGolangciPins()
  assert.ok(pins.length > 0, 'expected at least one golangci-lint-action pin in workflows')
  for (const { file, version } of pins) {
    assert.equal(version, pin, `${file} pins golangci-lint ${version} but Makefile pins ${pin}`)
  }
})
