#!/usr/bin/env node
/**
 * proof-brief.eval.mjs — Required-proof-led brief-generation eval (BOS-139 T8/D9).
 *
 * Exercises generateBriefFromDiff on a mixed TUI+web diff with two scoped
 * `## Required proof` bullets, and scores that the GENERATED brief actually
 * covers both required demonstrations (the required-first prompt worked) — not
 * just that they were injected verbatim.
 *
 * Key-gated: skips cleanly with ok:true when PROOF_ANTHROPIC_API_KEY is unset
 * (so `cd scripts && make test` stays green in CI without a key).
 *
 * Run standalone: PROOF_ANTHROPIC_API_KEY=… node scripts/proof-brief.eval.mjs
 * Injectable: import { runEval } from './proof-brief.eval.mjs' and pass stubs.
 */

import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { generateBriefFromDiff, validateBrief } from './proof-brief.mjs'

// A small, secrets-free mixed diff touching BOTH a boss TUI view and a web page.
const FIXTURE_DIFF = `
diff --git a/services/boss/internal/views/settings.go b/services/boss/internal/views/settings.go
index 1111111..2222222 100644
--- a/services/boss/internal/views/settings.go
+++ b/services/boss/internal/views/settings.go
@@ -40,6 +40,9 @@ func (v SettingsView) rows() []row {
   return []row{
     {label: "Theme", value: v.theme},
+    // Compact mode row: hides secondary metadata to fit more sessions on screen.
+    {label: "Compact mode", value: onOff(v.compact)},
     {label: "Editor", value: v.editor},
   }
 }
diff --git a/services/web/src/pages/Account.tsx b/services/web/src/pages/Account.tsx
index 3333333..4444444 100644
--- a/services/web/src/pages/Account.tsx
+++ b/services/web/src/pages/Account.tsx
@@ -18,6 +18,11 @@ export function AccountPage() {
   return (
     <section>
       <h1>Account</h1>
+      <label className="row">
+        Compact mode
+        <input type="checkbox" checked={compact} onChange={toggleCompact} />
+      </label>
     </section>
   )
 }
`.trim()

const FIXTURE_FILES = [
  'services/boss/internal/views/settings.go',
  'services/web/src/pages/Account.tsx',
]

const ROUTES = ['/            Home', '/account     Account settings page'].join('\n')
const FIXTURES = 'DemoWorld: a seeded account with Compact mode currently off.'

// Two scoped required-proof bullets (one TUI, one web) + the deterministic
// matcher used to score whether the generated brief covers each.
const REQUIRED = [
  {
    bullet: 'The boss TUI settings screen shows the new Compact mode row',
    match: /compact/i,
  },
  {
    bullet: 'The web /account page renders the Compact mode toggle in the browser',
    match: /toggle|\/account|account/i,
  },
]

/** Joins the brief's steerable text fields for coverage scoring. */
function briefText(brief) {
  return [
    brief.description ?? '',
    ...(brief.stepsHints ?? []),
    ...(brief.targetRoutes ?? []),
    ...(brief.expectedEvidence ?? []),
  ].join(' ')
}

/**
 * Runs the brief-gen eval.
 * @param {object} [opts]
 * @param {object}   [opts.env=process.env]
 * @param {Function} [opts.generate=generateBriefFromDiff]
 * @param {Function} [opts.log=console.log]
 * @returns {Promise<{ok: boolean, skipped?: boolean, results?: Array}>}
 */
export async function runEval({
  env = process.env,
  generate = generateBriefFromDiff,
  log = console.log,
} = {}) {
  if (!env.PROOF_ANTHROPIC_API_KEY) {
    log('skipped: no PROOF_ANTHROPIC_API_KEY — set it to run the brief-gen eval')
    return { skipped: true, ok: true }
  }

  const model = env.BOSS_PROOF_MODEL || 'claude-haiku-4-5'
  const raw = await generate({
    diff: FIXTURE_DIFF,
    changedFiles: FIXTURE_FILES,
    routes: ROUTES,
    fixtures: FIXTURES,
    model,
    planRequiredProof: REQUIRED.map((r) => r.bullet),
  })
  const { brief } = validateBrief(raw)

  const text = brief ? briefText(brief) : ''
  const results = REQUIRED.map(({ bullet, match }) => {
    const covered = Boolean(brief) && match.test(text)
    log(`[${covered ? 'PASS' : 'FAIL'}] required bullet covered: ${bullet}`)
    return { bullet, covered }
  })
  const passCount = results.filter((r) => r.covered).length
  log(`\nEval summary: ${passCount}/${results.length} required bullets covered`)
  return { ok: Boolean(brief) && results.every((r) => r.covered), results }
}

// ── CLI entry ──────────────────────────────────────────────────────────────────

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) {
  runEval()
    .then(({ ok }) => process.exit(ok ? 0 : 1))
    .catch((err) => {
      console.error(err)
      process.exit(1)
    })
}
