import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

// Repo root is the parent of scripts/.
const repoRoot = fileURLToPath(new URL('..', import.meta.url))
const workflowsDir = path.join(repoRoot, '.github', 'workflows')

// Single source of truth: BUF_VERSION lives in the Makefile only. Everything
// else (CI buf-setup-action version:, this test) asserts against it.
function readBufVersion() {
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
  const match = makefile.match(/^BUF_VERSION\s*:=\s*(\S+)/m)
  assert.ok(match, 'Makefile must define BUF_VERSION := <version>')
  return match[1].trim()
}

function listWorkflowFiles() {
  return fs
    .readdirSync(workflowsDir)
    .filter((name) => name.endsWith('.yml') || name.endsWith('.yaml'))
    .map((name) => path.join(workflowsDir, name))
}

// Extract the block of lines belonging to a step whose `- uses:` line is at
// `startIndex`. A step runs until the next sibling list item (`-` at the same
// indentation) or a dedent below the `- ` marker.
function stepBlock(lines, startIndex) {
  const usesLine = lines[startIndex]
  const dashIndent = usesLine.search(/\S/) // column of the leading `-`
  const block = [usesLine]
  for (let i = startIndex + 1; i < lines.length; i++) {
    const line = lines[i]
    if (line.trim() === '') {
      block.push(line)
      continue
    }
    const indent = line.search(/\S/)
    // A sibling `- ...` at the same indent, or any dedent to/below the marker,
    // ends this step.
    if (indent <= dashIndent) break
    block.push(line)
  }
  return block.join('\n')
}

const bufVersion = readBufVersion()
const workflowFiles = listWorkflowFiles()

test('(a) every buf setup/action block pins version: to the Makefile BUF_VERSION', () => {
  // Both action forms install a buf CLI and must be pinned: bufbuild/buf-setup-action
  // (test-proto.yml, ci.yml) and bufbuild/buf-action with setup_only (the web/release
  // workflows). Matching only one form would advertise complete coverage while leaving
  // the other free to drift onto latest.
  const bufActionUses = /uses:\s*bufbuild\/buf-(?:setup-)?action/
  let seen = 0
  for (const file of workflowFiles) {
    const lines = fs.readFileSync(file, 'utf8').split('\n')
    for (let i = 0; i < lines.length; i++) {
      if (!bufActionUses.test(lines[i])) continue
      seen++
      const block = stepBlock(lines, i)
      const versionMatch = block.match(/\bversion:\s*["']?([^"'\s]+)["']?/)
      assert.ok(versionMatch, `${path.basename(file)}: buf action step is missing a version: pin`)
      assert.equal(
        versionMatch[1],
        bufVersion,
        `${path.basename(file)}: buf action version: must equal BUF_VERSION (${bufVersion})`,
      )
    }
  }
  assert.ok(seen > 0, 'expected at least one buf setup/action step across workflows')
})

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
