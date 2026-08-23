import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  DOCTOR_CHECKS,
  LIVE_AGENT_CHECKS,
  PROOF_ENV_KEYS,
  binOnPath,
  defaultDoctorLookups,
  doctorReport,
  formatDoctorReport,
  requiredHeadlessShellBuild,
  requiredIdsForSurface,
} from './proof-doctor.mjs'

const repoRootForTest = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const HEADLESS_SHELL_INSTALL_COMMAND =
  'pnpm --dir services/web exec playwright install chromium-headless-shell'

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
    chromiumUnavailableReason: () => ({
      status: 'missing',
      detail: has('chromium') ? undefined : `${HEADLESS_SHELL_INSTALL_COMMAND}\nfixture detail`,
    }),
    webDepsPresent: () => has('web-node-modules'),
    goToolchainPresent: () => has('go-toolchain'),
    ghAuthOk: () => has('gh-auth'),
    gitCredentialOk: () => has('git-credential'),
    claudePluginBuilt: () => has('bossd-plugin-claude'),
  }
}

const ALL_IDS = new Set(DOCTOR_CHECKS.map((c) => c.id))

function withTempDir(prefix, fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix))
  try {
    return fn(dir)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
}

function writeJson(filePath, value) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true })
  fs.writeFileSync(filePath, JSON.stringify(value, null, 2))
}

function createPlaywrightFixture(root, browsersJsonText) {
  const webDir = path.join(root, 'services', 'web')
  const nodeModules = path.join(webDir, 'node_modules')
  writeJson(path.join(webDir, 'package.json'), { private: true })
  writeJson(path.join(nodeModules, '@playwright', 'test', 'package.json'), {
    name: '@playwright/test',
  })
  writeJson(path.join(nodeModules, 'playwright', 'package.json'), { name: 'playwright' })
  writeJson(path.join(nodeModules, 'playwright-core', 'package.json'), {
    name: 'playwright-core',
  })
  fs.writeFileSync(path.join(nodeModules, 'playwright-core', 'browsers.json'), browsersJsonText)
}

function createFixtureRepo(root, browsers) {
  createPlaywrightFixture(root, JSON.stringify({ browsers }))
}

function markInstalled(cacheDir, dirName) {
  const buildDir = path.join(cacheDir, dirName)
  fs.mkdirSync(buildDir, { recursive: true })
  fs.writeFileSync(path.join(buildDir, 'INSTALLATION_COMPLETE'), '')
}

function chromiumReportForFixture({ browsers, cacheEntries = [], createCacheRoot = true }) {
  return withTempDir('proof-doctor-repo-', (repoRoot) =>
    withTempDir('proof-doctor-cache-parent-', (cacheParent) => {
      createFixtureRepo(repoRoot, browsers)
      const cacheDir = path.join(cacheParent, 'ms-playwright')
      if (createCacheRoot) fs.mkdirSync(cacheDir, { recursive: true })
      for (const entry of cacheEntries) {
        if (entry.complete) markInstalled(cacheDir, entry.name)
        else fs.mkdirSync(path.join(cacheDir, entry.name), { recursive: true })
      }
      return doctorReport({
        surface: 'web',
        lookups: defaultDoctorLookups({
          repoRoot,
          env: { PLAYWRIGHT_BROWSERS_PATH: cacheDir, PATH: '' },
        }),
      }).checks.find((check) => check.id === 'chromium')
    }),
  )
}

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

test('formatDoctorReport leaves checks without detail byte-identical to the base line shape', () => {
  const report = doctorReport({ surface: 'tui', lookups: lookupsWith(ALL_IDS) })
  const text = formatDoctorReport(report)
  assert.ok(text.includes('bs-proof doctor — surface: tui\n\n'))
  assert.ok(text.includes('  [set] PROOF_ANTHROPIC_API_KEY (agent key) (required)\n'))
  assert.ok(!text.includes('    fixture detail'))
})

