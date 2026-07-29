#!/usr/bin/env node
// Compatibility entrypoint for persisted Boss cron gate commands.
//
// Existing jobs store this path in GateCommand. Keep it as a thin forwarding
// entrypoint while the canonical, vendored implementation lives in
// skills-toolbox/cron-gates/boss-build.mjs.
import '../../skills-toolbox/cron-gates/boss-build.mjs'
