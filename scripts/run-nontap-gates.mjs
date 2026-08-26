#!/usr/bin/env node

import { spawnSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(scriptDirectory, '..')

export function renderGateFailure({ gate, exitCode, signal }) {
  const status = signal ? `signal ${signal}` : `exit code ${exitCode}`
  return [
    `not ok - non-TAP gate failed: ${gate}`,
    `# gate: ${gate}`,
    `# ${status}`,
    `# remedy: run ${gate} directly, fix the drift or violation, then rerun make test-scripts`,
    '',
  ].join('\n')
}

export function runGates(
  gates,
  {
    cwd = repoRoot,
    nodePath = process.execPath,
    spawn = spawnSync,
    stderr = process.stderr,
    stdio = 'inherit',
  } = {},
) {
  for (const gate of gates) {
    const result = spawn(nodePath, [gate], { cwd, stdio })
    if (result.status === 0 && !result.signal && !result.error) {
      continue
    }

    const exitCode = typeof result.status === 'number' ? result.status : 1
    stderr.write(renderGateFailure({ gate, exitCode, signal: result.signal }))
    return exitCode
  }

  return 0
}

if (isMainModule(import.meta.url)) {
  process.exit(runGates(process.argv.slice(2)))
}
