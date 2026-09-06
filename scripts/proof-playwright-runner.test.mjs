#!/usr/bin/env node

import { spawnSync } from 'node:child_process'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  buildSpec,
  validateRecipe,
  slugify,
  parseArgs,
  stageEnvForArgs,
  OVERLAY_CAPTION_CSS,
  VIDEO_ACTIONS,
  uploadServerScript,
  echoServerScript,
  collectProofAuditTextScript,
} from './proof-playwright-runner.mjs'
import { OVERLAY_CAPTION_CSS as SPEC_OVERLAY_CAPTION_CSS } from './proof-caption-spec.mjs'
import { precedes, region } from './gate-region-lib.mjs'

const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))
const runnerPath = path.join(repoRoot, 'scripts/proof-playwright-runner.mjs')

test('buildSpec video branch records webm via its own context and screenshots a poster', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      steps: [
        { action: 'goto', route: '/' },
        { action: 'click', selector: '[data-testid="row"]' },
        { action: 'type', selector: 'input[name="q"]', value: 'hello' },
        { action: 'press', key: '?' },
        { action: 'wait', timeoutMs: 500 },
      ],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /recordVideo/)
  assert.match(spec, /newContext/)
  assert.match(spec, /web-flow\.webm/)
  assert.match(spec, /web-flow\.png/) // poster
  assert.match(spec, /\.click\(\)/)
  assert.match(spec, /pressSequentially\('hello', \{ delay: 60 \}\)/)
  assert.match(spec, /page\.keyboard\.press\('\?'\)/)
  assert.match(spec, /context\.close\(\)/) // finalizes the video before rename
})

