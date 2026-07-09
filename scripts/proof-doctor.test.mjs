import assert from 'node:assert/strict'
import path from 'node:path'
import { test } from 'node:test'

import {
  DOCTOR_CHECKS,
  LIVE_AGENT_CHECKS,
  PROOF_ENV_KEYS,
  binOnPath,
  defaultDoctorLookups,
  doctorReport,
  formatDoctorReport,
  requiredIdsForSurface,
} from './proof-doctor.mjs'

/**
 * Build a fully-injected lookups object. `present` is the set of check ids that
 * should probe as present; everything else probes as missing. Env keys are keyed
 * by their own id.
 */
function lookupsWith(present) {
  const has = (id) => present.has(id)
  return {
    env: (key) => (has(key) ? 'value-not-inspected' : undefined),
    hasBin: (bin) => has(bin),
    chromiumPresent: () => has('chromium'),
    webDepsPresent: () => has('web-node-modules'),
    goToolchainPresent: () => has('go-toolchain'),
    ghAuthOk: () => has('gh-auth'),
    gitCredentialOk: () => has('git-credential'),
    claudePluginBuilt: () => has('bossd-plugin-claude'),
  }
}

const ALL_IDS = new Set(DOCTOR_CHECKS.map((c) => c.id))

test('PROOF_ENV_KEYS are the five allowlisted keys', () => {
  assert.deepEqual(PROOF_ENV_KEYS, [
    'PROOF_ANTHROPIC_API_KEY',
    'CLOUDFLARE_API_TOKEN',
    'BOSS_PROOF_R2_BUCKET',
    'BOSS_PROOF_PUBLIC_BASE_URL',
    'CLOUDFLARE_ACCOUNT_ID',
  ])
})

test('doctorReport: everything present → ok, no missing', () => {
  const report = doctorReport({ surface: 'all', lookups: lookupsWith(ALL_IDS) })
  assert.equal(report.ok, true)
  assert.deepEqual(report.missing, [])
  assert.equal(report.checks.length, DOCTOR_CHECKS.length)
})

test('doctorReport: env keys report present/absent (never a value)', () => {
  const report = doctorReport({
    surface: 'all',
    lookups: lookupsWith(new Set(['PROOF_ANTHROPIC_API_KEY'])),
  })
  const key = report.checks.find((c) => c.id === 'PROOF_ANTHROPIC_API_KEY')
  const missingKey = report.checks.find((c) => c.id === 'CLOUDFLARE_API_TOKEN')
  assert.equal(key.present, true)
  assert.equal(missingKey.present, false)
  // The report carries booleans only — no secret values anywhere in the JSON.
  assert.ok(!JSON.stringify(report).includes('value-not-inspected'))
})

test('requiredIdsForSurface: tui needs the agent key + agg + ffmpeg + go, web needs chromium + web deps', () => {
  const tui = requiredIdsForSurface('tui')
  assert.ok(tui.includes('PROOF_ANTHROPIC_API_KEY'))
  assert.ok(tui.includes('agg'))
  assert.ok(tui.includes('ffmpeg'))
  assert.ok(tui.includes('go-toolchain'))
  assert.ok(!tui.includes('chromium'))

  const web = requiredIdsForSurface('web')
  assert.ok(web.includes('chromium'))
  assert.ok(web.includes('web-node-modules'))
  assert.ok(!web.includes('agg'))
})

test('requiredIdsForSurface: tui hard-requires agg+ffmpeg and does NOT require chromium/web-node-modules (BOS-216)', () => {
  // BOS-216 moves TUI stills onto ffmpeg-extracted frames from the agg-rendered
  // mp4 — so agg+ffmpeg stay hard doctor prereqs, while Chromium and the web
  // node_modules must remain OFF the TUI required-set (caption strips degrade
  // soft). This pins the doctor half of the "hard prereq" contract.
  const tui = requiredIdsForSurface('tui')
  assert.ok(tui.includes('agg'), 'agg is TUI-required (asciinema→video)')
  assert.ok(tui.includes('ffmpeg'), 'ffmpeg is TUI-required (still extraction + video)')
  assert.ok(!tui.includes('chromium'), 'chromium must NOT be required for the TUI surface')
  assert.ok(
    !tui.includes('web-node-modules'),
    'services/web/node_modules must NOT be required for the TUI surface',
  )
  // And a TUI run with chromium + web-node-modules absent still passes the doctor.
  const present = new Set(ALL_IDS)
  present.delete('chromium')
  present.delete('web-node-modules')
  assert.equal(doctorReport({ surface: 'tui', lookups: lookupsWith(present) }).ok, true)
})

