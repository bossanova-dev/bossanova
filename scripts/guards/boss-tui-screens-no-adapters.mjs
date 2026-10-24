#!/usr/bin/env node
// guard-id: boss-tui-screens-no-adapters
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { checkBossTuiScreens } from './boss-tui-screens.mjs'
import record from './registry/boss-tui-screens-no-adapters.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
export function checkBossTuiScreensNoAdapters({
  repoRoot = ROOT,
  baselinePath = record.baseline,
  write = false,
} = {}) {
  return checkBossTuiScreens({
    repoRoot,
    record,
    denied: [
      'os/exec$',
      'github.com/recurser/boss/internal/daemonlife',
      'github.com/recurser/boss/internal/daemon',
      'github.com/recurser/boss/internal/upgrade',
    ],
    baselinePath,
    write,
  })
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      check: { type: 'boolean', default: false },
      write: { type: 'boolean', default: false },
    },
  })
  const result = checkBossTuiScreensNoAdapters({ write: values.write })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`boss-tui-screens-no-adapters OK (${result.offenders.length} baseline files)`)
  process.exitCode = result.failures.length ? 1 : 0
}