test('buildSpec stages a closing attach socket only for the reconnecting chat recipe', () => {
  const reconnectingSpec = buildSpec({
    recipe: { id: 'web-chat-terminal-reconnecting', surface: 'web', route: '/' },
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })
  const healthySpec = buildSpec({
    recipe: { id: 'web-chat-terminal', surface: 'web', route: '/' },
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })

  assert.match(reconnectingSpec, /ws\.close\(\)/)
  assert.doesNotMatch(reconnectingSpec, /ws\.send\(\)/)
  assert.match(healthySpec, /ws\.send\(Buffer\.from/)
})

test('buildSpec stages a signed-in organization only for the recipes that name one', () => {
  // The shared web fixture stages no organization, so a recipe entering the
  // organization-subject settings route without this init script photographs
  // OrgScopedSettings' notice instead of its subject. Applied globally it would
  // instead hand every unrelated web still an organization it never asked for.
  const orgSpec = buildSpec({
    recipe: { id: 'web-org-create-modal', surface: 'web', route: '/org-e2e/settings/organization' },
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })
  const otherSpec = buildSpec({
    recipe: { id: 'web-header-menu', surface: 'web', route: '/' },
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })

  assert.match(orgSpec, /organizationId: 'workos-e2e'/)
  assert.doesNotMatch(otherSpec, /organizationId/)

  // The fakes resolve their fixture as `__BOSSANOVA_E2E__ ?? bossanovaE2e`, so a
  // staged spec must write through to whichever global is installed. A
  // bossanovaE2e-only write is invisible on a page that already has the other.
  assert.match(
    orgSpec,
    /window\.__BOSSANOVA_E2E__ = \{ \.\.\.window\.__BOSSANOVA_E2E__, \.\.\.staged \}/,
  )
  assert.doesNotMatch(otherSpec, /__BOSSANOVA_E2E__/)
})

// The ratchet on the dual-global staging convention. Every stage script that
// mirrors into the second fixture global now routes through stageFixtureScript
// -- 11 call sites across 10 functions, all of the runner's mirroring sites --
// and the reason it has to (the fakes resolve `__BOSSANOVA_E2E__ ??
// bossanovaE2e`, a precedence rather than a merge) is stated once, on that
// helper. A hand-written copy would restate the convention in a second place and
// could drift from it silently, so pin the count: exactly one ASSIGNMENT to the
// mirror global exists in the runner's own source. Assignments, not mentions --
// the module's prose names the global many times over, and matching the bare
// identifier would count comments as copies. Deliberately not covered: the stage
// scripts that write `bossanovaE2e` alone (sessionOrganizationStageScript and
// its siblings, which explain their own reason) and the ones that stage no
// fixture global at all.
test('the runner assigns the mirror fixture global in exactly one place', () => {
  const source = fs.readFileSync(new URL('./proof-playwright-runner.mjs', import.meta.url), 'utf8')

  const assignments = source.match(/window\.__BOSSANOVA_E2E__ = \{/g) ?? []

  assert.equal(
    assignments.length,
    1,
    `expected exactly one window.__BOSSANOVA_E2E__ assignment (stageFixtureScript's), found ${assignments.length} -- route the new staging through stageFixtureScript instead of copying the mirror`,
  )
})

test('buildSpec stages repository holder organizations only for repository list proof', () => {
  const repositorySpec = buildSpec({
    recipe: { id: 'web-repositories', surface: 'web', route: '/org-e2e/settings/repos' },
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })
  const editSpec = buildSpec({
    recipe: {
      id: 'web-repository-organization-refusal',
      surface: 'web',
      route: '/org-e2e/settings/repos/example',
    },
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })

  assert.match(repositorySpec, /repoOrganizations:/)
  assert.match(repositorySpec, /org-proof-acme/)
  assert.match(repositorySpec, /org-proof-globex/)
  assert.match(repositorySpec, /org-proof-initech/)
  assert.doesNotMatch(editSpec, /repoOrganizations:/)
})

test('repository filter proof captures all organizations before a narrowed-empty state', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find(
    (candidate) => candidate.id === 'web-repositories-daemon-filter-flow',
  )
  assert.ok(recipe, 'web-repositories-daemon-filter-flow recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  assert.match(recipe.title, /Organization Filter Flow/)
  assert.match(recipe.description, /All organizations/)

  const allOrganizationsIndex = recipe.steps.findIndex(
    (step) =>
      step.action === 'select' &&
      step.selector === '[data-testid="repository-organization-filter"]' &&
      step.value === '',
  )
  const emptyOrganizationIndex = recipe.steps.findIndex(
    (step) =>
      step.action === 'select' &&
      step.selector === '[data-testid="repository-organization-filter"]' &&
      step.value === 'org-proof-initech',
  )
  const emptyStateIndex = recipe.steps.findIndex(
    (step) =>
      step.action === 'wait' &&
      step.selector === 'text=No repositories belong to the selected organization',
  )
  assert.ok(allOrganizationsIndex >= 0, 'catalog must show All organizations')
  assert.ok(
    allOrganizationsIndex < emptyOrganizationIndex && emptyOrganizationIndex < emptyStateIndex,
    'catalog must select the empty Initech organization before waiting for its empty state',
  )

  const spec = buildSpec({
    recipe,
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })

  const allOrganizationsStep = region(
    spec,
    "caption(t), 'All organizations are included'",
    "caption(t), 'Filter to an organization with no matching repositories'",
    'all-organizations proof step',
  )
  assert.match(allOrganizationsStep, /repository-organization-filter/)
  assert.match(allOrganizationsStep, /selectOption\('\'\)/)
  const emptyOrganizationStep = region(
    spec,
    "caption(t), 'Filter to an organization with no matching repositories'",
    "caption(t), 'No repositories belong to the selected organization'",
    'empty-organization proof step',
  )
  assert.match(emptyOrganizationStep, /repository-organization-filter/)
  assert.match(emptyOrganizationStep, /selectOption\('org-proof-initech'\)/)
  assert.ok(
    precedes(
      spec,
      "caption(t), 'All organizations are included'",
      "caption(t), 'Filter to an organization with no matching repositories'",
      "page.locator('text=No repositories belong to the selected organization')",
    ),
    'organization proof must show the all-organizations control before selecting an empty organization and waiting for its empty state',
  )
})

test('the shipped catalog keeps the org-create-modal recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-org-create-modal')
  assert.ok(recipe, 'web-org-create-modal recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // The modal is opened by clicks, and buildSpec's still branch never runs a
  // recipe's steps — a "still" capture here would silently photograph the bare
  // route with no modal, so the capture kind is load-bearing.
  assert.equal(recipe.capture, 'video')
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule, 'no pathRule matches services/web/')
  assert.ok(webRule.recipeIds.includes('web-org-create-modal'))
})

// Standing invariant, not a fact about any one recipe: the settings-page
// OrgPicker only renders once useAuth() reports an organizationId, and the
// shared web fixture stages none. So any recipe that reaches it must be staged
// by organizationStageScript, or it silently photographs a frame with no control
// in it. Deriving the expectation from the catalog rather than restating a
// hardcoded id means a recipe added later - or the staging rule narrowed later -
// fails here loudly.
//
// The match is on the picker's `org-picker` test id rather than on a CSS class.
// The control used to be a button plus a hand-rolled `.org-switcher-*` menu; it
// is a native <select> now, and a class-based predicate would have gone on
// matching NOTHING while this test stayed green on its `length > 0` guard alone
// only until that guard was reached. The test id is the handle the recipes and
// the e2e spec both drive, so it survives the next restyling too.
//
// "Reaches the switcher" has to be read off every selector-bearing field, not
// just one: a still declares its subject in cropToSelector while a flow's are
// all step selectors, so a step-selector-only predicate would match nothing for
// a cropped still and leave the staging it depends on unguarded, while still
// leaving this file green.
function pickerSelectors(recipe) {
  return [
    recipe.cropToSelector,
    recipe.selector,
    ...(recipe.steps ?? []).flatMap((step) => [step.selector, step.toSelector]),
  ]
}

test('every catalog recipe that drives the org picker is staged with an organization', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const pickerRecipes = catalog.recipes.filter((recipe) =>
    pickerSelectors(recipe).some((selector) => (selector ?? '').includes('org-picker')),
  )
  assert.ok(pickerRecipes.length > 0, 'no catalog recipe drives the org picker')

  for (const recipe of pickerRecipes) {
    const spec = buildSpec({
      recipe,
      outputDir: '/tmp/out',
      surface: recipe.surface ?? 'web',
      stageEnv: { VITE_E2E: '1' },
    })
    assert.match(
      spec,
      /organizationId: '[a-z0-9-]+'/,
      recipe.id + ' drives the org picker but buildSpec stages no organization',
    )
  }
})

test('buildSpec replays the BOS-658 glyph line only for the plain chat-terminal still', () => {
  const specFor = (id) =>
    buildSpec({
      recipe: { id, surface: 'web', route: '/' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })

  // The captured canvas must carry real terminal output, so the healthy
  // chat-terminal socket replays a kind=0 data frame holding the evidence
  // glyphs. Every other web recipe keeps the original clients-frame-only
  // socket.
  const chatTerminal = specFor('web-chat-terminal')
  assert.match(chatTerminal, /✓ ↳ ─/)
  assert.match(chatTerminal, /Buffer\.concat\(\[header, payload\]\)/)

  assert.doesNotMatch(specFor('web-sessions'), /✓ ↳ ─/)
  assert.doesNotMatch(specFor('web-chat-terminal-reconnecting'), /✓ ↳ ─/)
})

test('buildSpec stages the BOS-661 upload responder only for the upload recipe', () => {
  const specFor = (id) =>
    buildSpec({
      recipe: { id, surface: 'web', route: '/' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })

  // Without a staged responder the browser's uploadFile() never receives a
  // kind=11 result, so the 'Upload complete' banner the recipe promises never
  // renders and the capture would hang on its final wait step.
  const upload = specFor('web-chat-terminal-upload')
  assert.match(upload, /ws\.onMessage\(/)
  assert.match(upload, /__frame\(10, Buffer\.concat\(\[id, seq, acked\]\)\)/) // chunk ack
  assert.match(upload, /__frame\(11, Buffer\.concat\(\[id, Buffer\.from\(\[0x01\]\)/) // ok result
  // The control's only job is to click the hidden <input type=file>, which
  // Chromium reports as a filechooser — the click step is inert without this.
  assert.match(upload, /page\.on\('filechooser'/)
  assert.match(upload, /agent-brief\.txt/)
  // The page clears its "Connecting…" overlay on the first data byte only, so
  // the upload capture replays one too.
  assert.match(upload, /✓ ↳ ─/)

  for (const id of ['web-sessions', 'web-chat-terminal', 'web-chat-terminal-reconnecting']) {
    assert.doesNotMatch(specFor(id), /filechooser/)
    assert.doesNotMatch(specFor(id), /ws\.onMessage\(/)
  }
})

// ----- BOS-661 staged upload responder: the ok flag must be CONDITIONAL -----
//
// The web-chat-terminal-upload video is the only end-to-end artefact for the
// browser side of BOS-661, and its final step waits for the 'Upload complete'
// banner. That banner is painted from the responder's kind=11 ok flag, so a
// responder that answers ok unconditionally makes the capture unfalsifiable:
// a browser that sent upload_start + upload_finish and NO chunks would still
// pass. These tests drive the generated snippet directly against a fake socket
// and pin the failure paths, so the proof keeps its ability to fail.

const UPLOAD_ID = Buffer.from('proof-upload-1', 'ascii')
const UPLOAD_ID_PREFIX = Buffer.concat([Buffer.from([UPLOAD_ID.length]), UPLOAD_ID])

function u64be(value) {
  const buf = Buffer.alloc(8)
  buf.writeUInt32BE(Math.floor(value / 4294967296), 0)
  buf.writeUInt32BE(value >>> 0, 4)
  return buf
}

function attachEnvelope(kind, payload) {
  const header = Buffer.from([
    kind,
    (payload.length >>> 16) & 0xff,
    (payload.length >>> 8) & 0xff,
    payload.length & 0xff,
  ])
  return Buffer.concat([header, payload])
}

function uploadStartFrame(sizeBytes, filename = 'agent-brief.txt') {
  const name = Buffer.from(filename, 'utf8')
  const nameLen = Buffer.alloc(2)
  nameLen.writeUInt16BE(name.length, 0)
  return attachEnvelope(6, Buffer.concat([UPLOAD_ID_PREFIX, u64be(sizeBytes), nameLen, name]))
}

function uploadChunkFrame(seq, data) {
  return attachEnvelope(7, Buffer.concat([UPLOAD_ID_PREFIX, u64be(seq), Buffer.from(data, 'utf8')]))
}

function uploadFinishFrame() {
  return attachEnvelope(8, UPLOAD_ID_PREFIX)
}

// Runs the staged responder snippet against a fake `ws` and returns every
// frame it sent back, decoded to { kind, ok, message }.
function runUploadResponder(frames) {
  const source = uploadServerScript({ id: 'web-chat-terminal-upload' })
  assert.ok(source.includes('ws.onMessage('), 'responder snippet is empty')
  const sent = []
  let handler = null
  const ws = {
    send: (buf) => sent.push(Buffer.from(buf)),
    onMessage: (fn) => {
      handler = fn
    },
  }
  // eslint-disable-next-line no-new-func -- the snippet is generated by this repo
  new Function('ws', 'Buffer', source)(ws, Buffer)
  assert.ok(handler, 'responder registered no message handler')
  for (const frame of frames) {
    handler(frame)
  }
  return sent.map((buf) => {
    const payload = buf.subarray(4)
    const idLen = payload[0]
    return {
      kind: buf[0],
      ok: buf[0] === 11 ? (payload[1 + idLen] & 0x01) === 0x01 : undefined,
      message: buf[0] === 11 ? payload.subarray(2 + idLen).toString('utf8') : '',
    }
  })
}

const UPLOAD_BODY = 'Fixture upload for the BOS-661 chat file upload proof.\n'
const UPLOAD_SIZE = Buffer.byteLength(UPLOAD_BODY, 'utf8')

test('staged upload responder reports ok only for a complete, in-order transfer', () => {
  const results = runUploadResponder([
    uploadStartFrame(UPLOAD_SIZE),
    uploadChunkFrame(0, UPLOAD_BODY),
    uploadFinishFrame(),
  ]).filter((frame) => frame.kind === 11)

  assert.equal(results.length, 1, 'exactly one terminal frame per upload id')
  assert.equal(results[0].ok, true)
  assert.equal(results[0].message, '', 'error_message is empty when ok')
})

test('staged upload responder FAILS an upload whose chunks never arrived', () => {
  // The exact regression the video has to be able to catch: start + finish
  // with zero bytes in between.
  const results = runUploadResponder([uploadStartFrame(UPLOAD_SIZE), uploadFinishFrame()]).filter(
    (frame) => frame.kind === 11,
  )

  assert.equal(results.length, 1)
  assert.equal(results[0].ok, false)
  assert.match(results[0].message, /incomplete upload: received 0 of \d+ bytes/)
})

test('staged upload responder FAILS a short transfer', () => {
  const results = runUploadResponder([
    uploadStartFrame(UPLOAD_SIZE),
    uploadChunkFrame(0, UPLOAD_BODY.slice(0, 10)),
    uploadFinishFrame(),
  ]).filter((frame) => frame.kind === 11)

  assert.equal(results.length, 1)
  assert.equal(results[0].ok, false)
  assert.match(results[0].message, /incomplete upload/)
})

test('staged upload responder FAILS an out-of-order chunk and an unstarted upload', () => {
  const skipped = runUploadResponder([
    uploadStartFrame(UPLOAD_SIZE),
    uploadChunkFrame(1, UPLOAD_BODY), // seq 0 was never sent
  ]).filter((frame) => frame.kind === 11)
  assert.equal(skipped.length, 1)
  assert.equal(skipped[0].ok, false)
  assert.match(skipped[0].message, /out-of-order chunk/)

  const unstarted = runUploadResponder([uploadFinishFrame()]).filter((frame) => frame.kind === 11)
  assert.equal(unstarted.length, 1)
  assert.equal(unstarted[0].ok, false)
  assert.match(unstarted[0].message, /unknown upload id/)
})

test('staged upload responder acks each chunk with the cumulative byte count', () => {
  const half = Math.floor(UPLOAD_SIZE / 2)
  const acks = runUploadResponder([
    uploadStartFrame(UPLOAD_SIZE),
    uploadChunkFrame(0, UPLOAD_BODY.slice(0, half)),
    uploadChunkFrame(1, UPLOAD_BODY.slice(half)),
    uploadFinishFrame(),
  ])

  assert.equal(acks.filter((frame) => frame.kind === 10).length, 2)
  const results = acks.filter((frame) => frame.kind === 11)
  assert.equal(results.length, 1)
  assert.equal(results[0].ok, true, 'a chunked-but-complete transfer still succeeds')
})

test('the shipped catalog keeps the upload recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-chat-terminal-upload')
  assert.ok(recipe, 'web-chat-terminal-upload recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // A recipe absent from the pathRule is never selected for a services/web
  // diff, so it would silently prove nothing.
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule.recipeIds.includes('web-chat-terminal-upload'))
})

test('the shipped catalog keeps the paste recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-chat-terminal-paste')
  assert.ok(recipe, 'web-chat-terminal-paste recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // Same hazard as the upload recipe above: absent from the pathRule it is
  // never selected for a services/web diff, so BOS-879's required proof would
  // silently never run.
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule.recipeIds.includes('web-chat-terminal-paste'))
})

test('the session-expired recipe stages the fixture flag into BOTH e2e globals', () => {
  const specFor = (id) =>
    buildSpec({
      recipe: { id, surface: 'web', capture: 'still', route: '/' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })

  const staged = specFor('web-session-expired')
  assert.match(staged, /authRefreshFailed: true/)
  // The app fakes resolve `__BOSSANOVA_E2E__ ?? bossanovaE2e`, a precedence and
  // not a merge, so a single-global write is invisible wherever the other is
  // already installed. Nothing in the proof runner installs it today, so this
  // mirror is latent rather than load-bearing; it is pinned so a future writer
  // cannot quietly turn this recipe into an unstaged capture. Were that to
  // happen the readiness gate asserted below is what makes it a failing spec
  // rather than a screenshot of a healthy signed-in app.
  assert.match(staged, /window\.bossanovaE2e = \{ \.\.\.window\.bossanovaE2e, \.\.\.staged \}/)
  assert.match(
    staged,
    /window\.__BOSSANOVA_E2E__ = \{ \.\.\.window\.__BOSSANOVA_E2E__, \.\.\.staged \}/,
  )

  // The live control. The flag replaces the whole app with the notice, so a
  // staging that leaked into the shared web fixture would blank out every other
  // web recipe's subject while this test still passed.
  assert.doesNotMatch(specFor('web-sessions'), /authRefreshFailed/)
})

test('the accounts-probe recipe stages the cut-short daemon into BOTH e2e globals', () => {
  // Built from the SHIPPED catalog rather than a synthetic literal: this test's
  // subject is that the staging reaches exactly one recipe id, and against a
  // stand-in it would keep passing after the id was renamed out of the catalog
  // -- a recipe capturing an unstaged refresh with the gate still green.
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const specFor = (id) => {
    const recipe = catalog.recipes.find((r) => r.id === id)
    assert.ok(recipe, `${id} recipe is missing from the catalog`)
    return buildSpec({
      recipe,
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })
  }

  const staged = specFor('web-accounts-refresh-interrupted')
  // The daemon id is asserted, not just the key: staging the WRONG daemon would
  // cut short the one that owns the rows the capture is about, which is the
  // opposite recipe -- and an all-legs cancellation, which is the shape the unit
  // tests pin rather than the recovery this video is proof of.
  assert.match(staged, /accountsProbeErrors: \{ 'daemon-proof-standby': /)
  // The app fakes resolve `__BOSSANOVA_E2E__ ?? bossanovaE2e`, a precedence and
  // not a merge, so a single-global write is invisible wherever the other is
  // already installed. Latent today (nothing in the runner installs the other
  // global) and pinned for the same reason the session-expired mirror above is.
  assert.match(staged, /window\.bossanovaE2e = \{ \.\.\.window\.bossanovaE2e, \.\.\.staged \}/)
  assert.match(
    staged,
    /window\.__BOSSANOVA_E2E__ = \{ \.\.\.window\.__BOSSANOVA_E2E__, \.\.\.staged \}/,
  )

  // The live control, and the reason this staging is recipe-scoped at all: a
  // leak into the shared web fixture would leave every OTHER accounts recipe
  // photographing a permanently cut-short probe -- an interruption notice over
  // the very table they exist to show -- while this test still passed.
  for (const id of ['web-accounts-list', 'web-accounts-filter-flow', 'web-accounts-refresh-flow']) {
    assert.doesNotMatch(specFor(id), /accountsProbeErrors/)
  }
})

test('the session-expired spec waits for the notice before it screenshots', () => {
  // Built from the SHIPPED recipes, not from a synthetic literal, because WHICH
  // buildSpec branch emits the screenshot depends on whether the recipe declares
  // `selector` or `cropToSelector` -- and this test's whole subject is where the
  // gate lands relative to that screenshot. Against a stand-in, adding either
  // field to the catalog entry would move the capture into a different branch
  // with this assertion still green, which is the gate covering a recipe shape
  // the catalog no longer ships. The registration test below reads the same
  // catalog; the control `web-sessions` is a cropToSelector recipe, so the pair
  // spans both branches as they are actually shipped.
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const specFor = (id) => {
    const recipe = catalog.recipes.find((r) => r.id === id)
    assert.ok(recipe, `${id} recipe is missing from the catalog`)
    return buildSpec({
      recipe,
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })
  }

  // The recipe declares neither `selector` nor `cropToSelector`, so buildSpec's
  // third branch screenshots straight after page.goto with no assertion of its
  // own -- and the subject arrives from an EFFECT (the fake provider fires
  // onRefreshFailure on mount, which latches the store and only then re-renders
  // Layout). Without this wait the capture races the very transition it is proof
  // of, and a staging that stopped landing would be photographed as a healthy
  // signed-in app with the spec still green.
  const staged = specFor('web-session-expired')
  assert.match(staged, /await expect\(page\.getByText\('Session expired'\)\)\.toBeVisible\(/)
  // After the navigation and before the still, or it gates nothing -- the same
  // pair the subscribe test below asserts.
  //
  // BOS-737: `precedes()` rather than the raw `indexOf < indexOf`. Raw, deleting
  // the gate -- the exact regression this pins -- makes `indexOf` return -1, and
  // `-1 < n` is true, so the assertion goes green on precisely the spec it exists
  // to forbid. `precedes()` throws on an absent marker instead. That matters more
  // here than almost anywhere: the defect this gate catches is one whose only
  // other symptom is a healthy-looking screenshot and a passing spec.
  assert.ok(precedes(staged, 'page.goto(', 'Session expired'))
  assert.ok(precedes(staged, 'Session expired', 'page.screenshot('))

  // The live control: the wait is recipe-scoped. Applied to every web still it
  // would hang each one that never shows the notice -- which is all of them.
  assert.doesNotMatch(specFor('web-sessions'), /Session expired/)
})

test('the shipped catalog keeps the session-expired recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-session-expired')
  assert.ok(recipe, 'web-session-expired recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // Same hazard as the recipes below: absent from the pathRule it is never
  // selected for a services/web diff, so BOS-1085's required proof would
  // silently never run.
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule.recipeIds.includes('web-session-expired'))
  // The id the stage script gates on is the id the catalog ships; a drift
  // between them captures the signed-in app instead of the expired state.
  assert.equal(recipe.capture, 'still')
  assert.equal(recipe.route, '/')
})

test('the daemon give-up recipe stages a NON-transient listDaemons failure into both e2e globals', () => {
  const specFor = (id) =>
    buildSpec({
      recipe: { id, surface: 'web', capture: 'still', route: '/' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })

  const staged = specFor('web-sessions-daemons-give-up')
  assert.match(staged, /errors: \{ listDaemons: \{ message: 'Daemon list unavailable', code: 13 \}/)
  // Code 13 is INTERNAL, and the code is the whole point: the page classifies
  // retryability by code, so a transient one would leave the capture racing
  // ~15s of retry ladder for a "Reconnecting…" pill instead of the give-up
  // notice this recipe is evidence of. Pinned as a number because that is what
  // the fake reads.
  assert.match(staged, /code: 13/)
  // Same latent-mirror reasoning as the session-expired test above.
  assert.match(staged, /window\.bossanovaE2e = \{ \.\.\.window\.bossanovaE2e, \.\.\.staged \}/)
  assert.match(
    staged,
    /window\.__BOSSANOVA_E2E__ = \{ \.\.\.window\.__BOSSANOVA_E2E__, \.\.\.staged \}/,
  )

  // The live control. Staged globally, an empty daemon filter would silently
  // change every other web recipe's subject.
  assert.doesNotMatch(specFor('web-sessions'), /listDaemons/)
})

test('the daemon give-up spec waits for the notice before it screenshots', () => {
  // Built from the SHIPPED catalog for the reason spelled out on the
  // session-expired gate above: which buildSpec branch emits the screenshot
  // depends on the recipe's own selector/cropToSelector fields.
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const specFor = (id) => {
    const recipe = catalog.recipes.find((r) => r.id === id)
    assert.ok(recipe, `${id} recipe is missing from the catalog`)
    return buildSpec({
      recipe,
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })
  }

  // The notice arrives only after a poll has actually failed, so without the
  // gate the still races it and photographs a page with a healthy daemon
  // filter — green spec, wrong subject.
  const staged = specFor('web-sessions-daemons-give-up')
  assert.match(staged, /await expect\(page\.getByRole\('button', \{ name: 'Try again' \}\)\)/)
  assert.ok(precedes(staged, 'page.goto(', "name: 'Try again'"))
  assert.ok(precedes(staged, "name: 'Try again'", 'page.screenshot('))

  // Recipe-scoped: applied to every web still it would hang each one that has
  // no failing poll, which is all of them.
  assert.doesNotMatch(specFor('web-sessions'), /Try again/)
})

test('the repo-organization refusal recipe stages a PermissionDenied set into both e2e globals', () => {
  const specFor = (id) =>
    buildSpec({
      recipe: { id, surface: 'web', capture: 'still', route: '/org-e2e/settings/repos' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })

  const staged = specFor('web-repository-organization-refusal')
  assert.match(
    staged,
    /setRepoOrganization: \{ message: 'organization membership required', code: 7 \}/,
  )
  // Code 7 is PERMISSION_DENIED, and the classifier keys on it and on nothing
  // else — any other code falls straight through to the server's own sentence,
  // which is the pre-BOS-1114 behaviour this capture is evidence against.
  assert.match(staged, /code: 7/)
  // Only the WRITE is refused. Refusing the read as well would leave the field
  // never learning the repo is unmapped, and refusing the clear would break a
  // release this recipe never drives.
  assert.doesNotMatch(staged, /getRepoOrganization:/)
  assert.doesNotMatch(staged, /clearRepoOrganization:/)
  // Same latent-mirror reasoning as the session-expired test above.
  assert.match(staged, /window\.bossanovaE2e = \{ \.\.\.window\.bossanovaE2e, \.\.\.staged \}/)
  assert.match(
    staged,
    /window\.__BOSSANOVA_E2E__ = \{ \.\.\.window\.__BOSSANOVA_E2E__, \.\.\.staged \}/,
  )

  // The live control. Staged globally it would refuse the write on every other
  // repository recipe, whose subjects are not this one.
  assert.doesNotMatch(specFor('web-repository-edit'), /setRepoOrganization/)
})

test('the shipped catalog keeps the repo-organization refusal recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-repository-organization-refusal')
  assert.ok(recipe, 'web-repository-organization-refusal recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // Absent from the pathRule it is never selected for a services/web diff, and
  // BOS-1114's required proof would silently never run.
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule.recipeIds.includes('web-repository-organization-refusal'))
  // The refusal only exists after a selection, and buildSpec's still branch
  // never runs a recipe's steps — a "still" here would photograph the bare
  // repository list with no control and no refusal in it.
  assert.equal(recipe.capture, 'video')
  // The negative evidence IS the ticket: the picker offers only organizations
  // the caller belongs to, so a non-membership claim would be certainly wrong.
  // A fresh-context judge grades against the description, so the prohibition has
  // to be stated there or it is graded by nobody.
  assert.match(recipe.description, /not a member/)
  assert.match(recipe.description, /must NOT appear/)
  // The refusal sentence has to be waited for, or the video ends before the
  // banner it is evidence of has rendered.
  const waited = recipe.steps.some(
    (step) =>
      step.action === 'wait' &&
      (step.selector ?? '').includes('check that your Bossanova Cloud subscription is active'),
  )
  assert.ok(waited, 'the refusal recipe never waits for the classified sentence')
})

test('the shipped catalog keeps the daemon give-up recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-sessions-daemons-give-up')
  assert.ok(recipe, 'web-sessions-daemons-give-up recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // Absent from the pathRule it is never selected for a services/web diff, and
  // BOS-1091's required proof would silently never run.
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule.recipeIds.includes('web-sessions-daemons-give-up'))
  assert.equal(recipe.capture, 'still')
  assert.equal(recipe.route, '/')
  // Uncropped on purpose: the evidence is the notice AND the surviving table in
  // one frame, and web-sessions' `.data-table-wrap` crop would cut the notice
  // out entirely.
  assert.equal(recipe.cropToSelector, undefined)
  assert.equal(recipe.selector, undefined)
})

test('the cold-start probe-failure spec clicks Refresh and waits for the compat notice', () => {
  // Built from the SHIPPED recipe for the same reason the session-expired test
  // above is: which buildSpec branch emits the screenshot depends on whether the
  // catalog entry declares `selector` or `cropToSelector`, and this test's whole
  // subject is where the staging lands relative to that screenshot.
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const specFor = (id) => {
    const recipe = catalog.recipes.find((r) => r.id === id)
    assert.ok(recipe, `${id} recipe is missing from the catalog`)
    return buildSpec({
      recipe,
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })
  }

  const staged = specFor('web-accounts-cold-start-probe-failure')
  // Both fixture flags, and both e2e globals. The subject only exists when the
  // passive read is still in flight (hangPassiveAccountsRead) AND the refresh
  // probe is answered the way an older bosso answers it
  // (accountsUsageRefreshUnsupported). Drop either and the capture is a healthy
  // accounts table; the app fakes resolve `__BOSSANOVA_E2E__ ?? bossanovaE2e`,
  // a precedence and not a merge, so a single-global write is invisible
  // wherever the other is already installed.
  assert.match(staged, /hangPassiveAccountsRead: true/)
  assert.match(staged, /accountsUsageRefreshUnsupported: true/)
  assert.match(staged, /window\.bossanovaE2e = \{ \.\.\.window\.bossanovaE2e, \.\.\.staged \}/)
  assert.match(
    staged,
    /window\.__BOSSANOVA_E2E__ = \{ \.\.\.window\.__BOSSANOVA_E2E__, \.\.\.staged \}/,
  )

  // A still recipe has no `steps`, so the one interaction the subject requires
  // lives in the readiness gate. The order is the whole gate: wait for the
  // spinner (the page must still be loading, or the probe failure is not the
  // cold-start case), THEN click Refresh, THEN wait for the compat notice the
  // probe mints. `precedes()` rather than a raw `indexOf < indexOf` because a
  // deleted marker makes `indexOf` return -1 and `-1 < n` passes on precisely
  // the spec this forbids.
  assert.ok(precedes(staged, 'page.goto(', "getByText('Loading accounts')"))
  assert.ok(precedes(staged, "getByText('Loading accounts')", 'Refresh account usage'))
  assert.ok(precedes(staged, 'Refresh account usage', 'update bosso and try again'))
  // The trailing wait is what makes an unstaged run fail loudly instead of
  // photographing a healthy accounts table.
  assert.ok(precedes(staged, 'update bosso and try again', 'page.screenshot('))

  // ...and these two are what make it a REGRESSION gate rather than a
  // screenshot. The compat-notice wait above cannot tell the fixed page from
  // the BOS-1089 one: the regressed cold-start branch titles its full-page
  // danger panel with the folded error, which for an unsuperseded probe
  // failure is that same string, and `getByText` matches on substring. Nor can
  // the spinner wait at the top, which is sequenced before the click. So the
  // spinner must be re-checked AFTER the notice paints, and the notice must
  // have arrived through the INLINE route. The live region is what carries
  // that second claim: BOS-1090 gives the give-up notice `role="alert"` on
  // every page, so an alert census no longer separates the inline notice from
  // the danger panel, whereas `[data-testid="connection-status"]` is rendered
  // only by ConnectionNotice — the subtree the panel replaces. Asserted over
  // the region between the notice and the capture, because both markers also
  // appear earlier in the spec and a first-occurrence `precedes` would be
  // satisfied by those. `region()` throws on an absent marker, so deleting
  // either assertion fails loudly rather than going green.
  const beforeCapture = region(staged, 'update bosso and try again', 'page.screenshot(')
  assert.match(beforeCapture, /getByText\('Loading accounts'\)/)
  assert.match(beforeCapture, /\.settings-content \[data-testid="connection-status"\]/)
  assert.match(beforeCapture, /toHaveCount\(1\)/)

  // The live control: both the staging and the gate are recipe-scoped. Leaked
  // into the shared web fixture, the hang would strand every other accounts
  // recipe on its spinner.
  const control = specFor('web-accounts-list')
  assert.doesNotMatch(control, /hangPassiveAccountsRead/)
  assert.doesNotMatch(control, /accountsUsageRefreshUnsupported/)
  assert.doesNotMatch(control, /update bosso and try again/)
})

test('the shipped catalog keeps the cold-start probe-failure recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-accounts-cold-start-probe-failure')
  assert.ok(recipe, 'web-accounts-cold-start-probe-failure recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // Same hazard as the recipes around it: absent from the pathRule it is never
  // selected for a services/web diff, so BOS-1089's required proof would
  // silently never run.
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule.recipeIds.includes('web-accounts-cold-start-probe-failure'))
  // The id accountsColdStartStageScript and the captureReadyScript clause both
  // gate on is the id the catalog ships; a drift between them captures a
  // healthy accounts table instead of the probe failure.
  assert.equal(recipe.capture, 'still')
  assert.equal(recipe.route, '/settings/accounts')
  // NOT the accounts-section testid the sibling accounts recipes crop to: that
  // node lives inside AccountsView, which the page does not render while the
  // first read is still in flight. Cropping to it here would fail the
  // `toBeVisible()` guard on the very state this recipe exists to capture.
  assert.equal(recipe.cropToSelector, '.settings-content')
})

test('the accounts give-up recipe stages a failing daemon read into BOTH e2e globals', () => {
  const specFor = (id) =>
    buildSpec({
      recipe: { id, surface: 'web', capture: 'still', route: '/' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })

  const staged = specFor('web-accounts-give-up-retry')
  // `listDaemons` and not one of the per-daemon account reads:
  // fetchAccountsSnapshot folds those through Promise.allSettled, so failing
  // them yields an empty TABLE rather than the notice this recipe is proof of.
  assert.match(staged, /Object\.defineProperty\(errors, 'listDaemons'/)
  // Code 14 is the honest wire code for a dropped daemon connection, and it is
  // what the notice's message is derived from. It is deliberately NOT load-
  // bearing for retry classification: `fetchAccountsSnapshot` re-throws
  // `errorMessage(err)` as a plain string, so the code is gone before
  // `isTransient` sees it and the failure is terminal whatever is staged here.
  // That is why the recipe films the notice persisting rather than a ladder --
  // see the comment on accountsGiveUpStageScript.
  assert.match(staged, /code: 14/)
  // BOTH halves of the arming condition, but NOT because they are symmetric --
  // see the comment on accountsGiveUpStageScript. The elapsed half is the
  // load-bearing one: `firstReadAt` is seeded inside the getter, so read #1 is
  // answered whatever the boot cost, and the elapsed check is then the only
  // thing standing between StrictMode's second mount read and a capture that
  // opens on an already-given-up page. `reads < 2` is a clock-independent
  // floor kept alongside it. Both are pinned because dropping either leaves a
  // spec that still builds and still runs.
  assert.match(staged, /reads < 2/)
  assert.match(staged, /Date\.now\(\) - firstReadAt < ARM_AFTER_MS/)
  // The seeding ORDER is the reason the halves are asymmetric, so pin it: move
  // this line above the increment or out of the getter and read #1 can start
  // failing, which is the cold-start substitution the capture gate exists for.
  assert.match(
    staged,
    /get\(\) \{\n\s+reads \+= 1;\n\s+if \(firstReadAt === 0\) firstReadAt = Date\.now\(\);/,
  )

  // Same latent-mirror pin as the session-expired recipe above.
  assert.match(staged, /window\.bossanovaE2e = \{ \.\.\.window\.bossanovaE2e, \.\.\.staged \}/)
  assert.match(
    staged,
    /window\.__BOSSANOVA_E2E__ = \{ \.\.\.window\.__BOSSANOVA_E2E__, \.\.\.staged \}/,
  )

  // The live control. A daemon read staged into the shared web fixture would
  // break every other web recipe's subject while this test stayed green.
  assert.doesNotMatch(specFor('web-accounts-list'), /defineProperty\(errors/)
})

test("the accounts give-up arming delay stays under the recipe's own dwell", () => {
  // A cross-file coupling with nowhere to live on the JSON side: the recipe
  // schema is `unevaluatedProperties: false` (proof/recipes/schema.json), so
  // the catalog cannot carry a note beside the number this constrains. Pin it
  // here instead, because this is what a maintainer trimming the dwell to keep
  // the video short will actually hit.
  //
  // accountsGiveUpStageScript answers every daemon read until ARM_AFTER_MS has
  // passed since the page's FIRST read. The recipe's selector-less waits before
  // the refresh click are what buy that time, so a dwell shorter than the
  // arming delay leaves the click's read answered: no notice is raised, and the
  // step waiting for one times out against a perfectly healthy page.
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-accounts-give-up-retry')
  assert.ok(recipe, 'web-accounts-give-up-retry recipe is missing from the catalog')

  const clickIndex = recipe.steps.findIndex(
    (step) => step.action === 'click' && String(step.selector).includes('Refresh account usage'),
  )
  assert.ok(clickIndex > 0, 'the give-up recipe no longer clicks the usage refresh control')
  // Only the dwells BEFORE the click count: the arming has to be satisfied by
  // the time that read happens. `|| 500` mirrors renderVideoStep's own default
  // for a timeout-less wait.
  const dwellMs = recipe.steps
    .slice(0, clickIndex)
    .filter((step) => step.action === 'wait' && step.selector === undefined)
    .reduce((total, step) => total + (Number(step.timeoutMs) || 500), 0)
  assert.ok(dwellMs > 0, 'the give-up recipe no longer dwells before the refresh click')

  const staged = buildSpec({
    recipe,
    outputDir: '/tmp/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })
  const armed = staged.match(/const ARM_AFTER_MS = (\d+);/)
  assert.ok(armed, 'accountsGiveUpStageScript no longer declares ARM_AFTER_MS')
  assert.ok(
    Number(armed[1]) < dwellMs,
    `ARM_AFTER_MS (${armed[1]}ms) must stay under the recipe's pre-click dwell (${dwellMs}ms), ` +
      'or the refresh click reads a daemon that is still answering and no give-up notice is raised',
  )
})

test('the accounts give-up video waits for the loaded table before it starts stepping', () => {
  // Built from the SHIPPED catalog for the same reason the session-expired gate
  // test is: which branch emits the capture depends on the recipe's declared
  // shape, and this test's subject is where the gate lands relative to it.
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const specFor = (id) => {
    const recipe = catalog.recipes.find((r) => r.id === id)
    assert.ok(recipe, `${id} recipe is missing from the catalog`)
    return buildSpec({
      recipe,
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })
  }

  const staged = specFor('web-accounts-give-up-retry')
  // The gate names a fixture ROW, which is the one thing the failure mode it
  // guards cannot produce: if the staging ever let the page's first read fail,
  // the route renders the cold-start EmptyState -- which also carries a
  // `[role="alert"]` and a "Try again", so every later step would still find
  // something to click and the video would be green proof of the wrong
  // component.
  assert.match(staged, /await expect\(page\.getByText\('work@anthropic\.com'\)/)
  // After the navigation and before anything is captured or clicked, or it
  // gates nothing. `precedes()` rather than raw indexOf comparison, for the
  // BOS-737 reason documented on the session-expired gate above: a deleted
  // marker makes indexOf return -1, and -1 < n is true.
  //
  // The markers below carry the `getByText(` call and not the bare address:
  // webStageScript seeds that same label into the accounts fixture ABOVE the
  // steps, so a bare-string search would match the staging block and order the
  // assertion against the wrong occurrence.
  const gate = "page.getByText('work@anthropic.com')"
  assert.ok(precedes(staged, 'page.goto(', gate))
  assert.ok(precedes(staged, gate, 'Refresh account usage'))

  // The live control: the wait is recipe-scoped. Applied to the sibling refresh
  // recipe it would still pass here while gating a capture that never needed it.
  assert.doesNotMatch(specFor('web-accounts-refresh-flow'), /getByText\('work@anthropic\.com'\)/)
})

test('the shipped catalog keeps the accounts give-up recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-accounts-give-up-retry')
  assert.ok(recipe, 'web-accounts-give-up-retry recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // Absent from the pathRule it is never selected for a services/web diff, so
  // BOS-1090's required proof would silently never run.
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule.recipeIds.includes('web-accounts-give-up-retry'))
  // The id the stage script gates on is the id the catalog ships, and the
  // capture mode the recipe's evidence depends on: a still cannot show a
  // control surviving the press that used to remove it.
  assert.equal(recipe.capture, 'video')
  // The control recipe named in the plan's required proof, re-run unmodified.
  const control = catalog.recipes.find((r) => r.id === 'web-chat-terminal-reconnecting')
  assert.ok(control, 'web-chat-terminal-reconnecting control recipe is missing')
  assert.ok(webRule.recipeIds.includes('web-chat-terminal-reconnecting'))

  // This recipe must never wait for the reconnecting pill. `accountsGiveUpStageScript`
  // rejects `listDaemons`, but `fetchAccountsSnapshot` rethrows `errorMessage(err)` as a
  // plain STRING, so `ConnectError.from(aString)` is `Code.Unknown` with a non-TypeError
  // cause and `isTransient` classifies it terminal -- `dispatch({type:'reconnecting'})`
  // lives only inside `scheduleRetry`, which a terminal failure never reaches. A step
  // waiting on that pill therefore hangs until the runner's own timeout, and because the
  // recipe sits in the `services/web/` pathRule asserted above it would fail EVERY future
  // web diff, not just the one that added it. That is the shape this assertion pins: the
  // wait is unsatisfiable-by-construction, so it cannot be caught by running the recipe
  // once on a good day. `Reconnecting` evidence belongs to the sibling
  // web-chat-terminal-reconnecting control, whose route does arm the ladder.
  const giveUpSteps = JSON.stringify(recipe.steps)
  assert.doesNotMatch(giveUpSteps, /connection-status/)
  assert.doesNotMatch(giveUpSteps, /Reconnecting/)
})

test('the shipped catalog keeps both organization members recipes wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  for (const id of ['web-organization-members', 'web-organization-members-mobile']) {
    const recipe = catalog.recipes.find((r) => r.id === id)
    assert.ok(recipe, `${id} recipe is missing from the catalog`)
    assert.doesNotThrow(() => validateRecipe(recipe))
    // Same hazard as the two chat-terminal recipes above, and worse here:
    // captureStill swallows a failed crop and falls back to a full-frame
    // screenshot, so a recipe that drops out of the pathRule -- or whose id
    // drifts from the one organizationStageScript seeds -- degrades to a
    // wrong-state capture instead of an error.
    assert.ok(webRule.recipeIds.includes(id), `${id} is not wired to the web path rule`)
  }
})

test('buildSpec waits for the rendered glyph row before capturing the chat-terminal still', () => {
  // The capture selector ([data-testid='chat-terminal-canvas']) goes visible as
  // soon as xterm mounts, which is before the staged socket's data frame is
  // painted — so toBeVisible() alone can screenshot an empty pane. The gate
  // must come from the rendered rows, and only for the recipe that stages the
  // glyph line.
  const specFor = (id, stageEnv) =>
    buildSpec({
      recipe: { id, surface: 'web', route: '/', selector: "[data-testid='chat-terminal-canvas']" },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv,
    })

  const staged = specFor('web-chat-terminal', { VITE_E2E: '1' })
  assert.ok(staged.includes(String.raw`page.locator('.xterm-rows').first()`))
  // Tolerant spacing: xterm pads a row with cell spaces between style runs.
  assert.ok(staged.includes(String.raw`toContainText(new RegExp("✓\\s*↳\\s*─")`))
  // The wait must precede the screenshot, or it gates nothing.
  assert.ok(precedes(staged, '.xterm-rows', '.screenshot('))

  // No staging → no glyph line → the wait would hang forever.
  assert.doesNotMatch(specFor('web-chat-terminal', undefined), /\.xterm-rows/)
  assert.doesNotMatch(specFor('web-sessions', { VITE_E2E: '1' }), /\.xterm-rows/)
  assert.doesNotMatch(specFor('web-chat-terminal-reconnecting', { VITE_E2E: '1' }), /\.xterm-rows/)
})

test('buildSpec video waits for the eligibility verdict before capturing a subscribe still', () => {
  // The readiness gate was wired into the stills branch only, so a video
  // capture of /subscribe could record and still-capture the fail-closed
  // pre-verdict frame the gate exists to prevent. Video must gate the same way.
  const specFor = (id, stageEnv) =>
    buildSpec({
      recipe: {
        id,
        surface: 'web',
        capture: 'video',
        steps: [{ action: 'goto', route: '/subscribe', label: 'open subscribe' }],
      },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv,
    })

  const staged = specFor('web-subscribe-trial-used', { VITE_E2E: '1' })
  assert.ok(staged.includes(String.raw`page.locator('.subscribe-cta')).toBeEnabled(`))
  // After the navigation and before the still, or it gates nothing.
  assert.ok(precedes(staged, 'page.goto(', '.subscribe-cta'))
  assert.ok(precedes(staged, '.subscribe-cta', 'await captureStill(page'))

  // Unstaged there is no fixture verdict to wait for, and a non-subscribe
  // recipe has no CTA at all — the wait would hang in both.
  assert.doesNotMatch(specFor('web-subscribe-trial-used', undefined), /subscribe-cta/)
  assert.doesNotMatch(specFor('web-sessions', { VITE_E2E: '1' }), /subscribe-cta/)
})

test('buildSpec stages a checkout-CTA state so the subscribe eligibility captures can differ', () => {
  // Regression: the ineligible opt-in used to stage cloudTrialEligibility alone.
  // The shared fixture leaves cloudAccessState unset -> the fake resolves ACTIVE
  // -> ACTIVE shows no checkout CTA -> the fake answers UNSPECIFIED and never
  // consults the eligibility that was staged. Both subscribe recipes then
  // rendered the same non-trial copy, and the two captures came back as one
  // byte-identical PNG -- a "passed" proof run whose eligible capture showed the
  // ineligible page. The access state is what makes the eligibility reachable,
  // so both fields must be staged together.
  const specFor = (id, stageEnv) =>
    buildSpec({
      recipe: { id, surface: 'web', route: '/subscribe', selector: '.subscribe-actions' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv,
    })

  for (const id of [
    'web-subscribe',
    'web-subscribe-trial-used',
    'web-subscribe-no-notification-prompt',
  ]) {
    const staged = specFor(id, { VITE_E2E: '1' })
    assert.match(
      staged,
      /cloudAccessState: 'needs_subscription'/,
      `${id} does not stage a checkout-CTA state`,
    )
    // Staging has to happen before the page is opened, or the first render
    // reads the unstaged fixture.
    assert.ok(
      precedes(staged, 'cloudAccessState', 'page.goto('),
      `${id} stages after the navigation`,
    )
  }

  // The two eligibility verdicts are the whole subject of the pair; if they
  // ever agree, the captures are proof of nothing.
  assert.match(specFor('web-subscribe', { VITE_E2E: '1' }), /cloudTrialEligibility: 'eligible'/)
  assert.match(
    specFor('web-subscribe-trial-used', { VITE_E2E: '1' }),
    /cloudTrialEligibility: 'ineligible'/,
  )

  // Unstaged, and for a recipe that is not about the CTA, neither field is touched.
  assert.doesNotMatch(specFor('web-subscribe', undefined), /cloudAccessState/)
  assert.doesNotMatch(specFor('web-sessions', { VITE_E2E: '1' }), /cloudTrialEligibility/)
})

test('buildSpec stages an active account and waits for the redirect destination', () => {
  // BOS-1148. The redirect recipe is the mirror of the CTA recipes above, and
  // it fails silently in two directions if either half is dropped. Without the
  // 'active' staging the fake answers from an unstaged fixture and the capture
  // is just another view of the offer; without the readiness gate the still is
  // taken at `load`, before the status RPC answers, and photographs the
  // pre-verdict /subscribe render -- a green run proving the loop is still
  // there. The absent .subscribe-actions is the fix; .data-table-wrap is the
  // destination the user was routed to.
  const specFor = (id, stageEnv) =>
    buildSpec({
      recipe: { id, surface: 'web', route: '/subscribe', cropToSelector: '.data-table-wrap' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv,
    })

  const staged = specFor('web-subscribe-active-redirect', { VITE_E2E: '1' })
  assert.match(staged, /cloudAccessState: 'active'/)
  assert.ok(precedes(staged, 'cloudAccessState', 'page.goto('))
  assert.ok(staged.includes(String.raw`page.locator('.data-table-wrap')).toBeVisible(`))
  assert.ok(staged.includes(String.raw`page.locator('.subscribe-actions')).toHaveCount(0)`))
  // Anchored on the whole gate expression, not the bare class: the shared spec
  // preamble mentions .data-table-wrap in a comment long before the navigation.
  assert.ok(
    precedes(staged, 'page.goto(', String.raw`page.locator('.data-table-wrap')).toBeVisible(`),
  )
  assert.ok(precedes(staged, '.subscribe-actions', 'page.screenshot('))

  // It must NOT inherit the CTA gate: that waits for an enabled .subscribe-cta,
  // which an active account never renders, so the wait would hang and fail.
  assert.doesNotMatch(staged, /subscribe-cta/)
  // And it must not stage a trial verdict it has no use for.
  assert.doesNotMatch(staged, /cloudTrialEligibility/)

  // Unstaged there is no fixture to answer from, so nothing is staged or waited on.
  assert.doesNotMatch(specFor('web-subscribe-active-redirect', undefined), /cloudAccessState/)
})

test('validateRecipe requires a key for press steps and accepts a valid one', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'press', key: '?' }],
    }),
  )
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'press' }] }),
    /video press step requires key/,
  )
})

