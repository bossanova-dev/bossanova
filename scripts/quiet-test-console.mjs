// Test helper: silence the code-under-test's console.log / console.warn for the
// duration of each test in the calling file, so a fully-passing `make` run
// doesn't dump manifests or diagnostics into its output.
//
// The proof-* suites are the main consumer: finalizeAgentProof console.logs the
// full run-file manifest and warns "[proof-tui-agent] DEGRADED …" as intentional
// side effects that every test exercising it would otherwise print. Tests that
// assert on console output install their own capture inside the test body, which
// composes cleanly — the local capture runs during the body, then afterEach here
// restores the real console functions.
//
// Call once at module top level: `silenceConsole()`.

import { beforeEach, afterEach } from 'node:test'

export function silenceConsole({ log = true, warn = true } = {}) {
  let realLog
  let realWarn
  beforeEach(() => {
    realLog = console.log
    realWarn = console.warn
    if (log) console.log = () => {}
    if (warn) console.warn = () => {}
  })
  afterEach(() => {
    console.log = realLog
    console.warn = realWarn
  })
}
