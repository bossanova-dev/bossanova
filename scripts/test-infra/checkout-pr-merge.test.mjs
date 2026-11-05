import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { classifyLines, keyValue } from '../dev/workflow-yaml-lib.mjs'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const lines = classifyLines(
  fs.readFileSync(path.join(root, '.github/actions/checkout-pr-merge/action.yml'), 'utf8'),
)
const start = lines.findIndex(
  (line) => line.kind === 'structural' && keyValue(line.text)?.key === 'run',
)
const script = lines
  .slice(start + 1)
  .filter((line) => line.kind === 'scalar')
  .map((line) => line.raw.slice(lines[start].indent + 2))
  .join('\n')

for (const [name, parents, expected] of [
  ['merge containing dispatched head', ['base', 'head'], 0],
  ['stale merge containing a different head', ['base', 'old-head'], 1],
  ['non-merge commit', ['head'], 1],
]) {
  test(name, (t) => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'checkout-pr-merge-'))
    t.after(() => fs.rmSync(temp, { recursive: true, force: true, maxRetries: 3 }))
    // Git's raw object reader exposes parent headers even in a shallow checkout.
    fs.writeFileSync(path.join(temp, 'git'), '#!/bin/bash\ncat "$MERGE_OBJECT"\n', { mode: 0o755 })
    const object = path.join(temp, 'object')
    fs.writeFileSync(
      object,
      `tree tree-id\n${parents.map((p) => `parent ${p}\n`).join('')}\nmessage\nparent head\n`,
    )
    const result = spawnSync('/bin/bash', ['-c', script], {
      encoding: 'utf8',
      env: {
        ...process.env,
        PATH: `${temp}:${process.env.PATH}`,
        MERGE_OBJECT: object,
        EXPECTED_HEAD: 'head',
      },
    })
    assert.equal(result.status, expected, result.stderr)
    if (expected) assert.match(result.stdout, /::error::.*stale or conflicted/)
  })
}