test('buildSpec video drives a native select with selectOption and reloads in place', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-filter-flow',
      surface: 'web',
      capture: 'video',
      steps: [
        { action: 'goto', route: '/' },
        { action: 'select', selector: '[data-testid="sessions-daemon-filter"]', value: 'daemon-b' },
        { action: 'reload' },
      ],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  // A click would only open the OS popup, which never paints into the video.
  assert.match(spec, /\.selectOption\('daemon-b'\)/)
  // reload, not goto: a fresh navigation would discard the localStorage the
  // persistence proof exists to demonstrate.
  assert.match(spec, /await page\.reload\(\)/)
})

test('validateRecipe requires selector and value for select steps, allowing an empty value', () => {
  const recipe = (step) => ({ id: 'v', surface: 'web', capture: 'video', steps: [step] })
  assert.doesNotThrow(() => validateRecipe(recipe({ action: 'select', selector: 's', value: 'x' })))
  // '' is the leading "All …" filter option, so clearing a filter is valid.
  assert.doesNotThrow(() => validateRecipe(recipe({ action: 'select', selector: 's', value: '' })))
  assert.throws(
    () => validateRecipe(recipe({ action: 'select', value: 'x' })),
    /video select step requires selector/,
  )
  assert.throws(
    () => validateRecipe(recipe({ action: 'select', selector: 's' })),
    /video select step requires value/,
  )
  assert.doesNotThrow(() => validateRecipe(recipe({ action: 'reload' })))
})