test('requiredIdsForSurface: recipe needs no agent key; docs needs only push creds', () => {
  const recipe = requiredIdsForSurface('recipe')
  assert.ok(!recipe.includes('PROOF_ANTHROPIC_API_KEY'))
  assert.ok(recipe.includes('ffmpeg'))

  const docs = requiredIdsForSurface('docs')
  assert.deepEqual(docs.sort(), ['gh-auth', 'git-credential'])
})

test('requiredIdsForSurface: dry-run (no upload) drops upload + push credentials', () => {
  const web = requiredIdsForSurface('web', { shouldUpload: false })
  assert.ok(!web.includes('CLOUDFLARE_API_TOKEN'))
  assert.ok(!web.includes('gh-auth'))
  assert.ok(web.includes('ffmpeg'), 'ffmpeg is still needed to render locally')
})

test('doctorReport: per-surface scoping — a web-only gap does not fail a tui run and vice-versa', () => {
  // Everything present EXCEPT chromium (a web-only requirement).
  const present = new Set(ALL_IDS)
  present.delete('chromium')

  const tui = doctorReport({ surface: 'tui', lookups: lookupsWith(present) })
  assert.equal(tui.ok, true, 'chromium is not required for tui')

  const web = doctorReport({ surface: 'web', lookups: lookupsWith(present) })
  assert.equal(web.ok, false)
  assert.deepEqual(web.missing, ['chromium'])
})

test('doctorReport: a missing required prerequisite is listed in `missing`', () => {
  const present = new Set(ALL_IDS)
  present.delete('ffmpeg')
  present.delete('CLOUDFLARE_API_TOKEN')
  const report = doctorReport({ surface: 'web', lookups: lookupsWith(present) })
  assert.equal(report.ok, false)
  assert.ok(report.missing.includes('ffmpeg'))
  assert.ok(report.missing.includes('CLOUDFLARE_API_TOKEN'))
})

test('formatDoctorReport prints set/MISSING per line and never a value', () => {
  const report = doctorReport({
    surface: 'web',
    lookups: lookupsWith(new Set(['PROOF_ANTHROPIC_API_KEY'])),
  })
  const text = formatDoctorReport(report)
  assert.ok(text.includes('[set] PROOF_ANTHROPIC_API_KEY'))
  assert.ok(text.includes('[MISSING] ffmpeg'))
  assert.ok(text.includes('Missing required prerequisites:'))
  assert.ok(!text.includes('value-not-inspected'))
})

test('formatDoctorReport reports all-present cleanly', () => {
  const report = doctorReport({ surface: 'tui', lookups: lookupsWith(ALL_IDS) })
  assert.ok(formatDoctorReport(report).includes('All required prerequisites present.'))
})

// ── binOnPath (pure PATH probe) ─────────────────────────────────────────────

test('binOnPath finds a bin present in a PATH directory', () => {
  const env = { PATH: ['/opt/bin', '/usr/bin'].join(path.delimiter) }
  const fileExists = (p) => p === path.join('/usr/bin', 'ffmpeg')
  assert.equal(binOnPath('ffmpeg', { env, fileExists }), true)
  assert.equal(binOnPath('agg', { env, fileExists }), false)
})

test('binOnPath handles an empty PATH', () => {
  assert.equal(binOnPath('ffmpeg', { env: {}, fileExists: () => true }), false)
})

// ── defaultDoctorLookups wiring (smoke) ─────────────────────────────────────

