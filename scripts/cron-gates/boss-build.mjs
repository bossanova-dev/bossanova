#!/usr/bin/env node
// Compatibility entrypoint for persisted Boss cron gate commands.
//
// Existing jobs store this path in GateCommand. Keep it as a thin forwarding
// entrypoint while the canonical, vendored implementation lives in
// skills-toolbox/cron-gates/boss-build.mjs.
//
// This calls the canonical decision function rather than importing the module for its side
// effects: the canonical module guards its entry point with `isMainModule`, so a bare
// `import` of it from here runs NOTHING and exits 0 — turning a fail-closed gate into an
// unconditional always-run.
import { evaluateBossBuildGate } from '../../skills-toolbox/cron-gates/boss-build.mjs'
import { gateExit } from '../../skills-toolbox/linear-gate-lib.mjs'
import { resolveTrackerAdapter } from '../../skills-toolbox/tracker/adapter.mjs'
import { loadSkillConfig } from '../../skills-toolbox/skill-config.mjs'

try {
  const { hasWork, reason } = await evaluateBossBuildGate({
    config: loadSkillConfig(),
    tracker: resolveTrackerAdapter(),
  })
  gateExit(hasWork, reason)
} catch (err) {
  gateExit(false, `boss-build gate: ${err.message}`)
}