// The video-step contract is encoded twice — VIDEO_ACTIONS + the validateRecipe
// ladder here, and the action enum in proof/recipes/schema.json — and the two
// HAVE drifted: `press` was accepted by this runner while the schema enum
// omitted it, and the step schema is additionalProperties:false, so valid
// recipes were schema-invalid until someone noticed by hand. Pin the schema to
// the runner's own constant, the way proof-scenario.test.mjs pins the scenarios
// schema to STEP_OPS, so a one-sided addition fails here instead.
test('recipe schema step actions agree with the runner VIDEO_ACTIONS set', () => {
  const schema = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/schema.json', import.meta.url), 'utf8'),
  )
  const stepSchema = schema.$defs.browserRecipe.allOf[1].properties.steps.items
  assert.deepEqual(
    [...stepSchema.properties.action.enum].sort(),
    [...VIDEO_ACTIONS].sort(),
    'proof/recipes/schema.json step actions must match proof-playwright-runner.mjs VIDEO_ACTIONS',
  )
})

test('buildSpec video: emits test.use slowMo with default 350 when recipe has no slowMo', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /test\.use\(\{ launchOptions:/)
  assert.match(spec, /slowMo: 350/)
})

test('buildSpec video: bakes recipe.slowMo as numeric literal when provided', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      slowMo: 700,
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /slowMo: 700/)
  assert.doesNotMatch(spec, /slowMo: 350/)
})

