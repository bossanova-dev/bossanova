#!/usr/bin/env node
// guard-id: boss-tui-screens-isolated
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { checkBossTuiScreens } from './boss-tui-screens.mjs'
import record from './registry/boss-tui-screens-isolated.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
export function checkBossTuiScreensIsolated({
  repoRoot = ROOT,
  baselinePath = record.baseline,
  write = false,
} = {}) {
  return checkBossTuiScreens({
    repoRoot,
    record,
    denied: ['github.com/recurser/boss/api/tui'],
    allowed: ['github.com/recurser/boss/api/tui/ui$'],
    listMode: 'lax',
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
  const result = checkBossTuiScreensIsolated({ write: values.write })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`boss-tui-screens-isolated OK (${result.offenders.length} baseline files)`)
  process.exitCode = result.failures.length ? 1 : 0
}
