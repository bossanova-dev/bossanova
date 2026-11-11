import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

// Repo root is the parent of scripts/.
const repoRoot = fileURLToPath(new URL('../..', import.meta.url))
const workflowsDir = path.join(repoRoot, '.github', 'workflows')

function listWorkflowFiles() {
  return fs
    .readdirSync(workflowsDir)
    .filter((name) => name.endsWith('.yml') || name.endsWith('.yaml'))
    .map((name) => path.join(workflowsDir, name))
}

const workflowFiles = listWorkflowFiles()

test('(b) no workflow installs @latest Go codegen plugins or inlines a bare Go-plugin --template', () => {
  const atLatest = /protoc-gen-(?:connect-)?go@latest/
  // Inline JSON `--template` invoking a bare Go plugin, e.g.
  // {"local":"protoc-gen-go",...} or {"local":"protoc-gen-connect-go",...}.
  const inlineBareGoPlugin = /["']local["']\s*:\s*["']protoc-gen-(?:connect-)?go["']/
  for (const file of workflowFiles) {
    const content = fs.readFileSync(file, 'utf8')
    assert.ok(
      !atLatest.test(content),
      `${path.basename(file)}: must not install protoc-gen-go/protoc-gen-connect-go @latest (drift class)`,
    )
    assert.ok(
      !inlineBareGoPlugin.test(content),
      `${path.basename(file)}: must not inline a bare Go-plugin --template (use the pinned buf.gen.yaml)`,
    )
  }
})

test('(c) buf.gen.yaml invokes the Go plugins via the go tool command-array form', () => {
  const bufGen = fs.readFileSync(path.join(repoRoot, 'buf.gen.yaml'), 'utf8')
  for (const plugin of ['protoc-gen-go', 'protoc-gen-connect-go']) {
    const arrayForm = new RegExp(
      `local:\\s*\\[\\s*["']go["']\\s*,\\s*["']tool["']\\s*,\\s*["']${plugin}["']\\s*\\]`,
    )
    assert.match(
      bufGen,
      arrayForm,
      `buf.gen.yaml must invoke ${plugin} via local: ["go","tool","${plugin}"]`,
    )
    // Guard against a regression to the bare `local: protoc-gen-go` form.
    const bareForm = new RegExp(`local:\\s*${plugin}\\s*$`, 'm')
    assert.ok(!bareForm.test(bufGen), `buf.gen.yaml must not use the bare local: ${plugin} form`)
  }
})