test('buildSpec video branch preserves playwright baseURL for relative goto steps', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })

  assert.match(spec, /async \(\{ browser, baseURL \}\)/)
  assert.match(spec, /baseURL,/)
  assert.doesNotMatch(spec, /chromium\.launch/)
})

test('validateRecipe accepts a video recipe and rejects an unknown action', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    }),
  )
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'nope' }] }),
    /unsupported video step action/,
  )
})

test('validateRecipe rejects incomplete video steps before playwright starts', () => {
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'goto' }] }),
    /video goto step requires route/,
  )
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'click' }] }),
    /video click step requires selector/,
  )
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        surface: 'web',
        capture: 'video',
        steps: [{ action: 'type', selector: 'input' }],
      }),
    /video type step requires value/,
  )
})

test('rejects recipe ids that are unsafe as screenshot filenames', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-bad-id-'))
  const recipePath = path.join(dir, 'recipe.json')
  const outputDir = path.join(dir, 'out')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'bad/id',
      route: '/',
      selector: 'main',
    }),
  )

  const result = runRunner(['--surface', 'web', '--recipe', recipePath, '--output-dir', outputDir])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /invalid recipe id/)
  assert.equal(fs.existsSync(path.join(outputDir, 'bad')), false)
})

test('rejects malformed browser proof surface ids before playwright starts', () => {
  // BOS-202: surfaces are no longer a closed allowlist (a consumer declares its
  // own), but the id must still be a safe slug — a malformed id is rejected.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-bad-surface-'))
  const recipePath = path.join(dir, 'recipe.json')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: '/',
      selector: 'main',
    }),
  )

  const result = runRunner([
    '--surface',
    'Bad_Surface',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /invalid --surface/)
})