test('defaultDoctorLookups exposes every probe seam the checks reference', () => {
  const lookups = defaultDoctorLookups({ repoRoot: '/tmp/does-not-matter' })
  for (const check of DOCTOR_CHECKS) {
    if (check.kind === 'env') assert.equal(typeof lookups.env, 'function')
    else if (check.kind === 'bin') assert.equal(typeof lookups.hasBin, 'function')
    else assert.equal(typeof lookups[check.probeKey], 'function', `missing ${check.probeKey}`)
  }
  // env getter reads process.env by key, never returns a hard-coded secret.
  assert.equal(lookups.env('DEFINITELY_UNSET_PROOF_KEY_xyz'), undefined)
})

// ── BOS-142: live-agent prerequisite gating ─────────────────────────────────

test('requiredIdsForSurface: liveAgent:false is byte-identical to the non-live web set', () => {
  const plain = requiredIdsForSurface('web')
  const live = requiredIdsForSurface('web', { liveAgent: false })
  assert.deepEqual(live, plain)
  for (const c of LIVE_AGENT_CHECKS) {
    assert.ok(!plain.includes(c.id), `${c.id} must not appear in the non-live web set`)
  }
})

test('requiredIdsForSurface: liveAgent:true appends claude + tmux + bossd-plugin-claude', () => {
  const live = requiredIdsForSurface('web', { liveAgent: true })
  for (const id of ['claude', 'tmux', 'bossd-plugin-claude']) {
    assert.ok(live.includes(id), `${id} must be required for a live-agent web run`)
  }
  // The surface's own ids are still present.
  assert.ok(live.includes('chromium'))
  assert.ok(live.includes('web-node-modules'))
})

test('doctorReport: liveAgent:false checks + missing are byte-identical to today', () => {
  const present = new Set(ALL_IDS)
  present.delete('chromium')
  const plain = doctorReport({ surface: 'web', lookups: lookupsWith(present) })
  const live = doctorReport({ surface: 'web', liveAgent: false, lookups: lookupsWith(present) })
  assert.equal(live.checks.length, DOCTOR_CHECKS.length, 'no extra check lines when not live')
  assert.deepEqual(live.checks, plain.checks)
  assert.deepEqual(live.missing, plain.missing)
  for (const c of LIVE_AGENT_CHECKS) {
    assert.ok(
      !live.checks.some((ch) => ch.id === c.id),
      `${c.id} must not appear in a non-live report`,
    )
  }
})

test('doctorReport: liveAgent:true adds the three live checks, required, present when supplied', () => {
  const present = new Set([...ALL_IDS, 'claude', 'tmux', 'bossd-plugin-claude'])
  const report = doctorReport({ surface: 'web', liveAgent: true, lookups: lookupsWith(present) })
  assert.equal(report.checks.length, DOCTOR_CHECKS.length + LIVE_AGENT_CHECKS.length)
  for (const id of ['claude', 'tmux', 'bossd-plugin-claude']) {
    const check = report.checks.find((c) => c.id === id)
    assert.ok(check, `${id} check must be present in a live report`)
    assert.equal(check.required, true, `${id} must be required`)
    assert.equal(check.present, true, `${id} must probe present via the spy lookups`)
  }
  assert.deepEqual(report.missing, [], 'nothing missing when all live prereqs supplied')
})

test('doctorReport: liveAgent:true with claude absent names it in missing', () => {
  const present = new Set([...ALL_IDS, 'tmux', 'bossd-plugin-claude']) // claude absent
  const report = doctorReport({ surface: 'web', liveAgent: true, lookups: lookupsWith(present) })
  assert.equal(report.ok, false)
  assert.ok(report.missing.includes('claude'), 'absent claude must be named in missing')
  assert.ok(!report.missing.includes('tmux'))
  assert.ok(!report.missing.includes('bossd-plugin-claude'))
})

test('defaultDoctorLookups exposes claudePluginBuilt for the live plugin probe', () => {
  const lookups = defaultDoctorLookups({ repoRoot: '/tmp/does-not-matter' })
  assert.equal(typeof lookups.claudePluginBuilt, 'function')
  // No plugin binary under a bogus repo root → probes false, never throws.
  assert.equal(lookups.claudePluginBuilt(), false)
})
