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
  // Run the WHOLE list, reporting every failing gate, and return the FIRST non-zero status
  // (BOS-1276). Returning at the first failure hid every later gate in the set until a subsequent
  // CI round, which is how a two-gate breakage costs two round trips instead of one. The returned
  // status stays the first failure's so the exit code this runner has always produced is unchanged
  // for the single-failure case.
  let firstFailureStatus = 0

  for (const gate of gates) {
    const result = spawn(nodePath, [gate], { cwd, stdio })
    if (result.status === 0 && !result.signal && !result.error) {
      continue
    }

    // Never 0 on this branch: a gate that failed through a signal or a spawn error can report
    // status 0, and letting that through would make a failing set return success.
    const exitCode = typeof result.status === 'number' && result.status !== 0 ? result.status : 1
    stderr.write(renderGateFailure({ gate, exitCode, signal: result.signal }))
    if (firstFailureStatus === 0) firstFailureStatus = exitCode
  }

  return firstFailureStatus
}

if (isMainModule(import.meta.url)) {
  process.exit(runGates(process.argv.slice(2)))
}