test('rejects external browser proof routes before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-external-route-'))
  const recipePath = path.join(dir, 'recipe.json')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: 'http://localhost:3000/admin',
      selector: 'main',
    }),
  )

  const result = runRunner([
    '--surface',
    'marketing',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /proof browser route must be relative/)
})

test('rejects protocol-relative browser proof routes before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-protocol-route-'))
  const recipePath = path.join(dir, 'recipe.json')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: '//example.com/admin',
      selector: 'main',
    }),
  )

  const result = runRunner([
    '--surface',
    'marketing',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /proof browser route must be relative/)
})

test('rejects backslash browser proof routes before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-backslash-route-'))
  const recipePath = path.join(dir, 'recipe.json')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: '/\\example.com/admin',
      selector: 'main',
    }),
  )

  const result = runRunner([
    '--surface',
    'marketing',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /proof browser route must be relative/)
})

// ── slugify ───────────────────────────────────────────────────────────────────

test('slugify: basic cases', () => {
  assert.equal(slugify('Open home'), 'open-home')
  assert.equal(slugify('  A/B  '), 'a-b')
  assert.equal(slugify(''), 'step')
  assert.equal(slugify('goto'), 'goto')
})

// ── buildSpec video: per-step stills + video-meta.json ───────────────────────

const labelledVideoRecipe = {
  id: 'web-new-session-flow',
  surface: 'web',
  capture: 'video',
  cropToSelector: '.data-table-wrap',
  viewport: { width: 1440, height: 1000 },
  steps: [
    { action: 'goto', route: '/', label: 'Open home' },
    { action: 'wait', selector: '.data-table-wrap', label: 'Sessions list' },
    { action: 'click', selector: '.data-table-wrap .row-clickable', label: 'Open session' },
    { action: 'wait', timeoutMs: 800, label: 'Session view' },
  ],
}

test('buildSpec video: emits captureStill call after each step', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  // Should have exactly 4 captureStill calls
  const matches = spec.match(/captureStill\(/g) ?? []
  assert.equal(matches.length, 4 + 1) // 4 calls + 1 helper definition
})

test('buildSpec video: still filenames use 2-digit NN and slug', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /01-open-home\.png/)
  assert.match(spec, /02-sessions-list\.png/)
  assert.match(spec, /03-open-session\.png/)
  assert.match(spec, /04-session-view\.png/)
})

test('buildSpec video: emits __stills.push with fileName and label', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /__stills\.push\(\{ fileName: '01-open-home\.png', label: 'Open home' \}\)/)
  assert.match(
    spec,
    /__stills\.push\(\{ fileName: '02-sessions-list\.png', label: 'Sessions list' \}\)/,
  )
  assert.match(
    spec,
    /__stills\.push\(\{ fileName: '03-open-session\.png', label: 'Open session' \}\)/,
  )
  assert.match(
    spec,
    /__stills\.push\(\{ fileName: '04-session-view\.png', label: 'Session view' \}\)/,
  )
})

test('buildSpec video: writes video-meta.json with cropHeight and stills', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /video-meta\.json/)
  assert.match(spec, /cropHeight/)
  assert.match(spec, /__stills/)
})

test('buildSpec video: preserves the tallest measured crop height across stills', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /__cropHeight = Math\.max\(__cropHeight \?\? 0, __h\)/)
  assert.doesNotMatch(spec, /__cropHeight = __h;/)
})

test('buildSpec video: disables crop metadata after any full-frame still fallback', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /let __disableCrop = false;/)
  assert.match(spec, /if \(__h === null\) __disableCrop = true;/)
  assert.match(spec, /cropHeight: __disableCrop \? null : __cropHeight/)
})

test('buildSpec video: uses recipe cropToSelector when present', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /\.data-table-wrap/)
})

test('buildSpec video: uses #root default for web surface without cropToSelector', () => {
  const recipe = {
    id: 'web-flow',
    surface: 'web',
    capture: 'video',
    viewport: { width: 1440, height: 1000 },
    steps: [{ action: 'goto', route: '/', label: 'Open home' }],
  }
  const spec = buildSpec({ recipe, outputDir: '/tmp/out', surface: 'web' })
  assert.match(spec, /'#root'/)
})

test('buildSpec video: uses main default for marketing surface without cropToSelector', () => {
  const recipe = {
    id: 'mkt-flow',
    surface: 'marketing',
    capture: 'video',
    viewport: { width: 1440, height: 1000 },
    steps: [{ action: 'goto', route: '/', label: 'Open home' }],
  }
  const spec = buildSpec({ recipe, outputDir: '/tmp/out', surface: 'marketing' })
  assert.match(spec, /'main'/)
})

// ── Behavior 2: overlay harness ───────────────────────────────────────────────

test('buildSpec video: injects overlay harness with addInitScript, __proofOverlay, pointer-events:none, and z-index', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /addInitScript/)
  assert.match(spec, /__proofOverlay/)
  assert.match(spec, /pointer-events:none/)
  assert.match(spec, /2147483647/)
})

test('buildSpec video: emits caption call for step with caption field', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'goto', route: '/', caption: 'Hi' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /window\.__proofOverlay\?\.caption\(/)
  assert.match(spec, /'Hi'/)
})

test('buildSpec video: goto step emits its caption AFTER the navigation', () => {
  // A caption set before goto would be destroyed by the navigation, so for goto
  // steps the caption must be emitted after page.goto() completes.
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'goto', route: '/', caption: 'Open home' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  const gotoIdx = spec.indexOf('page.goto(')
  const captionIdx = spec.indexOf('window.__proofOverlay?.caption(')
  assert.ok(gotoIdx >= 0 && captionIdx >= 0, 'spec should contain goto and caption')
  assert.ok(captionIdx > gotoIdx, 'goto caption must be emitted after page.goto()')
})

test('buildSpec video: scrolls click target into view before measuring ripple position', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'click', selector: 'button' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /window\.__proofOverlay\?\.ripple\(/)
  assert.match(spec, /boundingBox\(\)/)
  const actionBlock = region(spec, "const __loc = page.locator('button').first();")
  // BOS-737: `precedes()` rather than the raw `indexOf < indexOf`. Raw, deleting the
  // `scrollIntoViewIfNeeded()` call — the exact regression this pins — makes `indexOf` return -1
  // and `-1 < n` true, so the assertion goes green on precisely the spec it exists to forbid.
  assert.ok(
    precedes(actionBlock, 'scrollIntoViewIfNeeded()', 'boundingBox()'),
    'click ripple must measure after target is in the viewport',
  )
})

test('buildSpec video: scrolls type target into view before measuring ripple position', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'type', selector: 'input', value: 'hello' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /window\.__proofOverlay\?\.ripple\(/)
  assert.match(spec, /boundingBox\(\)/)
  const actionBlock = region(spec, "const __loc = page.locator('input').first();")
  assert.ok(
    precedes(actionBlock, 'scrollIntoViewIfNeeded()', 'boundingBox()'),
    'type ripple must measure after target is in the viewport',
  )
})

test('buildSpec video: step without caption does NOT emit caption call', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.doesNotMatch(spec, /window\.__proofOverlay\?\.caption\(/)
})

// ── validateRecipe: label field ───────────────────────────────────────────────

test('validateRecipe: video step with label: "" throws', () => {
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        surface: 'web',
        capture: 'video',
        steps: [{ action: 'goto', route: '/', label: '' }],
      }),
    /video step label must be a non-empty string/,
  )
})

test('validateRecipe: video step with label: "X" passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/', label: 'X' }],
    }),
  )
})

test('validateRecipe: video step without label passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    }),
  )
})

// ── Behavior 3: scroll step ───────────────────────────────────────────────────

test('validateRecipe: scroll step with toSelector passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      capture: 'video',
      steps: [{ action: 'scroll', toSelector: 'main' }],
    }),
  )
})

test('validateRecipe: scroll step with byPx passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      capture: 'video',
      steps: [{ action: 'scroll', byPx: 600 }],
    }),
  )
})

test('validateRecipe: scroll step with neither toSelector nor byPx throws', () => {
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        capture: 'video',
        steps: [{ action: 'scroll' }],
      }),
    /scroll step requires toSelector, byPx, or fullPage/,
  )
})

test('validateRecipe: scroll step with byPx as non-finite string throws', () => {
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        capture: 'video',
        steps: [{ action: 'scroll', byPx: 'bad' }],
      }),
    /scroll step byPx must be a finite number/,
  )
})

test('buildSpec video: scroll with toSelector emits scrollIntoView with smooth behavior', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'scroll', toSelector: 'main' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /scrollIntoView/)
  assert.match(spec, /behavior: 'smooth'/)
  assert.match(spec, /waitForTimeout\(800\)/)
})

test('buildSpec video: scroll with byPx emits scrollBy with smooth behavior', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      capture: 'video',
      steps: [{ action: 'scroll', byPx: 600 }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /window\.scrollBy/)
  assert.match(spec, /behavior: 'smooth'/)
  assert.match(spec, /waitForTimeout\(800\)/)
})

// ── fullPage scroll step ──────────────────────────────────────────────────────

const videoRecipe = (steps) => ({
  id: 'web-x',
  surface: 'web',
  capture: 'video',
  viewport: { width: 1440, height: 1000 },
  steps,
})

test('validateRecipe accepts a fullPage scroll step', () => {
  assert.doesNotThrow(() =>
    validateRecipe(
      videoRecipe([
        { action: 'goto', route: '/' },
        { action: 'scroll', fullPage: true },
      ]),
    ),
  )
})

test('validateRecipe still rejects a scroll step with no target', () => {
  assert.throws(
    () => validateRecipe(videoRecipe([{ action: 'goto', route: '/' }, { action: 'scroll' }])),
    /scroll step requires toSelector, byPx, or fullPage/,
  )
})

