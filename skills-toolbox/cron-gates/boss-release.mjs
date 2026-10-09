#!/usr/bin/env node
import { nextReleases } from '../release-gate.mjs'
import { isMainModule } from '../main-module.mjs'

export function evaluateBossReleaseGate({ root = process.cwd(), run } = {}) {
  const next = nextReleases({ root, run })
  const hasWork = next.extension !== null && next.candidates.length > 0
  return { hasWork, reason: hasWork ? null : next.report }
}

export function main(options = {}, { stderr = (line) => process.stderr.write(line) } = {}) {
  try {
    const { hasWork, reason } = evaluateBossReleaseGate(options)
    if (!hasWork) stderr(`${reason}\n`)
    return hasWork ? 0 : 1
  } catch (error) {
    stderr(`boss-release gate: ${error.message}\n`)
    return 1
  }
}

if (isMainModule(import.meta.url)) {
  if (process.argv.length > 2) {
    process.stderr.write('boss-release gate: no arguments accepted; run from the repository root\n')
    process.exit(1)
  }
  process.exit(main())
}