test('formatDoctorReport renders detail as an indented continuation line', () => {
  const report = doctorReport({
    surface: 'web',
    lookups: lookupsWith(new Set(['PROOF_ANTHROPIC_API_KEY'])),
  })
  const text = formatDoctorReport(report)
  assert.ok(
    text.includes(
      `  [MISSING] Playwright chrome-headless-shell (required)\n    ${HEADLESS_SHELL_INSTALL_COMMAND}`,
    ),
  )
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

// ── chrome-headless-shell resolver and probe ────────────────────────────────

test('requiredHeadlessShellBuild resolves the real services/web Playwright headless-shell revision', (t) => {
  const build = requiredHeadlessShellBuild({ repoRoot: repoRootForTest })
  if (build.status === 'unresolvable') {
    t.skip(`services/web Playwright install is not resolvable here: ${build.reason}`)
    return
  }
  assert.match(build.revision, /^\d+$/)
  assert.equal(build.dirName, `chromium_headless_shell-${build.revision}`)
  assert.ok(build.browsersJsonPath.endsWith(path.join('playwright-core', 'browsers.json')))
})

test('requiredHeadlessShellBuild picks chromium-headless-shell, not chromium decoys', () => {
  withTempDir('proof-doctor-repo-', (repoRoot) => {
    createFixtureRepo(repoRoot, [
      { name: 'chromium', revision: '1111' },
      { name: 'chromium-tip-of-tree', revision: '2222' },
      { name: 'chromium-headless-shell', revision: '3333' },
    ])
    const build = requiredHeadlessShellBuild({
      repoRoot,
      env: { PLAYWRIGHT_BROWSERS_PATH: '/tmp/pw' },
    })
    assert.equal(build.status, 'ok')
    assert.equal(build.revision, '3333')
    assert.equal(build.dirName, 'chromium_headless_shell-3333')
  })
})

test('requiredHeadlessShellBuild returns unresolvable when @playwright/test cannot be resolved', () => {
  withTempDir('proof-doctor-empty-repo-', (repoRoot) => {
    writeJson(path.join(repoRoot, 'services', 'web', 'package.json'), { private: true })
    const build = requiredHeadlessShellBuild({
      repoRoot,
      env: { PLAYWRIGHT_BROWSERS_PATH: '/tmp/pw' },
    })
    assert.equal(build.status, 'unresolvable')
    assert.match(build.reason, /@playwright\/test\/package\.json/)
  })
})

test('requiredHeadlessShellBuild returns unresolvable for unparseable browsers.json with the path', () => {
  withTempDir('proof-doctor-repo-', (repoRoot) => {
    createPlaywrightFixture(repoRoot, '{not json')
    const build = requiredHeadlessShellBuild({
      repoRoot,
      env: { PLAYWRIGHT_BROWSERS_PATH: '/tmp/pw' },
    })
    assert.equal(build.status, 'unresolvable')
    assert.match(build.reason, /could not parse browsers\.json/)
    assert.match(build.reason, /browsers\.json/)
    assert.equal(
      fs.realpathSync(build.browsersJsonPath),
      fs.realpathSync(
        path.join(repoRoot, 'services', 'web', 'node_modules', 'playwright-core', 'browsers.json'),
      ),
    )
  })
})

test('requiredHeadlessShellBuild returns unresolvable when browsers.json has no headless-shell entry', () => {
  withTempDir('proof-doctor-repo-', (repoRoot) => {
    createFixtureRepo(repoRoot, [{ name: 'chromium', revision: '1234' }])
    const build = requiredHeadlessShellBuild({
      repoRoot,
      env: { PLAYWRIGHT_BROWSERS_PATH: '/tmp/pw' },
    })
    assert.equal(build.status, 'unresolvable')
    assert.match(build.reason, /no chromium-headless-shell entry/)
    assert.match(build.reason, /browsers\.json/)
  })
})

test('chromium probe rejects full Chromium at the wanted revision without headless shell', () => {
  const check = chromiumReportForFixture({
    browsers: [{ name: 'chromium-headless-shell', revision: '1234' }],
    cacheEntries: [{ name: 'chromium-1234', complete: true }],
  })
  assert.equal(check.present, false)
  assert.equal(check.status, 'missing')
})

test('chromium probe rejects the BOS-947 stale headless-shell host state', () => {
  const check = chromiumReportForFixture({
    browsers: [{ name: 'chromium-headless-shell', revision: '1234' }],
    cacheEntries: [
      { name: 'chromium-1097', complete: true },
      { name: 'chromium-1208', complete: true },
      { name: 'chromium_headless_shell-1208', complete: true },
      { name: 'chromium_headless_shell-1228', complete: true },
    ],
  })
  assert.equal(check.present, false)
  assert.equal(check.status, 'missing')
})

test('chromium probe accepts the complete wanted headless-shell build with no detail', () => {
  const check = chromiumReportForFixture({
    browsers: [{ name: 'chromium-headless-shell', revision: '1234' }],
    cacheEntries: [{ name: 'chromium_headless_shell-1234', complete: true }],
  })
  assert.equal(check.present, true)
  assert.equal(check.status, 'ok')
  assert.equal(check.detail, undefined)
})

test('chromium probe rejects a partial headless-shell install without INSTALLATION_COMPLETE', () => {
  const check = chromiumReportForFixture({
    browsers: [{ name: 'chromium-headless-shell', revision: '1234' }],
    cacheEntries: [{ name: 'chromium_headless_shell-1234', complete: false }],
  })
  assert.equal(check.present, false)
  assert.equal(check.status, 'missing')
})

test('chromium probe reports an absent cache root distinctly from an absent build', () => {
  const check = chromiumReportForFixture({
    browsers: [{ name: 'chromium-headless-shell', revision: '1234' }],
    createCacheRoot: false,
  })
  assert.equal(check.present, false)
  assert.equal(check.status, 'missing')
  assert.match(check.detail, /Playwright cache root is absent/)
})

test('chromium missing detail carries the whole install command before the first newline', () => {
  const check = chromiumReportForFixture({
    browsers: [{ name: 'chromium-headless-shell', revision: '1234' }],
  })
  assert.equal(check.status, 'missing')
  assert.ok(check.detail.includes(HEADLESS_SHELL_INSTALL_COMMAND))
  assert.equal(check.detail.split('\n')[0], HEADLESS_SHELL_INSTALL_COMMAND)
  assert.ok(JSON.stringify(check).includes('"status":"missing"'))
})

test('chromium unresolvable detail is distinguishable from a missing build', () => {
  const check = chromiumReportForFixture({
    browsers: [{ name: 'chromium', revision: '1234' }],
  })
  assert.equal(check.present, false)
  assert.equal(check.status, 'unresolvable')
  assert.match(check.detail, /Could not resolve the required chrome-headless-shell build/)
  assert.doesNotMatch(
    check.detail,
    new RegExp(HEADLESS_SHELL_INSTALL_COMMAND.replaceAll('/', '\\/')),
  )
})

// ── defaultDoctorLookups wiring (smoke) ─────────────────────────────────────

test('defaultDoctorLookups exposes every probe seam the checks reference', () => {
  const lookups = defaultDoctorLookups({ repoRoot: '/tmp/does-not-matter' })
  for (const check of DOCTOR_CHECKS) {
    if (check.kind === 'env') assert.equal(typeof lookups.env, 'function')
    else if (check.kind === 'bin') assert.equal(typeof lookups.hasBin, 'function')
    else assert.equal(typeof lookups[check.probeKey], 'function', `missing ${check.probeKey}`)
    if (check.detailKey) {
      assert.equal(typeof lookups[check.detailKey], 'function', `missing ${check.detailKey}`)
    }
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