test('buildSpec emits a whole-page scroll loop for a fullPage scroll step', () => {
  const spec = buildSpec({
    recipe: videoRecipe([
      { action: 'goto', route: '/' },
      { action: 'scroll', fullPage: true },
    ]),
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.match(spec, /scrollHeight/)
  assert.match(spec, /window\.scrollTo/)
})

test('OVERLAY_CAPTION_CSS re-export is import-equal to the shared caption-spec module (BOS-140)', () => {
  assert.equal(OVERLAY_CAPTION_CSS, SPEC_OVERLAY_CAPTION_CSS)
  const spec = buildSpec({
    recipe: videoRecipe([{ action: 'goto', route: '/' }]),
    outputDir: '/tmp/out',
    surface: 'web',
  })
  assert.ok(spec.includes(SPEC_OVERLAY_CAPTION_CSS))
})

test('overlay caption is anchored to the top of the viewport', () => {
  assert.match(OVERLAY_CAPTION_CSS, /top:\s*24px/)
  assert.doesNotMatch(OVERLAY_CAPTION_CSS, /bottom:\s*\d/)
})

test('overlay caption reserves horizontal space for the burned-in timer', () => {
  // Wide viewports reserve the timer corner via calc; narrow (390px mobile)
  // viewports fall back to the 60% floor so the caption stays readable.
  assert.match(OVERLAY_CAPTION_CSS, /max-width:\s*max\(60%,\s*calc\(100% - 380px\)\)/)
  assert.doesNotMatch(OVERLAY_CAPTION_CSS, /max-width:\s*80%/)
})

function runRunner(args) {
  return spawnSync(process.execPath, [runnerPath, ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    timeout: 5000,
  })
}

// Same, but with an empty PATH so the `pnpm exec playwright` spawn fails
// immediately with ENOENT. That turns a real capture into a fast assertion on
// everything the runner does BEFORE the browser: parse, validate, and build the
// spec -- including every staging payload.
function runRunnerWithoutPlaywright(args) {
  return spawnSync(process.execPath, [runnerPath, ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    timeout: 5000,
    env: { ...process.env, PATH: '' },
  })
}

// ── Surface-agnostic runner: spec-root, crop, stage env via CLI args (BOS-202) ──

test('parseArgs accepts an arbitrary surface when spec-root is supplied', () => {
  const parsed = parseArgs([
    '--surface',
    'portal',
    '--recipe',
    'r.json',
    '--output-dir',
    'o',
    '--spec-root',
    'e2e',
    '--default-crop',
    'main',
  ])
  assert.equal(parsed.surface, 'portal')
  assert.equal(parsed['spec-root'], 'e2e')
  assert.equal(parsed['default-crop'], 'main')
})

test('parseArgs still rejects a malformed surface id', () => {
  assert.throws(
    () => parseArgs(['--surface', 'Bad Surface', '--recipe', 'r.json', '--output-dir', 'o']),
    /invalid --surface/,
  )
})

test('stageEnvForArgs preserves legacy direct web staging without --stage-env', () => {
  assert.deepEqual(stageEnvForArgs({ surface: 'web' }), { VITE_E2E: '1' })
  assert.equal(stageEnvForArgs({ surface: 'marketing' }), undefined)
  assert.deepEqual(stageEnvForArgs({ surface: 'web', 'stage-env': '{"CUSTOM":"1"}' }), {
    CUSTOM: '1',
  })
})

test('buildSpec video honors an explicit defaultCrop over the surface heuristic', () => {
  const spec = buildSpec({
    recipe: {
      id: 'v',
      surface: 'docs',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
      viewport: { width: 1280, height: 1000 },
    },
    outputDir: '/out',
    surface: 'docs',
    defaultCrop: '.docs-main',
  })
  assert.ok(spec.includes('.docs-main'))
})

test('buildSpec video falls back to the surface crop heuristic when no defaultCrop', () => {
  const spec = buildSpec({
    recipe: {
      id: 'v',
      surface: 'marketing',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
      viewport: { width: 1280, height: 1000 },
    },
    outputDir: '/out',
    surface: 'marketing',
  })
  assert.ok(spec.includes("'main'"))
})

test('buildSpec stages the web fixture only when a stageEnv is supplied', () => {
  const base = {
    id: 's',
    surface: 'web',
    route: '/',
    viewport: { width: 1280, height: 1000 },
  }
  const staged = buildSpec({
    recipe: base,
    outputDir: '/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })
  assert.ok(staged.includes('window.bossanovaE2e'))
  const unstaged = buildSpec({ recipe: base, outputDir: '/out', surface: 'web' })
  assert.ok(!unstaged.includes('window.bossanovaE2e'))
})

// BOS-1065, extended by BOS-1067/BOS-1072 and generalised by BOS-1073: some web
// captures need a signed-in organization. Organization-subject settings
// captures are staged by route because OrgScopedSettings reconciles the URL
// against the WorkOS claim and renders "Switching to ..." until it agrees. Route
// detection prevents a new scoped recipe from silently photographing the
// spinner.
//
// Every OTHER web recipe must keep the unset organizationId, so both halves are
// pinned: what enters a scoped route is seeded, and the staging does not leak
// to recipes that need no organization.
// A direct CLI run reaches the staging payloads while every `const` declared
// below the `if (invokedDirectly) run(...)` block is still in its temporal dead
// zone, so a module-level const used by staging fails the capture with
// "Cannot access '<name>' before initialization" -- a message that names no
// recipe and does not reproduce under the in-process tests above, which import
// the module and therefore always finish evaluating it first. Only a spawned
// run can see it.
test('a direct CLI run stages an organization-scoped recipe without a dead-zone error', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-org-tdz-'))
  const recipePath = path.join(dir, 'recipe.json')
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'web-org-tdz-probe',
      // The runner's own recipe.json carries the surface (proof.mjs writes it),
      // and the organization staging gate reads it -- without it this probe
      // would take the not-web early return and stage nothing at all.
      surface: 'web',
      route: '/org-e2e/settings/organization',
      selector: 'main',
    }),
  )

  const result = runRunnerWithoutPlaywright([
    '--surface',
    'web',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ])

  assert.doesNotMatch(result.stderr, /before initialization/)
  // Reaching the (unavailable) playwright spawn is what proves the spec was
  // built: the runner writes it and only then launches the browser.
  assert.match(result.stderr, /ENOENT/)
})

test('buildSpec seeds an organizationId for any organization-scoped route', () => {
  // Shape-based, not fixture-based: a recipe that coins its own organization id
  // is behind the same guard and needs the same staging.
  for (const route of [
    '/org-e2e/settings/appearance',
    '/whatever-org/settings',
    '/o/settings/cron',
  ]) {
    const staged = buildSpec({
      recipe: {
        id: 'web-unlisted-recipe',
        surface: 'web',
        route,
        viewport: { width: 1024, height: 768 },
      },
      outputDir: '/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })
    assert.ok(staged.includes("organizationId: 'workos-e2e'"), `${route} should be seeded`)
  }

  // A video recipe names its routes on the steps, not on the recipe.
  const stepped = buildSpec({
    recipe: {
      id: 'web-unlisted-video',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/org-e2e/settings/organization' }],
      viewport: { width: 1024, height: 768 },
    },
    outputDir: '/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })
  assert.ok(stepped.includes("organizationId: 'workos-e2e'"))
})

test('buildSpec leaves every other recipe without an organization', () => {
  const other = buildSpec({
    recipe: {
      id: 'web-header-menu',
      surface: 'web',
      route: '/',
      viewport: { width: 1440, height: 1000 },
    },
    outputDir: '/out',
    surface: 'web',
    stageEnv: { VITE_E2E: '1' },
  })
  assert.ok(other.includes('window.bossanovaE2e'))
  assert.ok(!other.includes('organizationId'))

  // The route shape is not unique to the app: the docs site has its own
  // `/reference/settings` page and no organization to stage.
  const docs = buildSpec({
    recipe: {
      id: 'docs-settings',
      surface: 'docs',
      route: '/reference/settings',
      viewport: { width: 1280, height: 1000 },
    },
    outputDir: '/out',
    surface: 'docs',
    stageEnv: { VITE_E2E: '1' },
  })
  assert.ok(!docs.includes('organizationId'))
})

// ----- BOS-879 staged echo responder --------------------------------------
//
// The paste capture's final assertion is that the pasted text appears in the
// terminal canvas. xterm never echoes locally — Terminal.paste() emits through
// onData and only the far end's echo paints anything — so the fixture has to
// play the PTY's part. These pin that it echoes client input and nothing else,
// and that it stays scoped to the one recipe that needs it.

function runEchoResponder(frames) {
  const source = echoServerScript({ id: 'web-chat-terminal-paste' })
  assert.ok(source.includes('ws.onMessage('), 'echo snippet is empty')
  const sent = []
  let handler = null
  const ws = {
    send: (buf) => sent.push(Buffer.from(buf)),
    onMessage: (fn) => {
      handler = fn
    },
  }
  new Function('ws', 'Buffer', source)(ws, Buffer)
  assert.ok(handler, 'echo responder registered no message handler')
  for (const frame of frames) {
    handler(frame)
  }
  return sent
}

function dataFrame(text) {
  const payload = Buffer.from(text, 'utf-8')
  const header = Buffer.from([
    0,
    (payload.length >>> 16) & 0xff,
    (payload.length >>> 8) & 0xff,
    payload.length & 0xff,
  ])
  return Buffer.concat([header, payload])
}

test('staged echo responder echoes kind=0 client input back as a data frame', () => {
  const sent = runEchoResponder([dataFrame('hello from the clipboard')])
  assert.equal(sent.length, 1)
  assert.equal(sent[0][0], 0, 'echo must be a kind=0 data frame')
  assert.equal(sent[0].subarray(4).toString('utf8'), 'hello from the clipboard')
})

test('staged echo responder ignores non-data frames and runt frames', () => {
  // kind=1 is a resize; a 4-byte frame carries no payload at all. Echoing
  // either would put bytes on the canvas the paste never produced.
  const resize = Buffer.from([1, 0, 0, 4, 0, 80, 0, 24])
  const runt = Buffer.from([0, 0, 0, 0])
  assert.equal(runEchoResponder([resize, runt]).length, 0)
})

test('staged echo responder is scoped to the paste recipe', () => {
  assert.equal(echoServerScript({ id: 'web-chat-terminal-upload' }), '')
  assert.equal(echoServerScript({ id: 'web-chat-terminal' }), '')
  assert.equal(echoServerScript(undefined), '')
})

// --- BOS-790: audit text is scoped to what the saved frame actually shows ----

const AUDIT_RECIPE_VIEWPORT = { width: 1024, height: 768 }

function auditSpecFor(overrides) {
  return buildSpec({
    recipe: {
      id: 'web-audit-scope',
      surface: 'web',
      route: '/',
      viewport: AUDIT_RECIPE_VIEWPORT,
      ...overrides,
    },
    outputDir: '/tmp/out',
    surface: 'web',
  })
}

// buildSpec emits ONE spec carrying all three runtime branches, so a whole-spec
// match cannot tell them apart. Extract each branch body fail-closed (region()
// throws on a moved marker rather than silently yielding an empty region).
//
// What these three gates pin is therefore the EMITTED CODE of each branch, not
// a per-recipe outcome: the recipe passed to buildSpec only changes the baked-in
// `const selector` / `const cropToSelector` literals, so all three specs are
// structurally identical and the recipe in each case is scene-setting. The
// runtime branch a given recipe takes is decided inside Playwright, which these
// gates never run.
const CROP_BRANCH = ['  if (cropToSelector) {', '  } else if (selector) {']
const SELECTOR_BRANCH = [
  '  } else if (selector) {',
  "  } else {\n    auditText = await page.locator('body')",
]
const PLAIN_BRANCH = [
  "  } else {\n    auditText = await page.locator('body')",
  '  await test.info().attach',
]

test('buildSpec emits a cropToSelector branch harvesting audit text only up to the clip height', () => {
  const spec = auditSpecFor({ cropToSelector: '#root' })
  const branch = region(spec, ...CROP_BRANCH, 'cropToSelector branch')
  assert.match(
    branch,
    /evaluate\(collectProofAuditText, height\)/,
    'the crop branch must pass the computed clip height into the audit collector',
  )
  // The height must be computed BEFORE the harvest, or the cut-off is undefined
  // at call time and the scoping silently degrades to whole-body.
  assert.ok(
    precedes(branch, 'const height = Math.min(', 'evaluate(collectProofAuditText, height)'),
    'clip height must be computed before the harvest',
  )
})

test('buildSpec emits a selector branch keeping its element-scoped audit source, uncapped', () => {
  const spec = auditSpecFor({ selector: '[data-testid="row"]' })
  const branch = region(spec, ...SELECTOR_BRANCH, 'selector branch')
  assert.match(branch, /await target\.evaluate\(collectProofAuditText\);/)
  assert.doesNotMatch(
    branch,
    /collectProofAuditText,/,
    'the element screenshot is not clipped, so it must not take a cut-off',
  )
})

test('buildSpec emits a plain-viewport branch keeping its whole-body audit source, uncapped', () => {
  const spec = auditSpecFor({})
  const branch = region(spec, ...PLAIN_BRANCH, 'plain-viewport branch')
  assert.match(branch, /await page\.locator\('body'\)\.evaluate\(collectProofAuditText\);/)
  assert.doesNotMatch(
    branch,
    /collectProofAuditText,/,
    'a full-viewport capture genuinely shows the whole body',
  )
})

function loadAuditCollector() {
  const src = collectProofAuditTextScript()
  return new Function(`${src}\nreturn collectProofAuditText;`)()
}

// A real rect carries `bottom` as well as `top`, and the collector reads both:
// `top` decides whether the node is in frame at all, `bottom` whether it is
// ENTIRELY in frame and can therefore be taken as one contiguous string. A stub
// that supplied only `top` would never exercise that second branch, so default
// to a plausible 100px-tall box and let a caller widen it.
function stubElement(top, texts, { measurable = true, height = 100 } = {}) {
  const node = {
    nodeType: 1,
    textContent: texts.join('\n'),
    getAttribute: () => null,
    querySelectorAll: () => [],
    childNodes: [],
  }
  if (measurable) node.getBoundingClientRect = () => ({ top, bottom: top + height })
  node.childNodes = texts.map((text) => ({ nodeType: 3, textContent: text, parentElement: node }))
  return node
}

// The root is the whole document: always straddling, never taken whole.
function stubRoot(children) {
  return {
    nodeType: 1,
    textContent: children.map((child) => child.textContent).join('\n'),
    getBoundingClientRect: () => ({ top: 0, bottom: 100000 }),
    getAttribute: () => null,
    querySelectorAll: () => [],
    childNodes: children,
  }
}

test('collectProofAuditText drops nodes below the cut-off and keeps them without one', () => {
  const collect = loadAuditCollector()
  const root = stubRoot([
    stubElement(10, ['in-frame token']),
    stubElement(5000, ['below-fold token']),
  ])
  const clipped = collect(root, 800)
  assert.match(clipped, /in-frame token/)
  assert.doesNotMatch(clipped, /below-fold token/, 'text below the clip never appeared in the PNG')

  const whole = collect(root)
  assert.match(whole, /in-frame token/)
  assert.match(whole, /below-fold token/, 'with no cut-off the whole element is harvested')
})

test('collectProofAuditText fails OPEN for a node with no resolvable rect', () => {
  // Text nodes have no getBoundingClientRect; an element with no resolvable box
  // must still be harvested, because dropping unpositioned text would silently
  // shrink audit.txt and could red an existing recipe's evidence gate.
  const collect = loadAuditCollector()
  const root = stubRoot([
    stubElement(10, ['in-frame token']),
    stubElement(9999, ['unpositioned token'], { measurable: false }),
    stubElement(5000, ['below-fold token']),
  ])
  const clipped = collect(root, 800)
  assert.match(clipped, /in-frame token/)
  assert.match(clipped, /unpositioned token/, 'no resolvable rect must mean included, not dropped')
  assert.doesNotMatch(clipped, /below-fold token/)
})

test('collectProofAuditText does not leak below-clip text through the attribute pass', () => {
  // The trailing querySelectorAll pass visits every [aria-label]/[title]/[alt]
  // and form element. Such a container routinely STRADDLES the clip — its top
  // edge is in frame while its subtree runs past it (SessionDetail's
  // `<section aria-label="Rotation history">`, SettingsLayout's
  // `<nav aria-label="Settings">`). Pushing its textContent there would put the
  // below-clip half straight back into audit.txt and re-open the vacuous-gate
  // hole the cut-off exists to close. The label itself is still harvested.
  const collect = loadAuditCollector()
  // The container's own box really does span its children, so it straddles the
  // clip rather than sitting entirely above it — that is the whole point.
  const straddler = stubElement(10, ['in-frame token', 'below-fold token'], { height: 5090 })
  straddler.getAttribute = (name) => (name === 'aria-label' ? 'Rotation history' : null)
  // Only the second child sits below the clip; the container starts above it.
  straddler.childNodes = [
    stubElement(10, ['in-frame token']),
    stubElement(5000, ['below-fold token']),
  ]
  const root = stubRoot([straddler])
  root.querySelectorAll = () => [straddler]

  const clipped = collect(root, 800)
  assert.match(clipped, /in-frame token/)
  assert.match(clipped, /Rotation history/, 'the aria-label itself is in frame and must be kept')
  assert.doesNotMatch(
    clipped,
    /below-fold token/,
    'a straddling labelled container must not re-admit its below-clip subtree',
  )

  // Uncapped, the attribute pass still harvests the whole subtree as before.
  const whole = collect(root)
  assert.match(whole, /below-fold token/, 'no cut-off means no scoping, on every path')
})

test('collectProofAuditText keeps a fully in-frame subtree contiguous', () => {
  // Scoping must not become a SECOND cause of token loss. A phrase spanning
  // inline elements — `<p>Rotation <strong>history</strong></p>` — is one
  // string in the unscoped harvest, and every glyph of it is inside the clip
  // here. Splitting it per text node would drop the phrase from audit.txt for a
  // reason unrelated to clipping, and invite exactly the wrong diagnosis: "the
  // token was below the clip, the recipe was vacuous".
  const collect = loadAuditCollector()
  // Built by hand rather than via stubElement: this needs a real inline split,
  // where the element's textContent is the CONCATENATION of its text nodes.
  const paragraph = {
    nodeType: 1,
    textContent: 'Rotation history',
    getBoundingClientRect: () => ({ top: 10, bottom: 50 }),
    getAttribute: () => null,
    querySelectorAll: () => [],
    childNodes: [],
  }
  paragraph.childNodes = [
    { nodeType: 3, textContent: 'Rotation ', parentElement: paragraph },
    { nodeType: 3, textContent: 'history', parentElement: paragraph },
  ]
  const root = stubRoot([paragraph, stubElement(5000, ['below-fold token'])])

  const clipped = collect(root, 800)
  assert.match(
    clipped,
    /Rotation history/,
    'a phrase entirely inside the clip must survive as one contiguous string',
  )
  assert.doesNotMatch(clipped, /below-fold token/, 'scoping still drops what is below the clip')
})

test('recipe schema documents what each capture field actually captures', () => {
  const schema = JSON.parse(
    fs.readFileSync(path.join(repoRoot, 'proof/recipes/schema.json'), 'utf8'),
  )
  const props = schema.$defs.browserRecipe.allOf[1].properties
  for (const field of ['selector', 'cropToSelector', 'viewport', 'fullPage', 'capture', 'canvas']) {
    const description = props[field]?.description
    assert.equal(typeof description, 'string', `${field} must carry a description`)
    assert.ok(description.trim().length > 0, `${field} description must be non-empty`)
  }
  // The two descriptions that exist to stop a specific, repeatedly-made mistake.
  assert.match(props.cropToSelector.description, /top/i)
  assert.match(props.cropToSelector.description, /not a box crop/i)
  assert.match(props.selector.description, /unobtainable/i)
})

test('the reconnecting recipe stages a dark theme and a post-first-answer hang into both e2e globals', () => {
  const specFor = (id) =>
    buildSpec({
      recipe: { id, surface: 'web', capture: 'still', route: '/' },
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })

  const staged = specFor('web-sessions-reconnecting-dark')
  // `1`, not `0`, and the difference is the whole recipe. The counter is read
  // AFTER recordCall, so `1` answers call one and hangs from call two -- a
  // painted table with a live ladder behind it. `0` would hang from call one,
  // and Sessions returns its loading view with no pill at all.
  assert.match(staged, /hangSessionsReadAfter: 1/)
  // Dark is asserted twice because the two halves fail independently: the
  // emulateMedia call covers a system-preference read, the storage key covers
  // index.html's pre-paint reader. Either alone can leave a light capture.
  assert.match(staged, /emulateMedia\(\{ colorScheme: 'dark' \}\)/)
  assert.match(staged, /localStorage\.setItem\('bossanova\.theme', 'dark'\)/)
  // Same latent-mirror reasoning as the give-up test above: the app fakes read
  // `__BOSSANOVA_E2E__ ?? bossanovaE2e`, so a single-global write is invisible
  // wherever the other one is already installed.
  assert.match(staged, /window\.bossanovaE2e = \{ \.\.\.window\.bossanovaE2e, \.\.\.staged \}/)
  assert.match(
    staged,
    /window\.__BOSSANOVA_E2E__ = \{ \.\.\.window\.__BOSSANOVA_E2E__, \.\.\.staged \}/,
  )

  // The live control. Staged globally, a hanging sessions read would strand
  // every other web recipe behind a permanently-retrying page.
  assert.doesNotMatch(specFor('web-sessions'), /hangSessionsReadAfter/)
})

test('the reconnecting spec waits for the pill AND its staleness clause before it screenshots', () => {
  // Built from the SHIPPED catalog for the same reason as the give-up gate: the
  // screenshot branch buildSpec emits depends on the recipe's own
  // selector/cropToSelector fields, which a hand-built stub would not carry.
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const specFor = (id) => {
    const recipe = catalog.recipes.find((r) => r.id === id)
    assert.ok(recipe, `${id} recipe is missing from the catalog`)
    return buildSpec({
      recipe,
      outputDir: '/tmp/out',
      surface: 'web',
      stageEnv: { VITE_E2E: '1' },
    })
  }

  const staged = specFor('web-sessions-reconnecting-dark')
  // The pill region is mounted unconditionally and starts empty, so waiting on
  // its VISIBILITY is satisfied at first paint and photographs the healthy
  // list. Both clauses are text waits for that reason, and both are asserted
  // here because they fail independently -- see the runner's own note beside
  // the gate.
  assert.match(staged, /toContainText\('Reconnecting…'/)
  assert.match(staged, /toContainText\('Showing data from'/)
  assert.ok(precedes(staged, 'page.goto(', "toContainText('Reconnecting…'"))
  assert.ok(precedes(staged, "toContainText('Showing data from'", 'page.screenshot('))

  // Recipe-scoped: applied to every web still, it would hang each one whose
  // page never reconnects, which is all of them.
  assert.doesNotMatch(specFor('web-sessions'), /Reconnecting…/)
})

test('the shipped catalog keeps the reconnecting recipe wired to the web path rule', () => {
  const catalog = JSON.parse(
    fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
  )
  const recipe = catalog.recipes.find((r) => r.id === 'web-sessions-reconnecting-dark')
  assert.ok(recipe, 'web-sessions-reconnecting-dark recipe is missing from the catalog')
  assert.doesNotThrow(() => validateRecipe(recipe))
  // Absent from the pathRule it is never selected for a services/web diff, and
  // BOS-1093's reconnect-pill proof would silently never run.
  const webRule = catalog.pathRules.find((rule) => rule.patterns.includes('services/web/'))
  assert.ok(webRule.recipeIds.includes('web-sessions-reconnecting-dark'))
  assert.equal(recipe.capture, 'still')
  assert.equal(recipe.route, '/')
  // Uncropped on purpose: the evidence is the pill AND the stale table it
  // qualifies in one frame, so web-sessions' `.data-table-wrap` crop would cut
  // the pill out entirely.
  assert.equal(recipe.cropToSelector, undefined)
  assert.equal(recipe.selector, undefined)
  // Desktop width, well clear of the 390px mobile recipes: the pill sits above
  // the table and a narrow viewport reflows it out of the frame.
  assert.equal(recipe.viewport.width, 1024)
})
