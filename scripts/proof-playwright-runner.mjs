#!/usr/bin/env node

import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { OVERLAY_CAPTION_CSS } from './proof-caption-spec.mjs'
import { normalizeRecipe, validateBrowserRoute } from './proof-lib.mjs'

// OVERLAY_CAPTION_CSS is imported from proof-caption-spec.mjs (BOS-140 D8),
// the single source of truth also consumed by the web agent overlay
// (services/web/tests/e2e/agent/overlay.ts) — replaces the previous
// comment-enforced "keep BYTE-IDENTICAL" duplication with import-equality.
export { OVERLAY_CAPTION_CSS }

// Surfaces are no longer a closed allowlist (BOS-202): a consuming repo declares
// its own via the catalog `surfaces` block, and proof.mjs passes the descriptor's
// --spec-root/--default-crop/--stage-env in. The runner only validates the surface
// id SHAPE (the same slug rule recipe ids use) so it stays a safe path segment.
const validSurfacePattern = /^[a-z0-9][a-z0-9-]*$/
const validRecipeIdPattern = /^[a-z0-9][a-z0-9-]*$/
// Exported so the schema-agreement test can pin proof/recipes/schema.json's
// step-action enum to this set, mirroring how proof-scenario.mjs exports
// STEP_OPS for the scenarios schema. The two encodings HAVE drifted before:
// `press` was live here and in validateRecipe while the schema enum omitted it,
// and the step schema is additionalProperties:false, so valid recipes were
// schema-invalid until it was noticed by hand.
export const VIDEO_ACTIONS = new Set([
  'goto',
  'click',
  'type',
  'wait',
  'scroll',
  'press',
  'select',
  'reload',
])
const DEFAULT_VIDEO_SLOWMO_MS = 350

// ----- Per-recipe staging payloads ---------------------------------------
//
// These MUST stay above the `if (invokedDirectly) run(...)` block below: a
// direct run reaches attachStageScript()/captureReadyScript() while any `const`
// declared later in the module is still in its temporal dead zone, which fails
// the capture with "Cannot access '<name>' before initialization" rather than
// anything that names the recipe.

// The subscribe CTA is disabled until the cloud-access status RPC answers, and
// its label and terms paragraph both branch on that answer. .subscribe-actions
// is visible the moment the route renders, so an unwaited capture can show a
// faded button above copy the verdict is about to replace. Waiting for the
// button to become enabled is exactly waiting for eligibility to resolve.
//
// That makes these ids mutually exclusive with the fake's `holdCloudAccessStatus`,
// which pins the status RPC in `loading` by returning a promise that never
// resolves. The button never becomes enabled under it, so a recipe listed here
// that also staged it would fail the wait above rather than capture anything. No
// proof recipe stages it today -- it is reached only from the e2e specs -- so
// this is a caution for whoever adds the first one, not a live conflict.
const SUBSCRIBE_CTA_RECIPE_IDS = new Set([
  'web-subscribe',
  'web-subscribe-trial-used',
  'web-subscribe-no-notification-prompt',
])

// The opposite subject (BOS-1148): an account whose cloud access is already
// ACTIVE never sees the offer at all -- /subscribe routes it on to the sessions
// list rather than rendering a checkout it cannot use. These ids therefore must
// NOT join the set above: its readiness gate waits for an enabled .subscribe-cta
// that this staging deliberately makes unreachable, so a recipe in both sets
// would hang for 10s and fail rather than capture the redirect.
const SUBSCRIBE_ACTIVE_RECIPE_IDS = new Set(['web-subscribe-active-redirect'])

// BOS-658 evidence line for the web-chat-terminal capture: a status mark, a
// continuation arrow, and a box-drawing rule — the glyph classes that degraded
// on mobile Safari. Without this the capture is an empty terminal pane, because
// proof capture stages its own attach socket instead of reusing
// installAttachServer. Mirrors GLYPH_TOKENS in
// services/web/tests/e2e/specs/chat-terminal.spec.ts.
const CHAT_TERMINAL_GLYPH_TOKENS = ['✓', '↳', '─']
const CHAT_TERMINAL_GLYPH_LINE = `${CHAT_TERMINAL_GLYPH_TOKENS.join(' ')}\r\n`
// xterm splits a rendered row into one span per style run and pads it with cell
// spaces, so match the tokens in order with tolerant spacing rather than an
// exact substring (same tolerance as the e2e spec's GLYPH_ROW_TEXT).
const CHAT_TERMINAL_GLYPH_ROW_SOURCE = CHAT_TERMINAL_GLYPH_TOKENS.join('\\s*')
// Recipes whose staged attach socket replays that data frame. The chat page
// clears its "Connecting…" overlay on the first data byte only, so a chat
// capture without one shows a terminal that never connected.
const CHAT_TERMINAL_DATA_REPLAY_IDS = new Set([
  'web-chat-terminal',
  'web-chat-terminal-upload',
  'web-chat-terminal-paste',
])

// BOS-661 staged upload file for web-chat-terminal-upload. Small on purpose:
// one chunk fits inside a single kind=7 frame, so the capture never depends on
// the ack window draining. The name is what the completion banner echoes.
const CHAT_UPLOAD_FILENAME = 'agent-brief.txt'
const CHAT_UPLOAD_CONTENT = 'Fixture upload for the BOS-661 chat file upload proof.\n'

// True when any route the recipe visits is organization-scoped
// (`/<orgId>/settings/...`, BOS-1073). Those routes mount OrgScopedSettings,
// which reconciles the URL's organization against useAuth().organizationId --
// with none staged it fires a switch and renders "Switching to ..." until the
// fake's claim catches up, so a capture can land on the spinner rather than the
// page. Staging the claim up front makes the capture deterministic.
//
// Deliberately shape-based rather than a fixture-id match: a recipe that coins
// its own organization id still needs the claim staged.
const ORG_SCOPED_ROUTE = /^\/[^/]+\/settings(?:\/|$)/

// Only drive Playwright when invoked directly; importing this module (e.g. from
// the unit tests, to exercise buildSpec/validateRecipe) must not start a run.
import { isMainModule } from '../skills-toolbox/main-module.mjs'

// Recipes whose scene needs the caller to belong to two organizations: the
// picker switch, and the billing-portal entry proving it follows the
// organization on screen rather than the session's claim. callerRole 1 is
// MemberRole.OWNER, which the billing action is gated on.
const TWO_ORGANIZATION_RECIPE_IDS = new Set([
  'web-org-picker-switch',
  'web-org-billing-portal-second-org',
])

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) {
  try {
    run(process.argv.slice(2))
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}

function run(argv) {
  const args = parseArgs(argv)
  const recipe = normalizeRecipe(JSON.parse(fs.readFileSync(args.recipe, 'utf8')))
  validateRecipe(recipe)
  fs.mkdirSync(args['output-dir'], { recursive: true })

  const specDir = path.join(
    serviceSpecRoot(args['spec-root'], args.surface),
    `proof-${process.pid}`,
  )
  fs.mkdirSync(specDir, { recursive: true })
  const specPath = path.join(specDir, 'proof.spec.ts')
  const stageEnv = stageEnvForArgs(args)
  fs.writeFileSync(
    specPath,
    buildSpec({
      recipe,
      outputDir: path.resolve(args['output-dir']),
      surface: args.surface,
      defaultCrop: args['default-crop'],
      stageEnv,
    }),
  )

  let cleaned = false
  const cleanup = () => {
    if (!cleaned) {
      cleaned = true
      fs.rmSync(specDir, { recursive: true, force: true })
    }
  }

  const child = spawn('pnpm', ['exec', 'playwright', 'test', specPath, '--project=chromium'], {
    cwd: process.cwd(),
    stdio: 'inherit',
    env: {
      ...process.env,
      // The staging env (web's VITE_E2E today) arrives via --stage-env from the
      // surface descriptor; a surface without one exports nothing extra.
      ...(stageEnv ?? {}),
    },
  })

  for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
    process.once(signal, () => {
      child.kill(signal)
      cleanup()
      process.exit(1)
    })
  }

  child.on('error', (error) => {
    cleanup()
    console.error(error.message)
    process.exitCode = 1
  })

  child.on('exit', (code) => {
    cleanup()
    process.exitCode = code ?? 1
  })
}

export function parseArgs(argv) {
  const parsed = {}
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i]
    const value = argv[i + 1]
    if (!key?.startsWith('--') || !value) {
      throw new Error(`invalid argument near ${key ?? '<end>'}`)
    }
    parsed[key.slice(2)] = value
  }
  for (const required of ['surface', 'recipe', 'output-dir']) {
    if (!parsed[required]) {
      throw new Error(`missing --${required}`)
    }
  }
  if (!validSurfacePattern.test(parsed.surface)) {
    throw new Error(`invalid --surface: ${parsed.surface}`)
  }
  return parsed
}

export function stageEnvForArgs(args) {
  if (args['stage-env']) {
    return JSON.parse(args['stage-env'])
  }
  if (args.surface === 'web') {
    return { VITE_E2E: '1' }
  }
  return undefined
}

/**
 * Convert a human-readable label into a URL/filename-safe slug.
 * Lowercases, replaces each run of non-[a-z0-9] with '-', trims leading/trailing
 * dashes, and falls back to 'step' when the result would be empty.
 *
 * @param {string} text
 * @returns {string}
 */
export function slugify(text) {
  const slug = String(text ?? '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
  return slug || 'step'
}

export function validateRecipe(recipe) {
  if (!validRecipeIdPattern.test(String(recipe.id ?? ''))) {
    throw new Error(`invalid recipe id: ${recipe.id ?? '<missing>'}`)
  }
  if (recipe.capture === 'video') {
    if (!Array.isArray(recipe.steps) || recipe.steps.length === 0) {
      throw new Error('video recipe requires a non-empty steps array')
    }
    for (const step of recipe.steps) {
      if (!VIDEO_ACTIONS.has(step.action)) {
        throw new Error(`unsupported video step action: ${step.action ?? '<missing>'}`)
      }
      if (step.label !== undefined) {
        if (typeof step.label !== 'string' || step.label.length === 0) {
          throw new Error('video step label must be a non-empty string when provided')
        }
      }
      if (step.action === 'goto') {
        if (typeof step.route !== 'string' || step.route.length === 0) {
          throw new Error('video goto step requires route')
        }
        validateBrowserRoute(step.route)
      } else if (step.action === 'click') {
        requireVideoStepString(step, 'selector')
      } else if (step.action === 'type') {
        requireVideoStepString(step, 'selector')
        requireVideoStepString(step, 'value')
      } else if (step.action === 'press') {
        requireVideoStepString(step, 'key')
      } else if (step.action === 'select') {
        requireVideoStepString(step, 'selector')
        // '' is a legitimate option value (the leading "All …" filter entry), so
        // this deliberately does NOT go through requireVideoStepString.
        if (typeof step.value !== 'string') {
          throw new Error('video select step requires value')
        }
      } else if (step.action === 'wait' && step.selector !== undefined) {
        requireVideoStepString(step, 'selector')
      } else if (step.action === 'scroll') {
        if (step.toSelector === undefined && step.byPx === undefined && step.fullPage !== true) {
          throw new Error('scroll step requires toSelector, byPx, or fullPage')
        }
        if (step.toSelector !== undefined) {
          requireVideoStepString(step, 'toSelector')
        }
        if (step.byPx !== undefined && !Number.isFinite(step.byPx)) {
          throw new Error('scroll step byPx must be a finite number')
        }
      }
    }
    return
  }
  validateBrowserRoute(recipe.route)
}

function requireVideoStepString(step, field) {
  if (typeof step[field] !== 'string' || step[field].length === 0) {
    throw new Error(`video ${step.action} step requires ${field}`)
  }
}

function serviceSpecRoot(specRoot, surface) {
  // Prefer the catalog-declared spec root (passed by proof.mjs via --spec-root);
  // fall back to the shipped convention (marketing/docs → tests/e2e, everything
  // else → tests/e2e/specs) for direct/legacy invocations without the arg.
  const fallback = surface === 'marketing' || surface === 'docs' ? 'tests/e2e' : 'tests/e2e/specs'
  return path.resolve(specRoot || fallback)
}

export function buildSpec({ recipe, outputDir, surface, defaultCrop, stageEnv }) {
  if (recipe.capture === 'video') {
    return buildVideoSpec({ recipe, outputDir, surface, defaultCrop, stageEnv })
  }
  const fileName = `${recipe.id}.png`
  const target = JSON.stringify(path.join(outputDir, fileName))
  const auditTarget = JSON.stringify(path.join(outputDir, 'audit.txt'))
  const route = JSON.stringify(recipe.route)
  const selector = JSON.stringify(recipe.selector ?? '')
  const cropToSelector = JSON.stringify(recipe.cropToSelector ?? '')
  const viewport = JSON.stringify(recipe.viewport ?? { width: 1440, height: 1000 })
  const fullPage = Boolean(recipe.fullPage)
  const stageWeb = stageEnv ? webStageScript(recipe) : ''
  const captureReady = stageEnv ? captureReadyScript(recipe) : ''
  const testTitle = JSON.stringify(`proof screenshot: ${recipe.id}`)

  return `
import { expect, test } from '@playwright/test';

${collectProofAuditTextScript()}

test(${testTitle}, async ({ page }) => {
  ${stageWeb}
  await page.setViewportSize(${viewport});
  const response = await page.goto(${route});
  expect(response?.status(), 'proof route status').toBeLessThan(400);${captureReady}
  const selector = ${selector};
  const cropToSelector = ${cropToSelector};
  let auditText = '';
  if (cropToSelector) {
    // Capture the full page from the top (keeping the app header/chrome) but
    // clip the bottom to where the content ends, dropping the blank space that
    // flex:1 layouts leave below short content.
    const region = page.locator(cropToSelector).first();
    await expect(region).toBeVisible();
    const box = await region.boundingBox();
    if (!box) throw new Error('cropToSelector not found: ' + cropToSelector);
    const size = page.viewportSize() ?? ${viewport};
    const height = Math.min(size.height, Math.ceil(box.y + box.height + 24));
    // Harvest audit text only from nodes whose top edge is above the clip. A
    // token below \`height\` never appeared in the artifact, so matching it in
    // audit.txt would be a vacuous proof. This is a VERTICAL top-edge test, not
    // a visibility test: a display:none subtree measures 0/0 and still counts as
    // in frame, and nothing here excludes content off to the right. See the
    // caveat list in proof/recipes/README.md.
    auditText = await page.locator('body').evaluate(collectProofAuditText, height);
    await page.screenshot({ path: ${target}, clip: { x: 0, y: 0, width: size.width, height } });
  } else if (selector) {
    const target = page.locator(selector).first();
    await expect(target).toBeVisible();
    auditText = await target.evaluate(collectProofAuditText);
    await target.screenshot({ path: ${target} });
  } else {
    auditText = await page.locator('body').evaluate(collectProofAuditText);
    await page.screenshot({ path: ${target}, fullPage: ${fullPage} });
  }
  await test.info().attach('proof audit text', { body: auditText, contentType: 'text/plain' });
  await import('node:fs').then((fs) => fs.writeFileSync(${auditTarget}, auditText));
});
`
}

function buildVideoSpec({ recipe, outputDir, surface, defaultCrop, stageEnv }) {
  const webmPath = JSON.stringify(path.join(outputDir, `${recipe.id}.webm`))
  const posterPath = JSON.stringify(path.join(outputDir, `${recipe.id}.png`))
  const auditTarget = JSON.stringify(path.join(outputDir, 'audit.txt'))
  const metaPath = JSON.stringify(path.join(outputDir, 'video-meta.json'))
  const outputDirJson = JSON.stringify(outputDir)
  const viewportObj = recipe.viewport ?? { width: 1440, height: 1000 }
  const viewport = JSON.stringify(viewportObj)

  // Determine the effective crop selector: recipe-level, then the descriptor's
  // default crop (passed via --default-crop), then the shipped surface heuristic
  // as a fallback for direct/legacy invocations.
  const defaultCropToSelector = defaultCrop ?? (surface === 'marketing' ? 'main' : '#root')
  const cropToSelector = jsString(recipe.cropToSelector ?? defaultCropToSelector)

  const stageWeb = stageEnv ? webStageScript(recipe) : ''
  // Same readiness gate as buildSpec, gated the same way on stageEnv: without
  // it a video capture can record (and still-capture) the pre-eligibility frame
  // the stills gate exists to prevent. Load-bearing as of BOS-1090:
  // `web-accounts-give-up-retry` is the first SHIPPED capture "video" recipe
  // with a `captureReadyScript` arm, and that arm is what keeps the video from
  // stepping before the accounts table has loaded. Every other armed id
  // (web-subscribe*, web-session-expired, web-sessions-daemons-give-up,
  // web-accounts-cold-start-probe-failure, web-chat-terminal) is capture
  // "still", so this branch also remains what lets one of those be promoted to
  // video without quietly losing its gate.
  const captureReady = stageEnv ? captureReadyScript(recipe) : ''
  const testTitle = JSON.stringify(`proof video: ${recipe.id}`)
  const slowMo = Number(recipe.slowMo ?? DEFAULT_VIDEO_SLOWMO_MS)

  // Build step lines interleaved with still-capture blocks.
  const stepLines = recipe.steps
    .map((step, i) => {
      const nn = String(i + 1).padStart(2, '0')
      const label = step.label ?? step.action
      const slug = slugify(label)
      const fileName = `${nn}-${slug}.png`
      const outPath = JSON.stringify(path.join(outputDir, fileName))
      const fileNameJson = jsString(fileName)
      const labelJson = jsString(label)
      const action = renderVideoStep(step)
      // Only a navigation can land on a page whose evidence has not arrived
      // yet, so the gate rides the goto steps rather than every step.
      const ready = step.action === 'goto' ? captureReady : ''
      const stillBlock = `  {
    const __h = await captureStill(page, ${outPath}, ${cropToSelector}, ${viewport});
    __stills.push({ fileName: ${fileNameJson}, label: ${labelJson} });
    if (__h === null) __disableCrop = true;
    else if (!__disableCrop) __cropHeight = Math.max(__cropHeight ?? 0, __h);
  }`
      return `${action}${ready}\n${stillBlock}`
    })
    .join('\n')

  // Video records at the context level, so this spec owns its own context (the
  // shared `page` fixture cannot be told to record). The webm finalizes only on
  // context.close(); we screenshot a poster first, then rename the finalized
  // file to a stable <id>.webm.
  return `
import { expect, test } from '@playwright/test';
import fs from 'node:fs';

${collectProofAuditTextScript()}

${captureStillScript()}

test.use({ launchOptions: { slowMo: ${slowMo} } });

test(${testTitle}, async ({ browser, baseURL }) => {
  const context = await browser.newContext({
    baseURL,
    viewport: ${viewport},
    recordVideo: { dir: ${outputDirJson}, size: ${viewport} },
  });
  const page = await context.newPage();
  ${stageWeb}
${proofOverlayScript()}
  await page.setViewportSize(${viewport});
  const __stills = [];
  let __cropHeight = null;
  let __disableCrop = false;
${stepLines}
  const auditText = await page.locator('body').evaluate(collectProofAuditText);
  fs.writeFileSync(${auditTarget}, auditText);
  await page.screenshot({ path: ${posterPath} });
  const video = page.video();
  await context.close(); // finalizes the recorded video file
  if (video) {
    const tmp = await video.path();
    fs.renameSync(tmp, ${webmPath});
  }
  fs.writeFileSync(${metaPath}, JSON.stringify({ cropHeight: __disableCrop ? null : __cropHeight, stills: __stills }, null, 2));
});
`
}

// Single-quote a value as a JS string literal for the generated spec. Recipe
// content is trusted (fixture-only), but escape backslashes/quotes/newlines so
// selectors and typed values cannot break the generated spec's syntax.
function jsString(value) {
  return `'${String(value ?? '')
    .replace(/\\/g, '\\\\')
    .replace(/'/g, "\\'")
    .replace(/\r/g, '\\r')
    .replace(/\n/g, '\\n')
    .replace(/\u2028/g, '\\u2028')
    .replace(/\u2029/g, '\\u2029')}'`
}

// Unroll one video step into an inline statement (values baked in, not a
// runtime loop) so the generated spec is self-contained and inspectable.
function renderVideoStep(step) {
  const captionLine =
    step.caption !== undefined
      ? `  await page.evaluate((t) => window.__proofOverlay?.caption(t), ${jsString(step.caption)});\n`
      : ''
  switch (step.action) {
    case 'goto':
      // Emit the caption AFTER navigation: goto destroys the current document
      // (and its overlay), so a pre-goto caption would never be seen. The
      // overlay re-injects on the loaded page via addInitScript, so setting the
      // caption here makes it visible on the destination page.
      return `  {
    const response = await page.goto(${jsString(step.route)});
    expect(response?.status(), 'proof route status').toBeLessThan(400);
  }
${captionLine}`
    case 'click':
      return `${captionLine}  {
    const __loc = page.locator(${jsString(step.selector)}).first();
    await __loc.scrollIntoViewIfNeeded();
    const __box = await __loc.boundingBox();
    if (__box) await page.evaluate(([x, y]) => window.__proofOverlay?.ripple(x, y), [__box.x + __box.width / 2, __box.y + __box.height / 2]);
    await __loc.click();
  }`
    case 'type':
      return `${captionLine}  {
    const __loc = page.locator(${jsString(step.selector)}).first();
    await __loc.scrollIntoViewIfNeeded();
    const __box = await __loc.boundingBox();
    if (__box) await page.evaluate(([x, y]) => window.__proofOverlay?.ripple(x, y), [__box.x + __box.width / 2, __box.y + __box.height / 2]);
    await __loc.pressSequentially(${jsString(step.value)}, { delay: 60 });
  }`
    case 'press':
      // Global keyboard shortcut (e.g. opening the launcher with '?'): press on
      // the page keyboard rather than a located element, since the target is a
      // window-level handler with no clickable trigger.
      return `${captionLine}  await page.keyboard.press(${jsString(step.key)});`
    case 'select':
      // Native <select>: a click would only open the OS-drawn popup, which the
      // browser never paints into a screenshot or video. selectOption drives the
      // real change event, so the narrowing it causes IS the captured evidence.
      return `${captionLine}  {
    const __loc = page.locator(${jsString(step.selector)}).first();
    await __loc.scrollIntoViewIfNeeded();
    const __box = await __loc.boundingBox();
    if (__box) await page.evaluate(([x, y]) => window.__proofOverlay?.ripple(x, y), [__box.x + __box.width / 2, __box.y + __box.height / 2]);
    await __loc.selectOption(${jsString(step.value)});
  }`
    case 'reload':
      // Reload keeps storage (unlike goto with a fresh context), which is the
      // whole point for a persistence proof. Caption after the navigation for
      // the same reason as goto: the reload destroys the overlay.
      return `  {
    const response = await page.reload();
    expect(response?.status(), 'proof reload status').toBeLessThan(400);
  }
${captionLine}`
    case 'wait':
      return step.selector
        ? `${captionLine}  await expect(page.locator(${jsString(step.selector)}).first()).toBeVisible();`
        : `${captionLine}  await page.waitForTimeout(${Number(step.timeoutMs) || 500});`
    case 'scroll':
      if (step.fullPage === true) {
        return `${captionLine}  await page.evaluate(async () => {
    const dwell = (ms) => new Promise((r) => setTimeout(r, ms));
    // Some layouts scroll the documentElement rather than body; take the max so
    // we always reach the true bottom (the PR #863 "stops short" failure mode).
    const pageHeight = () => Math.max(document.body.scrollHeight, document.documentElement.scrollHeight);
    const step = Math.max(1, Math.round(window.innerHeight * 0.8));
    for (let y = 0; y < pageHeight() - window.innerHeight; y += step) {
      window.scrollTo({ top: y, behavior: 'smooth' });
      await dwell(450);
    }
    window.scrollTo({ top: pageHeight(), behavior: 'smooth' });
    await dwell(450);
    window.scrollTo({ top: 0, behavior: 'smooth' });
    await dwell(450);
  });
  await page.waitForTimeout(400);`
      }
      return step.toSelector
        ? `${captionLine}  await page.locator(${jsString(step.toSelector)}).first().evaluate((el) => el.scrollIntoView({ behavior: 'smooth', block: 'center' }));
  await page.waitForTimeout(800);`
        : `${captionLine}  await page.evaluate((px) => window.scrollBy({ top: px, behavior: 'smooth' }), ${Number(step.byPx)});
  await page.waitForTimeout(800);`
    default:
      throw new Error(`unsupported video step action: ${step.action ?? '<missing>'}`)
  }
}

function proofOverlayScript() {
  // addInitScript runs at document-start, where document.documentElement,
  // document.body and document.head can all still be null. Building the overlay
  // eagerly there threw "Cannot read properties of null (reading 'appendChild')"
  // so __proofOverlay was never installed and every caption()/ripple() silently
  // no-opped via the `?.` — the caption never rendered. Build the DOM lazily on
  // first use, by which point the document root exists, and re-create it if a
  // navigation detached it.
  return `
  await page.addInitScript(() => {
    if (window.__proofOverlay) return;
    let parts = null;
    const ensure = () => {
      if (parts && parts.captionEl.isConnected) return parts;
      const root = document.body || document.documentElement;
      if (!root) return null;
      const container = document.createElement('div');
      container.style.cssText = 'position:fixed;top:0;left:0;width:100%;height:100%;z-index:2147483647;pointer-events:none;overflow:hidden;';
      root.appendChild(container);
      const captionEl = document.createElement('div');
      captionEl.style.cssText = '${OVERLAY_CAPTION_CSS}';
      container.appendChild(captionEl);
      const style = document.createElement('style');
      style.textContent = '@keyframes __proofRipple{0%{transform:translate(-50%,-50%) scale(0);opacity:1}100%{transform:translate(-50%,-50%) scale(2.5);opacity:0}}';
      (document.head || root).appendChild(style);
      parts = { container, captionEl };
      return parts;
    };
    window.__proofOverlay = {
      caption(text) {
        const p = ensure();
        if (!p) return;
        p.captionEl.textContent = text;
        p.captionEl.style.display = text ? 'block' : 'none';
      },
      ripple(x, y) {
        const p = ensure();
        if (!p) return;
        const el = document.createElement('div');
        el.style.cssText = \`position:absolute;left:\${x}px;top:\${y}px;width:0;height:0;pointer-events:none;\`;
        const circle = document.createElement('div');
        circle.style.cssText = 'position:absolute;transform:translate(-50%,-50%);width:40px;height:40px;border-radius:50%;background:rgba(255,80,80,0.45);animation:__proofRipple 0.5s ease-out forwards;pointer-events:none;';
        el.appendChild(circle);
        p.container.appendChild(el);
        setTimeout(() => el.remove(), 600);
      },
    };
  });`
}

function captureStillScript() {
  return `
async function captureStill(page, outPath, cropToSelector, viewport) {
  const size = page.viewportSize() ?? viewport;
  try {
    if (cropToSelector) {
      const region = page.locator(cropToSelector).first();
      await region.waitFor({ state: 'visible', timeout: 2000 });
      const box = await region.boundingBox();
      if (box) {
        const height = Math.min(size.height, Math.ceil(box.y + box.height + 24));
        await page.screenshot({ path: outPath, clip: { x: 0, y: 0, width: size.width, height } });
        return height;
      }
    }
  } catch {}
  await page.screenshot({ path: outPath }); // full-frame fallback
  return null;
}
`
}

export function collectProofAuditTextScript() {
  return `
// \`cutoff\` is the clip height of a cropToSelector capture, in CSS pixels. When
// supplied, only nodes whose top edge sits above it are harvested, so audit.txt
// describes the saved frame rather than the whole document. Omit it and the
// whole-body / element-scoped behaviour is unchanged.
function collectProofAuditText(element, cutoff) {
  const values = [];
  const push = (value) => {
    const text = String(value ?? '').trim();
    if (text) values.push(text);
  };
  const limit = typeof cutoff === 'number' && Number.isFinite(cutoff) ? cutoff : null;
  // Fail-open: a text node has no getBoundingClientRect, so fall back to its
  // parent element's box, and INCLUDE any node whose top edge cannot be
  // resolved at all. Dropping unpositioned text would silently shrink audit.txt.
  const rectOf = (node) => {
    if (typeof node?.getBoundingClientRect !== 'function') return null;
    try {
      return node.getBoundingClientRect();
    } catch {
      return null;
    }
  };
  const withinCutoff = (node) => {
    if (limit === null) return true;
    const measured = typeof node?.getBoundingClientRect === 'function' ? node : node?.parentElement;
    if (!measured) return true;
    const rect = rectOf(measured);
    if (!rect) return true;
    const top = rect.top;
    if (typeof top !== 'number' || !Number.isFinite(top)) return true;
    return top < limit;
  };
  const visit = (node) => {
    if (!withinCutoff(node)) return;
    // \`textContent\` is the WHOLE subtree. Under a cut-off that would re-admit
    // below-clip text through any element that merely STARTS above the clip —
    // a straddling [aria-label] section or nav is the common shape — so take
    // it only when unscoped. The walk below already harvested every in-frame
    // text node, making this push redundant as well as leaky.
    if (limit === null) push(node.textContent);
    push(node.getAttribute?.('aria-label'));
    push(node.getAttribute?.('title'));
    push(node.getAttribute?.('alt'));
    if ('value' in node) push(node.value);
    if ('placeholder' in node) push(node.placeholder);
  };
  // Under a cut-off this contributes the root's own attributes only — its
  // textContent is the whole document, which is what the walk below exists to
  // avoid taking wholesale.
  visit(element);
  if (limit !== null) {
    // The root's own textContent carries the entire document, including
    // everything below the clip, so descend instead of taking it in one piece.
    //
    // A container whose own top edge is at/below the cut-off is skipped whole:
    // its subtree is below the clip too, except for the rare child lifted above
    // it by absolute positioning, a negative margin or a transform.
    //
    // A container that is ENTIRELY above the cut-off is taken whole, in one
    // contiguous string, exactly as the unscoped path would. Descending into it
    // would split a phrase that spans inline elements — \`<p>Rotation
    // <strong>history</strong></p>\` becoming two separate lines — and drop a
    // multi-word evidence token whose every glyph was in frame. Only a
    // STRADDLING container is worth walking node by node.
    const walk = (node) => {
      for (const child of node.childNodes ?? []) {
        if (child.nodeType === 3) {
          if (withinCutoff(child)) push(child.textContent);
        } else if (child.nodeType === 1 && withinCutoff(child)) {
          const rect = rectOf(child);
          const bottom = rect ? rect.bottom : null;
          if (typeof bottom === 'number' && Number.isFinite(bottom) && bottom <= limit) {
            push(child.textContent);
          } else {
            walk(child);
          }
        }
      }
    };
    if (withinCutoff(element)) walk(element);
  }
  for (const node of element.querySelectorAll?.('input, textarea, select, option, img, [aria-label], [title], [alt]') ?? []) {
    visit(node);
  }
  return values.join('\\n');
}
`
}

function webStageScript(recipe) {
  return `
  // NOTE: comments in here live inside a template literal, so a backtick in one ENDS the literal.
  // The file still parses and prettier reformats the wreckage, so nothing looks wrong. Escape it as
  // a backslash-backtick if you truly need one. Gated by scripts/check-template-literal-comments.mjs.
  await page.addInitScript(() => {
    window.bossanovaE2e = {
      sessions: [{
        id: 'sess-e2e-1',
        title: 'Proof session',
        branchName: 'proof/screenshots',
        baseBranch: 'main',
        repoId: 'repo-proof',
        repoDisplayName: 'bossanova',
        // Canonical origin URL, matching how proxyListReposAggregated derives an
        // AggregatedRepo originUrl from a repo id. Feeds the sessions-list
        // repository filter (BOS-654).
        repoOriginUrl: 'https://github.com/e2e/repo-proof',
        prNumber: 597,
        prUrl: 'https://github.com/recurser/bossanova/pull/597',
        // DisplayStatus.PASSING (=6) so the session-detail header offers the
        // Merge button — the Merge gate requires an open PR whose display
        // status is PASSING (BOS-365 item 3).
        displayStatus: 6,
        // Owning daemon surfaced on the chat-list snapshot so the new-chat page
        // (web-new-chat recipe) can resolve a daemon and list its agents rather
        // than falling back to auto-creating a chat with the session agent.
        daemonId: 'daemon-proof',
        // Mix agent names so the desktop Agent column visibly carries a per-row
        // value (BOS-700). Mobile: see the .cell-agent rule in services/web/src/index.css.
        chats: [
          { id: 'chat-1', agentSessionId: 'claude-1', title: 'Proof chat', status: 'idle', agentName: 'claude' },
          { id: 'chat-e2e-2', agentSessionId: 'codex-e2e-2', title: 'A much longer chat title that should wrap onto a second line at 390px', status: 'idle', agentName: 'codex' },
          { id: 'chat-e2e-3', agentSessionId: 'claude-e2e-3', title: 'Another chat', status: 'idle', agentName: 'claude' },
        ],
      }, {
        // BOS-704: archive_pending overrides the rendered label with orange,
        // spinning "archiving" while the retained DisplayStatus.MERGED (=7)
        // keeps every non-status cell visibly finished in the sessions proof.
        id: 'sess-e2e-archiving-merged',
        title: 'Merged proof session archiving',
        branchName: 'proof/archive-merged',
        baseBranch: 'main',
        daemonId: 'daemon-proof',
        repoId: 'repo-proof',
        repoDisplayName: 'bossanova',
        repoOriginUrl: 'https://github.com/e2e/repo-proof',
        prNumber: 704,
        prUrl: 'https://github.com/recurser/bossanova/pull/704',
        displayStatus: 7,
        displayLabel: 'archiving',
        // DisplayIntent.WARNING (=2); displaySpinner comes straight from bossd.
        displayIntent: 2,
        displaySpinner: true,
        archivePending: true,
      }, {
        // A Quick-Chat session with no PR: the header must show New chat/Archive
        // but NO Merge button and NO "Switch account" control (BOS-365 items 1 & 3).
        id: 'sess-e2e-quick',
        title: 'Proof quick chat',
        branchName: 'proof/quick-chat',
        baseBranch: 'main',
        // Attributed to the SECOND daemon and the second repository so the
        // sessions-list filters visibly narrow the table (BOS-654).
        daemonId: 'daemon-proof-standby',
        repoId: 'repo-proof-web',
        repoDisplayName: 'bossanova-web',
        repoOriginUrl: 'https://github.com/e2e/repo-proof-web',
        // BOS-700: a SINGLE-agent session that still names its agent. The
        // mixed-agent fixture above cannot prove the regression this ticket
        // fixes — before BOS-700 a one-agent chat list rendered no Agent column
        // at all — and a chat with no agentName would only render the dash
        // fallback. This is the still behind Required-proof item 4.
        chats: [{ id: 'chat-2', agentSessionId: 'claude-2', title: 'Quick chat', status: 'idle', agentName: 'claude' }],
      }, {
        // BOS-668: a session holding one chat parked on an armed GitHub callback
        // next to one genuinely working chat, so the web-session-waiting recipe
        // captures BOTH badges in one still — 'waiting' (INFO, no spinner) and
        // 'working' (spinner) — plus the reason notice above the table. The
        // parked chat must NOT read 'stopped', which is what the panel produced
        // before ChatStatus.WAITING had a case.
        id: 'sess-e2e-waiting',
        title: 'Proof waiting session',
        branchName: 'proof/waiting',
        baseBranch: 'main',
        daemonId: 'daemon-proof',
        repoId: 'repo-proof',
        repoDisplayName: 'bossanova',
        repoOriginUrl: 'https://github.com/e2e/repo-proof',
        prNumber: 668,
        prUrl: 'https://github.com/recurser/bossanova/pull/668',
        chats: [
          {
            id: 'chat-e2e-waiting',
            agentSessionId: 'claude-e2e-waiting',
            title: 'Ship the release checklist',
            status: 'waiting',
            // BOS-700: both chats here are Claude chats. Without a name the
            // now-unconditional Agent column renders a dash in every row of
            // this BOS-668 still, which reads as missing data.
            agentName: 'claude',
            // Byte-identical to the TUI fixture's reason (BOS-668): both
            // surfaces render the wording displaystatus.CallbackWaitingReason
            // composes, so the two stills can be compared literally.
            waitingReason: 'awaiting checks_passed_ready on acme/my-app#668',
          },
          { id: 'chat-e2e-busy', agentSessionId: 'claude-e2e-busy', title: 'Rebuild the search index', status: 'working', agentName: 'claude' },
        ],
      }, {
        // BOS-855: a session-level outcome hint is PAST tense, so on a row whose
        // own status reads as live activity it renders recessively. This row
        // pairs a live "working" status with a residual
        // "finalize failed (pr_skipped_no_github)" attention hint under a
        // DEMOTABLE reason (AttentionReason.BLOCKED_MAX_ATTEMPTS = 1), so the
        // sessions list shows the error line in its recessive treatment beside a
        // live status — the contradiction the ticket reported, reconciled.
        id: 'sess-e2e-live-past-failure',
        title: 'Proof live rebase run',
        branchName: 'proof/live-past-failure',
        baseBranch: 'main',
        daemonId: 'daemon-proof',
        repoId: 'repo-proof',
        repoDisplayName: 'bossanova',
        repoOriginUrl: 'https://github.com/e2e/repo-proof',
        prNumber: 855,
        prUrl: 'https://github.com/recurser/bossanova/pull/855',
        // The live composite: 'working' with DisplayIntent.DANGER (=3) and a
        // spinner, exactly what displaystatus.Compute serves for a blocked
        // session whose chat is still working.
        displayLabel: 'working',
        displayIntent: 3,
        displaySpinner: true,
        attentionStatus: {
          needsAttention: true,
          reason: 1,
          summary: 'finalize failed (pr_skipped_no_github)',
        },
        chats: [
          { id: 'chat-e2e-live-past-failure', agentSessionId: 'claude-e2e-live-past-failure', title: 'Rebase onto main', status: 'working', agentName: 'claude' },
        ],
      }, {
        // BOS-1133: a desktop sessions-list row carrying both a long name and a
        // long attention hint must clip inside the Name column instead of
        // widening the table until PR/Status are outside .data-table-wrap.
        id: 'sess-e2e-long-error',
        title: 'Proof draft PR creation failed session with an intentionally long title that should clip before it reaches the PR and Status columns',
        branchName: 'proof/long-error',
        baseBranch: 'main',
        daemonId: 'daemon-proof',
        repoId: 'repo-proof',
        repoDisplayName: 'bossanova',
        repoOriginUrl: 'https://github.com/e2e/repo-proof',
        prNumber: 1133,
        prUrl: 'https://github.com/recurser/bossanova/pull/1133',
        displayLabel: 'blocked',
        attentionStatus: {
          needsAttention: true,
          summary: 'draft PR creation failed because the web service returned a long blocked attention hint with retry details, remote branch context, check status notes, mergeability notes, and recovery guidance that belongs in the session detail view',
        },
        chats: [
          { id: 'chat-e2e-long-error', agentSessionId: 'claude-e2e-long-error', title: 'Repair the sessions table', status: 'idle', agentName: 'claude' },
        ],
      }, {
        // A session carrying a BOS-409 stale-failover-proxy-port audit record
        // (UNSPECIFIED outcome, whole message in detail) so the
        // web-session-detail-rotation recipe proves BOS-432: the row renders the
        // FULL detail string with no generic "rotation" fallback. Most fixtures
        // omit rotationEvents, so the block renders nothing there.
        id: 'sess-e2e-rotation',
        title: 'Proof rotated session',
        branchName: 'proof/rotation',
        baseBranch: 'main',
        daemonId: 'daemon-proof',
        repoId: 'repo-proof',
        repoDisplayName: 'bossanova',
        repoOriginUrl: 'https://github.com/e2e/repo-proof',
        rotationEvents: [{
          id: 'rot-e2e-1',
          // RotationOutcome.UNSPECIFIED (=0) — no meaningful label, so the row
          // renders "<time> <detail>" (BOS-432).
          outcome: 0,
          detail: 'stale failover-proxy port: pane baked 52106, live proxy on 44127 — restart this pane to reconnect (BOS-409)',
          // Pin the event to today at 14:34 local so the date-aware timestamp
          // renders time-only ("14:34") deterministically on any capture day.
          createdAtSeconds: Math.floor(new Date(new Date().setHours(14, 34, 0, 0)).getTime() / 1000),
        }],
      }],
      // Expose two registered agents so the new-chat picker (web-new-chat) and
      // the new-session wizard's agent step actually render the AgentSelect list
      // rather than auto-skipping past it (the page auto-advances with <=1 agent).
      agents: ['claude', 'codex'],
      // Seed connected repos so the web-repositories recipe (/settings/repos)
      // renders the repo table (.data-table-wrap) with Disconnect buttons
      // instead of the empty state — the recipe crops to that selector.
      githubRepos: [
        { repoOriginUrl: 'github.com/recurser/bossanova', installed: true },
        { repoOriginUrl: 'github.com/madverts/madverts-core', installed: true },
      ],
      // Seed daemon-local repositories so repository settings proof flows can
      // open a form and confirmation dialog without relying on a live daemon.
      //
      // TWO daemons and TWO repositories (BOS-654): the sessions-list daemon and
      // repository filters only have something to narrow when the fixture spans
      // more than one of each. 'daemon-proof' / 'repo-proof' stay FIRST — the
      // fake binds its default cron jobs and rotation accounts to the first
      // seeded daemon/repo, and every repo-settings flow clicks .first()
      // (alphabetical ordering keeps 'Proof repository' ahead of 'Proof web
      // repository', so BOS-656's sort does not move that target).
      daemons: [
        { id: 'daemon-proof', displayName: 'Proof daemon' },
        { id: 'daemon-proof-standby', displayName: 'Standby daemon' },
      ],
      // Cron jobs spanning BOTH daemons (BOS-657). The built-in defaultCronJobs()
      // binds every job to the first daemon, which leaves the cron list's daemon
      // filter with nothing to narrow and its Daemon column showing one value.
      // Seeded deliberately OUT of alphabetical order so the still also proves
      // the ascending name sort. lastRunStatus is the numeric CronJobStatus
      // (1 IDLE, 2 RUNNING, 3 FAILED) because the fixture crosses addInitScript
      // as plain JSON.
      cronJobs: [
        {
          id: 'cron-weekly-changelog',
          name: 'Weekly changelog',
          schedule: '0 9 * * 1',
          agentName: 'claude',
          repoId: 'repo-proof',
          daemonId: 'daemon-proof',
          daemonHostname: 'Proof daemon',
          enabled: false,
          lastRunStatus: 1,
        },
        {
          id: 'cron-sentry-triage',
          name: 'Sentry triage',
          schedule: '*/30 * * * *',
          agentName: 'codex',
          repoId: 'repo-proof',
          daemonId: 'daemon-proof',
          daemonHostname: 'Proof daemon',
          enabled: true,
          lastRunStatus: 3,
        },
        {
          id: 'cron-nightly-deps',
          name: 'Nightly dependency sweep',
          schedule: '0 3 * * *',
          agentName: 'claude',
          repoId: 'repo-proof',
          daemonId: 'daemon-proof',
          daemonHostname: 'Proof daemon',
          enabled: true,
          lastRunStatus: 2,
        },
        {
          id: 'cron-standby-cache',
          name: 'Standby cache warmer',
          schedule: '*/15 * * * *',
          agentName: 'codex',
          repoId: 'repo-proof',
          daemonId: 'daemon-proof-standby',
          daemonHostname: 'Standby daemon',
          enabled: true,
          lastRunStatus: 1,
        },
      ],
      // Rotation accounts spanning BOTH daemons (BOS-655). The built-in
      // defaultAccounts() binds every account to the first daemon, which leaves
      // the accounts list's daemon filter with nothing to narrow and its Daemon
      // column showing one value. The first four rows reproduce
      // defaultAccounts() so the existing account stills keep their promised
      // evidence — a Codex row, an undeterminable-usage row whose Usage cell is
      // an em dash, and the saturated row that proves the widest Usage string —
      // then add a disabled row for the dormant treatment the still's
      // description promises, and one on the Standby daemon so the filter flow
      // has a second option and a row set to narrow to.
      //
      // reset*InSeconds are offsets FROM NOW (see E2eAccount in
      // services/web/tests/e2e/fakes/api.ts); mid-bucket values keep the
      // rendered countdown stable across capture drift.
      accounts: [
        {
          id: 'acct-claude-work',
          provider: 'claude',
          label: 'work@anthropic.com',
          status: 'active',
          health: 'ok',
          // Checked and clean, with an age. BOS-1142: the reassurance is dated,
          // so it cannot be confused with the never-checked row below.
          authCheck: { outcome: 'healthy', checkedSecondsAgo: 10800 },
          tier: 'max',
          daemonId: 'daemon-proof',
          util5h: 0.42,
          util7d: 0.18,
          reset5hInSeconds: 16200,
          reset7dInSeconds: 302400,
        },
        {
          id: 'acct-codex-personal',
          provider: 'codex',
          label: 'personal',
          status: 'active',
          // health is deliberately still 'ok': nothing has attempted a refresh
          // since the live check started failing, which is exactly the stale
          // state BOS-1142 stops rendering as a dominant green row. The Health
          // pill must fan in the check and read Failed anyway.
          health: 'ok',
          authCheck: {
            outcome: 'auth_invalid',
            failureClass: 'auth_invalidated',
            checkedSecondsAgo: 900,
          },
          tier: 'pro',
          daemonId: 'daemon-proof',
          util5h: 0.1,
          util7d: 0.05,
          reset5hInSeconds: 9000,
          reset7dInSeconds: 561600,
        },
        {
          id: 'acct-claude-undeterminable',
          provider: 'claude',
          label: 'ops@anthropic.com',
          status: 'active',
          health: 'ok',
          daemonId: 'daemon-proof',
          util5h: 0,
          util7d: 0,
          usageStatus: 'RATE_LIMIT_PLAN_STATUS_UNSUPPORTED',
          reset5hInSeconds: 16200,
        },
        {
          id: 'acct-claude-saturated',
          provider: 'claude',
          label: 'saturated@anthropic.com',
          status: 'active',
          health: 'ok',
          // A check that could not be EVALUATED (no smoke runner wired) — not a
          // rejected credential. BOS-881: this row must render its own state
          // rather than borrowing either the clean row's or the failed row's.
          authCheck: {
            outcome: 'unavailable',
            failureClass: 'runner_unavailable',
            checkedSecondsAgo: 300,
          },
          tier: 'max',
          daemonId: 'daemon-proof',
          util5h: 1,
          util7d: 1,
          reset5hInSeconds: 3240,
          reset7dInSeconds: 82800,
        },
        {
          // The dormant-row treatment web-accounts-list promises. defaultAccounts()
          // has no disabled row, so before this fixture owned the list that claim
          // had nothing behind it.
          id: 'acct-claude-retired',
          provider: 'claude',
          label: 'retired@anthropic.com',
          status: 'disabled',
          health: 'ok',
          tier: 'max',
          daemonId: 'daemon-proof',
          util5h: 0,
          util7d: 0.12,
          reset5hInSeconds: 10800,
          reset7dInSeconds: 432000,
        },
        {
          id: 'acct-claude-standby',
          provider: 'claude',
          label: 'standby@anthropic.com',
          status: 'active',
          health: 'ok',
          tier: 'max',
          daemonId: 'daemon-proof-standby',
          util5h: 0.6,
          util7d: 0.31,
          reset5hInSeconds: 12600,
          reset7dInSeconds: 216000,
        },
      ],
      // BOS-656: each repository is attributed to a DIFFERENT daemon so the
      // repository-settings Daemon column and its header filter have something
      // to show and narrow. Without an explicit daemonId the fake binds every
      // repo to the first online daemon.
      repos: [{
        id: 'repo-proof',
        displayName: 'Proof repository',
        daemonId: 'daemon-proof',
        setupScript: 'pnpm install',
        canAutoMerge: true,
        canAutoMergeDependabot: true,
        canAutoRepair: true,
        archiveSessionsAfterMerge: true,
        sentryOrg: 'proof',
        hasLinearKey: true,
        hasSentryKey: true,
      }, {
        id: 'repo-proof-web',
        displayName: 'Proof web repository',
        daemonId: 'daemon-proof-standby',
        setupScript: 'pnpm install',
        canAutoMerge: true,
      }],
    };
  });
${attachStageScript(recipe)}
${notificationStageScript(recipe)}
${organizationStageScript(recipe)}
${sessionOrganizationStageScript(recipe)}
${cronOrganizationStageScript(recipe)}
${repositoryOrganizationStageScript(recipe)}
${subscribeStageScript(recipe)}
${accountsProbeStageScript(recipe)}
${sessionExpiredStageScript(recipe)}
${sessionsDaemonFailureStageScript(recipe)}
${orgDeleteRefusedStageScript(recipe)}
${repoOrganizationRefusalStageScript(recipe)}
${sessionsReconnectingStageScript(recipe)}
${accountsColdStartStageScript(recipe)}
${accountsGiveUpStageScript(recipe)}
`
}

// Recipes whose subject is the sessions-list ORGANIZATION filter (BOS-1070).
//
// TWO organizations, and the fixture's sessions attributed across them, for the
// same reason the shared fixture seeds TWO daemons and TWO repositories: a
// filter only has something to narrow when the fixture spans more than one, and
// FilterSelect renders NOTHING at all for a caller with fewer than two
// organizations -- so a single-organization fixture would silently drop the
// third drop-down and every still promising it would capture the two-drop-down
// row instead, with its own toBeVisible() gate satisfied by the wrong element.
//
// Recipe-SCOPED rather than folded into the shared fixture above, on purpose:
//   - the shared web fixture is asserted to carry no `organizationId` for any
//     recipe that has not opted in (proof-playwright-runner.test.mjs pins that
//     the auth-organization opt-in below stays recipe-scoped, and a substring
//     check cannot tell the two same-named fields apart).
// Declared as a function for the same temporal-dead-zone reason as
// organizationStageScript below.
//
// Deliberately writes only `window.bossanovaE2e`, unlike organizationStageScript
// below, which mirrors into `window.__BOSSANOVA_E2E__` as well. The api fake
// resolves `__BOSSANOVA_E2E__ ?? bossanovaE2e`, so mirroring only THIS staging
// would be worse than not mirroring: the fake would resolve the mirror and find
// organizations but none of the daemons, repositories, or sessions the shared
// web fixture above wrote to `bossanovaE2e` alone. This staging is exactly as
// visible as the fixture it extends, which is the correct coupling.
//
// The organization ids here are Bossanova organization ids (Session.organization_id),
// NOT the WorkOS id organizationStageScript seeds -- these recipes need no
// signed-in organization, only a caller with two memberships.
function sessionOrganizationStageScript(recipe) {
  const stagedRecipeIds = [
    // Both .sub-header crops promise three drop-downs.
    'web-sessions-filters',
    'web-sessions-filters-mobile',
    // The organization filter's own flow.
    'web-sessions-org-filter-flow',
    'web-daemons-org-attribution',
    'web-daemons-org-filter',
  ]
  if (!stagedRecipeIds.includes(recipe?.id)) {
    return ''
  }
  return `
  await page.addInitScript(() => {
    // Attribution mirrors the daemon split, so the organization filter narrows
    // to exactly the row the daemon filter narrows to: everything on the Proof
    // daemon belongs to Acme, and the Standby daemon's session to Globex.
    const orgBySessionId = { 'sess-e2e-quick': 'org-proof-globex' };
    const orgByDaemonId = { 'daemon-proof-standby': 'org-proof-globex' };
    const fixture = window.bossanovaE2e ?? {};
    window.bossanovaE2e = {
      ...fixture,
      organizations: [
        { id: 'org-proof-acme', workosOrgId: 'workos-proof-acme', name: 'Acme', memberCount: 2 },
        { id: 'org-proof-globex', workosOrgId: 'workos-proof-globex', name: 'Globex', memberCount: 2 },
      ],
      sessions: (fixture.sessions ?? []).map((session) => ({
        ...session,
        organizationId: orgBySessionId[session.id] ?? 'org-proof-acme',
      })),
      daemons: (fixture.daemons ?? []).map((daemon) => ({
        ...daemon,
        organizationId: orgByDaemonId[daemon.id] ?? 'org-proof-acme',
      })),
    };
  });`
}

// Recipes proving the cron list's cross-organization attribution and shared
// organization filter (BOS-1158). Kept recipe-scoped so existing cron proofs
// retain their original single-organization fixture shape.
function cronOrganizationStageScript(recipe) {
  const stagedRecipeIds = ['web-cron-org-filter-flow', 'web-cron-org-filter-row-attribution']
  if (!stagedRecipeIds.includes(recipe?.id)) {
    return ''
  }
  return `
  await page.addInitScript(() => {
    const orgByCronId = { 'cron-standby-cache': 'org-proof-globex' };
    const fixture = window.bossanovaE2e ?? {};
    window.bossanovaE2e = {
      ...fixture,
      organizations: [
        { id: 'org-proof-acme', workosOrgId: 'workos-proof-acme', name: 'Acme', memberCount: 2 },
        { id: 'org-proof-globex', workosOrgId: 'workos-proof-globex', name: 'Globex', memberCount: 2 },
        { id: 'org-proof-empty', workosOrgId: 'workos-proof-empty', name: 'Umbrella', memberCount: 1 },
      ],
      cronJobs: (fixture.cronJobs ?? []).map((job) => ({
        ...job,
        organizationId: orgByCronId[job.id] ?? 'org-proof-acme',
      })),
    };
  });`
}

// Repository-list proof needs two memberships because FilterSelect renders no
// organization control for a single membership. Stamp the same origins the API
// fake synthesizes so the column and its shared filter map exercise real holder
// values instead of false-greening on two blank cells.
function repositoryOrganizationStageScript(recipe) {
  const stagedRecipeIds = [
    'web-repositories',
    'web-repositories-daemon-filter-flow',
    'web-repositories-mobile',
  ]
  if (!stagedRecipeIds.includes(recipe?.id)) return ''
  return `
  await page.addInitScript(() => {
    const fixture = window.bossanovaE2e ?? {};
    window.bossanovaE2e = {
      ...fixture,
      organizations: [
        { id: 'org-proof-acme', workosOrgId: 'workos-proof-acme', name: 'Acme', memberCount: 2 },
        { id: 'org-proof-globex', workosOrgId: 'workos-proof-globex', name: 'Globex', memberCount: 2 },
        { id: 'org-proof-initech', workosOrgId: 'workos-proof-initech', name: 'Initech', memberCount: 2 },
      ],
      repoOrganizations: {
        'https://github.com/e2e/repo-proof': 'org-proof-acme',
        'https://github.com/e2e/repo-proof-web': 'org-proof-globex',
      },
    };
  });`
}

// stageFixtureScript emits the ONE dual-global staging convention. Every stage
// script below that mirrors into both fixture globals routes through here, so
// the convention itself is stated here and restated nowhere else. Two kinds of
// stage script are outside that set and stay outside it: the ones that write
// `bossanovaE2e` alone (sessionOrganizationStageScript above argues that case
// for itself, and mirroring them would be actively wrong), and the ones that
// stage no fixture global at all.
//
// The app fakes resolve their fixture as `window.__BOSSANOVA_E2E__ ??
// window.bossanovaE2e` (services/web/tests/e2e/fakes/api.ts,
// fakes/authkit-react.tsx). Because that is `??` and not a merge, a
// bossanovaE2e-only write is invisible to any page where the other global is
// already installed -- so the emitted script writes both, guarding the mirror on
// the global already existing. The proof runner never installs it today, so the
// mirror is latent rather than load-bearing; it is written anyway to keep a
// future caller that does install it from turning these into silently unstaged
// captures.
//
// `stagedFields` is the SOURCE TEXT of the object literal, not a runtime object:
// these functions emit JavaScript into a generated Playwright spec, and several
// callers build their literal by interpolation or from a binding the prologue
// declares -- a JSON.stringify of a real object could not express those. Pass a
// multi-line literal with its own newlines and indentation when the emitted
// spec should carry them.
//
// `prologue` carries statements that must run INSIDE the same init script before
// `const staged`. It is concatenated into emitted source, so feed it only
// module-local literals from this file; do not widen it to recipe-derived input
// without escaping (see the Set-not-object-literal note on
// accountsProbeStageScript about a recipe id named `constructor`).
//
// Each call emits its OWN `addInitScript` arrow, which is what keeps `const
// staged` scoped per arrow when two stage scripts apply to the same recipe. Do
// not collapse these into one shared init script.
//
// A function declaration, not a const: run() executes at module top level, so a
// const below a call site would sit in its temporal dead zone. A declaration
// hoists and is safe wherever it is placed.
function stageFixtureScript(stagedFields, { prologue = '' } = {}) {
  return `
  await page.addInitScript(() => {${prologue}
    const staged = ${stagedFields};
    window.bossanovaE2e = { ...window.bossanovaE2e, ...staged };
    if (window.__BOSSANOVA_E2E__) {
      window.__BOSSANOVA_E2E__ = { ...window.__BOSSANOVA_E2E__, ...staged };
    }
  });`
}

// subscribeStageScript stages the two fixture fields the subscribe CTA copy
// branches on. They are only meaningful together, and staging the second one
// alone is the trap this function exists to close.
//
// The server resolves trial eligibility only for the states whose CTA copy can
// branch on it (services/bosso/internal/server/billing.go), and the fake
// mirrors that gate: every other state leaves the field UNSPECIFIED. The shared
// web fixture leaves cloudAccessState unset, which the fake resolves to ACTIVE
// -- a state that shows no checkout CTA -- so an unstaged subscribe recipe is
// answered UNSPECIFIED no matter which eligibility it asked for. isTrialEligible()
// requires an affirmative ELIGIBLE, so the eligible and the ineligible recipe
// then render the SAME non-trial copy: two proofs, byte-identical, one of them
// showing the opposite of what its own description promises.
//
// So every subscribe CTA recipe stages needs_subscription -- the state a real
// visitor to /subscribe is in -- and only then does the ineligible opt-in below
// reach the branch that consults it.
function subscribeStageScript(recipe) {
  // Staged EXPLICITLY rather than left to the fake's ACTIVE default, for the
  // same reason the eligibility strings below are explicit: the default is what
  // a future change is free to move, and this capture is proof OF the active
  // state, not of whatever the fixture happens to seed.
  if (SUBSCRIBE_ACTIVE_RECIPE_IDS.has(recipe?.id)) {
    return stageFixtureScript(`{ cloudAccessState: 'active' }`)
  }
  if (!SUBSCRIBE_CTA_RECIPE_IDS.has(recipe?.id)) return ''
  // Only the recipe whose whole subject IS a spent trial asks for ineligible.
  // The others ask for eligible explicitly rather than leaning on the fake's
  // default, so a change to that default cannot silently retitle their
  // evidence -- these two strings are what the captures are proof OF.
  const eligibility = recipe.id === 'web-subscribe-trial-used' ? 'ineligible' : 'eligible'
  return stageFixtureScript(
    `{ cloudAccessState: 'needs_subscription', cloudTrialEligibility: '${eligibility}' }`,
  )
}

// accountsProbeStageScript cuts ONE daemon out of the accounts page live usage
// probe (BOS-1088). The page fans the probe out over every online daemon; before
// the fix a single cancelled leg threw away every other daemon's completed work,
// stopped the spinner, and surfaced an interruption notice over stale numbers.
//
// The shared web fixture seeds TWO online daemons, and only the standby one is
// staged to fail, so the capture shows the recovering shape: the daemon-proof
// rows -- 'work@anthropic.com' and its Usage column -- survive the fan-out and
// the interrupted notice never appears. Failing BOTH would prove the opposite
// case and is what the unit tests pin; the video is proof of the recovery.
//
// Recipe-SCOPED: a cancelled probe leg in the shared fixture would degrade every
// other web recipe's accounts view for no reason.
function accountsProbeStageScript(recipe) {
  // Declared INSIDE the function: run() executes at module top level, so a const
  // hoisted to module scope below this call site would be in its temporal dead
  // zone. A Set rather than an object literal so a recipe id like `constructor`
  // cannot reach Object.prototype.
  const stagedRecipeIds = new Set(['web-accounts-refresh-interrupted'])
  if (!stagedRecipeIds.has(recipe?.id)) return ''
  return stageFixtureScript(
    `{ accountsProbeErrors: { 'daemon-proof-standby': { code: 1, message: 'This operation was aborted' } } }`,
  )
}

// sessionExpiredStageScript stages the mid-session re-authentication state
// (BOS-1085). The real trigger is AuthKitProvider's `onRefreshFailure`, which
// fires only for a refresh that begins AFTER initialization -- something the
// VITE_E2E fake never does, since it reports a signed-in user unconditionally
// and hands out a fixed token. So the fake provider fires the callback once on
// mount when this flag is staged (services/web/tests/e2e/fakes/authkit-react.tsx),
// which latches src/lib/sessionExpiry.ts and makes Layout render the notice in
// place of the whole app.
//
// Recipe-SCOPED, and it has to be: the flag replaces the entire app with the
// notice, so staging it in the shared web fixture above would blank out every
// other web recipe's subject.
//
// The mirror stageFixtureScript writes is latent on every recipe today, but this
// is the one whose failure would be loud if that ever changed: this recipe's arm
// of captureReadyScript below waits for the notice itself before the screenshot,
// so an unstaged run fails the spec instead of photographing a healthy
// signed-in app.
//
// Declared inside the function rather than as a module-level const, for the
// temporal-dead-zone reason documented at the top of this module.
function sessionExpiredStageScript(recipe) {
  const stagedRecipeIds = new Set(['web-session-expired'])
  if (!stagedRecipeIds.has(recipe?.id)) return ''
  return stageFixtureScript(`{ authRefreshFailed: true }`)
}

// orgDeleteRefusedStageScript refuses the organization delete so the reason can
// be photographed. It is the only way to capture BOS-1154's refusal state: every
// blocker the real handler enforces -- remaining members, remaining sessions, an
// entitled cloud subscription -- lives in bosso, and the fake API answers the
// settings page without any of that machinery, so the delete would otherwise
// always succeed and navigate the capture off the page it is meant to show.
//
// Code 9 (FAILED_PRECONDITION) rather than an internal error, because that is
// the code the handler actually returns for a non-empty organization, and the
// message is the handler's own wording (services/bosso/internal/server/organizations.go)
// -- a refusal scene whose copy no server can emit is evidence of nothing.
//
// Recipe-SCOPED by id, like every other stage script here: staging it in the
// shared web fixture would break the delete for any future recipe capturing the
// success path.
//
// Declared inside the function for the temporal-dead-zone reason documented at
// the top of this module.
function orgDeleteRefusedStageScript(recipe) {
  const stagedRecipeIds = new Set(['web-org-delete-refused'])
  if (!stagedRecipeIds.has(recipe?.id)) return ''
  return stageFixtureScript(`{
      errors: {
        deleteOrganization: {
          message: 'this organization still has sessions -- delete them before deleting it',
          code: 9,
        },
      },
    }`)
}

// sessionsDaemonFailureStageScript fails the sessions page's DAEMON-options
// poll, which is the only way to photograph BOS-1091's subject: the page mounts
// two pollers and renders one connection notice, and the give-up state is what
// puts the notice's error text and its `Try again` control on screen. The
// sessions read itself stays healthy, because the whole point of the still is
// that the table survives underneath.
//
// The fake API consults `errors[method]` before answering (see
// services/web/tests/e2e/fakes/api.ts), and its listDaemons arm is wired to
// that check, so staging the failure is all this needs.
//
// Code 13 (INTERNAL) rather than a transient code, deliberately: the page's
// retry ladder only gives up on a NON-transient failure (see
// services/web/src/lib/connectRetry.ts), and a transient one would leave the
// capture racing ~15s of backoff to photograph a notice that says
// "Reconnecting…" instead of the error this recipe is evidence of.
//
// Recipe-SCOPED by id, like every other stage script here: staging it in the
// shared web fixture would empty the daemon filter on every other web recipe.
//
// Declared inside the function for the temporal-dead-zone reason documented at
// the top of this module.
function sessionsDaemonFailureStageScript(recipe) {
  const stagedRecipeIds = new Set(['web-sessions-daemons-give-up'])
  if (!stagedRecipeIds.has(recipe?.id)) return ''
  return stageFixtureScript(`{
      errors: { listDaemons: { message: 'Daemon list unavailable', code: 13 } },
    }`)
}

// repoOrganizationRefusalStageScript refuses the repo-edit page's organization
// WRITE, which is the only way to photograph BOS-1114's subject: the control
// renders the server's refusal, and the whole point of the change is which
// sentence it renders for a refusal the server cannot itself explain.
//
// The fake API consults `errors[method]` before answering (see
// services/web/tests/e2e/fakes/api.ts, whose setRepoOrganization arm is wired
// to that check), so staging the failure is all this needs. Only the SET is
// staged: the read has to keep answering or the field never learns the repo is
// unmapped, and a staged clear would refuse a release this recipe never drives.
//
// Code 7 is PERMISSION_DENIED, and the code is the whole point. The classifier
// keys on it and on nothing else -- a different code falls through to the
// server's own message, which is the pre-change behaviour and the opposite of
// what this capture is evidence of. Pinned as a number because that is what the
// fake reads.
//
// The message is bosso's real non-membership prose, deliberately. It is the
// string the old code rendered verbatim, so a capture that still showed it
// would be a capture of the defect.
//
// Recipe-SCOPED by id, like every other stage script here: staged in the shared
// web fixture it would refuse the write on every other repo-settings recipe.
//
// Declared inside the function for the temporal-dead-zone reason documented at
// the top of this module.
function repoOrganizationRefusalStageScript(recipe) {
  const stagedRecipeIds = new Set(['web-repository-organization-refusal'])
  if (!stagedRecipeIds.has(recipe?.id)) return ''
  return stageFixtureScript(`{
      errors: {
        setRepoOrganization: { message: 'organization membership required', code: 7 },
      },
    }`)
}

// sessionsReconnectingStageScript stages the RECONNECTING pill on the sessions
// list (BOS-1093) -- the state the daemon give-up recipe above is the other
// half of. There the poll has given up; here the ladder is still running.
//
// `hangSessionsReadAfter: 1` lets the first read answer and leaves every later
// one unanswered. Both halves matter. The first answer is what paints the table
// and sets `lastSuccessAt`, without which `combineIndicators` folds the age to
// null (it is null when EITHER poller's is) and the pill loses its "Showing data
// from ..." clause -- half the subject. And leaving the read HUNG rather than
// rejecting it is what makes the state hold still: see the budget below.
//
// Only the sessions read is staged. The daemon poll keeps answering, so the
// fold's `lastSuccessAt` stays pinned to the sessions read's one success, which
// is the older of the two and the honest thing for the clause to report.
//
// BUDGET. The state is stable but not permanent, and the arithmetic is
// auditable rather than approximate. POLL_INTERVAL (5s) to the first hung
// attempt, then ATTEMPT_TIMEOUT_MS (10s) before it fails its deadline -- so the
// pill appears at ~15s. From there usePolledResource walks MAX_RETRY_ATTEMPTS
// (4) more rungs, each burning another 10s deadline, separated by
// computeBackoffMs sleeps of 1s, 2s, 4s and 8s at +/-25% jitter: ~55s more
// before it gives up and the pill is replaced by the give-up alert. The
// captureReadyScript gate below waits out the first 15s, which leaves ~50s of
// slack for the capture. If this recipe ever starts photographing a give-up
// alert, that ladder running to completion is the first thing to check.
//
// The dark theme is pinned rather than inherited. `DEFAULT_THEME` in
// src/lib/theme.ts is 'dark' today, so an unpinned capture would look right by
// accident -- and would silently retitle itself the day that default changes,
// which is exactly what this still is proof about. Written to localStorage
// because index.html's pre-paint script reads it there before first paint;
// `emulateMedia` covers the media-query half in case the attribute is ever
// dropped.
//
// Declared inside the function for the temporal-dead-zone reason documented at
// the top of this module.
//
// The localStorage write is a PROLOGUE: it has to run inside the same init
// script, before first paint, so it goes through stageFixtureScript. The
// emulateMedia call does not -- it is a Playwright call rather than init-script
// source, so it is prepended to the helper's output here.
function sessionsReconnectingStageScript(recipe) {
  const stagedRecipeIds = new Set(['web-sessions-reconnecting-dark'])
  if (!stagedRecipeIds.has(recipe?.id)) return ''
  return (
    `
  await page.emulateMedia({ colorScheme: 'dark' });` +
    stageFixtureScript(`{ hangSessionsReadAfter: 1 }`, {
      prologue: `
    try { localStorage.setItem('bossanova.theme', 'dark'); } catch { /* storage disabled */ }`,
    })
  )
}

// accountsColdStartStageScript stages the accounts page's cold start together
// with a usage probe that fails on it (BOS-1089).
//
// TWO fields, and neither is optional. `hangPassiveAccountsRead` keeps the
// PASSIVE read unanswered, which is what "cold start" means on this page --
// nothing has ever loaded, so there is no snapshot behind an error and the
// full-page branch is live. `accountsUsageRefreshUnsupported` makes the probe's
// own leg answer the way a bosso too old to honour should_refresh does, so the
// page raises its compatibility rejection. Stage only the first and the capture
// is a plain spinner; only the second and the page has a table on screen, which
// is the non-cold-start path this recipe is not about.
//
// The shared `errors` map cannot express this: it keys failure injection on the
// method name alone, so it would fail the passive read and the probe together.
// That is the gap noted in services/web/tests/e2e/fakes/api.ts.
//
// Recipe-SCOPED: the hang leaves the accounts table permanently unloaded, so
// staging it in the shared web fixture would replace every accounts capture's
// subject with a spinner.
//
// Declared inside the function rather than as a module-level const, for the
// temporal-dead-zone reason documented at the top of this module.
function accountsColdStartStageScript(recipe) {
  const stagedRecipeIds = new Set(['web-accounts-cold-start-probe-failure'])
  if (!stagedRecipeIds.has(recipe?.id)) return ''
  return stageFixtureScript(
    `{ hangPassiveAccountsRead: true, accountsUsageRefreshUnsupported: true }`,
  )
}

// accountsGiveUpStageScript makes the accounts page's give-up notice reachable
// (BOS-1090). The notice is what `usePolledResource` renders once a read has
// stopped answering, so the capture needs a daemon read that SUCCEEDS first --
// a table has to be on screen for the notice to sit above -- and then fails.
// `errors[method]` in the fake is read fresh on every call
// (services/web/tests/e2e/fakes/api.ts), so an accessor property on that object
// is enough to change the answer mid-recipe without touching the fake.
//
// The arming condition is a call count AND an elapsed time, but the two halves
// are NOT symmetric, and which one is load-bearing is the thing to know before
// touching either:
//
//   - the ELAPSED half carries the guard. `firstReadAt` is seeded from the
//     page's own first read (below), so the clock measures time since that
//     read rather than since boot. That is what holds off React StrictMode's
//     double-invoked effect: in a development build the mount can spend TWO
//     reads back to back, and the second already satisfies `reads >= 2` while
//     the recipe has clicked nothing -- only the elapsed check still answers
//     it. Delete this half and the capture opens on a page that has already
//     given up: no healthy table, no transition, and the video still exits
//     green.
//   - the COUNT half is belt-and-braces, not an independent guard. Because
//     `firstReadAt` is seeded INSIDE the getter, immediately before the
//     comparison, read #1 always measures an elapsed of 0 and is always
//     answered however slow the boot -- so `reads < 2` blocks only a read the
//     elapsed half has already blocked. It is kept as a floor that does not
//     depend on a wall clock (`Date.now()` can jump) and that survives someone
//     lowering ARM_AFTER_MS, not because it covers a case of its own today.
//
// What the pair exists to prevent is the page's FIRST read failing: that fails
// the initial LOAD, which lands on the cold-start branch -- an EmptyState with
// its own retry, a different component from the ConnectionNotice this recipe is
// proof of. `captureReadyScript` gates on a fixture ROW for exactly that
// substitution.
//
// TIMING COUPLING, and it runs both ways: ARM_AFTER_MS (2000) has to stay
// BELOW the dwell `web-accounts-give-up-retry` spends on the loaded table
// before it clicks refresh -- the `"action": "wait", "timeoutMs": 3000`
// "Connection drops" step in proof/recipes/default.json. Trim that dwell under
// 2s and the refresh click's read is still answered, so the notice never
// appears and the step waiting for it times out on a healthy page. The recipe
// schema is `unevaluatedProperties: false` (proof/recipes/schema.json), so the
// coupling cannot be written into the JSON beside the number it constrains;
// the test "the accounts give-up arming delay stays under the recipe's own
// dwell" in proof-playwright-runner.test.mjs pins the two against each other
// instead, and that is what a maintainer trimming the dwell will actually hit.
//
// `listDaemons` is the read to fail because it is the one call that propagates:
// `fetchAccountsSnapshot` runs the per-daemon account reads through
// `Promise.allSettled` and folds a rejection into an empty row set, so failing
// those would produce an empty table rather than a notice. It is read once per
// `fetchAccountsSnapshot` and every entry point runs one -- the initial load,
// the 30s passive poll, the refresh probe, and the Try-again reload -- which is
// what makes a read COUNT a usable arming input at all: the mount spends read
// #1 (and read #2 as well, under StrictMode), and the step-4 refresh click is
// the read that arms.
//
// The failure this stages is TERMINAL, not retryable, and that is a property of
// the route rather than of the code below. `fetchAccountsSnapshot` catches every
// rejection and re-throws `errorMessage(err)` as a plain STRING
// (AccountsSettings.tsx), so the wire code is gone before `isTransient`
// (src/lib/connectRetry.ts) ever sees it: `ConnectError.from(aString)` is
// `Code.Unknown` with a non-TypeError cause, which that predicate rejects. No
// ladder is armed and no "Reconnecting…" pill is raised on this route -- which
// is why the recipe films the notice PERSISTING across a failed retry rather
// than leaving and returning. `web-chat-terminal-reconnecting` is the recipe
// that films the pill. Code 14 is kept because it is the honest wire code for a
// dropped daemon connection and it is what the notice's own message is derived
// from; it is not load-bearing for the retry classification.
//
// The accessor is a PROLOGUE: `staged` here is `{ errors }`, a reference to the
// binding those statements declare, so they have to run inside the same init
// script ahead of it. stageFixtureScript takes them as such.
function accountsGiveUpStageScript(recipe) {
  const stagedRecipeIds = new Set(['web-accounts-give-up-retry'])
  if (!stagedRecipeIds.has(recipe?.id)) return ''
  return stageFixtureScript(`{ errors }`, {
    prologue: `
    const ARM_AFTER_MS = 2000;
    let reads = 0;
    let firstReadAt = 0;
    const errors = {};
    Object.defineProperty(errors, 'listDaemons', {
      enumerable: true,
      get() {
        reads += 1;
        if (firstReadAt === 0) firstReadAt = Date.now();
        if (reads < 2 || Date.now() - firstReadAt < ARM_AFTER_MS) return undefined;
        return { code: 14, message: 'daemon connection lost' };
      },
    });`,
  })
}

// organizationStageScript stages a signed-in WorkOS organization so the pages
// and controls that only exist once useAuth() reports one have something to
// render from. /settings/organization early-returns its "No active organization"
// empty state without one, and the shared web fixture deliberately leaves
// organizationId unset. A recipe that needs an organization therefore says so by
// entering on an organization-scoped route; one that forgets photographs the
// empty state instead of its subject.
//
// There is no longer an id-based opt-in beside that route test. It existed for
// the two recipes that needed an organization while entering at `/` -- both of
// them subjects of the header organization switcher, which no longer exists. Its
// last user, web-org-create-modal, now enters at the settings page that owns the
// New organization button, so the route test covers it.
//
// The fake's defaultOrganizations() derives its workosOrgId from whatever is
// staged, so organizationId is the only field that has to be staged here.
//
// Membership is the only load-bearing part; which id is staged is not. The
// fake derives workosOrgId from whatever is staged, so the switcher's label
// reads the same for any non-empty id -- hence one shared constant rather than
// a per-recipe value. `workos-e2e` merely echoes the fake's own fallback.
//
// A Set of ids rather than an object literal keyed by id, so the lookup cannot
// reach Object.prototype -- an id like `constructor` satisfies
// validRecipeIdPattern and would otherwise resolve.
//
// Both are declared inside the function rather than as module-level consts:
// `run()` executes at module top level (see the `if (invokedDirectly)` block
// above), so a const declared down here is still in its temporal dead zone when
// the first recipe is staged by a direct CLI invocation.
//
// The list only has to name recipes that need an organization WITHOUT visiting
// an org-scoped route. An organization-settings recipe is detected from its own
// steps by usesOrgScopedRoute below, so adding one cannot silently photograph
// the guard's "Switching to ..." spinner instead of the page.
//
// Every entry names a live recipe.
// ORG_SCOPED_ROUTE is declared with the other staging payloads at the top of the
// module, above the direct-invocation block, for the temporal-dead-zone reason
// documented there. This function is a declaration, so it hoists on its own.
function usesOrgScopedRoute(recipe) {
  // Surface-gated because the shape is not unique to the app: the docs site has
  // a `/reference/settings` page that matches the same pattern and has no
  // organization to stage.
  if (recipe?.surface !== 'web') return false
  const routes = [recipe?.route, ...(recipe?.steps ?? []).map((step) => step?.route)]
  return routes.some((route) => typeof route === 'string' && ORG_SCOPED_ROUTE.test(route))
}

function organizationStageScript(recipe) {
  const stagedOrganizationId = 'workos-e2e'
  if (!usesOrgScopedRoute(recipe)) return ''
  const organizationId = stagedOrganizationId
  // A few recipes need the caller to belong to TWO organizations: a picker with
  // a single entry photographs nothing worth photographing, and its promised
  // switch has nowhere to go. Every other staged recipe keeps the fake's
  // single-organization default, whose ids this list has to repeat because
  // staging `organizations` replaces that default rather than extending it --
  // `org-e2e` and the workosOrgId staged above are what the fake derives, and
  // what every scoped `/org-e2e/settings/...` route in the recipes resolves.
  const organizations = TWO_ORGANIZATION_RECIPE_IDS.has(recipe.id)
    ? [
        {
          id: 'org-e2e',
          workosOrgId: organizationId,
          name: 'E2E Organization',
          callerRole: 1,
          memberCount: 2,
        },
        {
          id: 'org-proof-beta',
          workosOrgId: 'workos-proof-beta',
          name: 'Beta Robotics',
          callerRole: 1,
          memberCount: 2,
        },
      ]
    : null
  const stagedFields = organizations
    ? `{ organizationId: '${organizationId}', organizations: ${JSON.stringify(organizations)} }`
    : `{ organizationId: '${organizationId}' }`
  return stageFixtureScript(stagedFields)
}

// captureReadyScript emits an extra readiness gate for recipes whose promised
// evidence lands *after* their capture selector becomes visible.
//
// web-chat-terminal crops to [data-testid='chat-terminal-canvas'], which xterm
// mounts as soon as the route renders — before the staged attach socket's
// kind=0 data frame has been parsed and painted. `toBeVisible()` is therefore
// satisfied by an empty pane, and the still could be captured with none of the
// glyphs its recipe description promises. Wait for the row text itself, the
// same way services/web/tests/e2e/specs/chat-terminal.spec.ts does.
//
// The subscribe recipes are the same shape: .subscribe-actions renders
// immediately, but the CTA stays disabled and its copy stays fail-closed until
// the cloud-access status RPC answers, so an unwaited still can carry a faded
// button above copy the verdict is about to replace.
//
// web-session-expired is the same shape once more, and its subject arrives from
// an EFFECT rather than from the first render — as does the DAEMON give-up
// notice gated further below, which cannot appear until a poll has failed. (The
// accounts give-up gate below it is the exception that proves the rule: its
// subject also arrives from an effect, but its gate deliberately waits for the
// HEALTHY pre-state instead, for the reason written on that clause.) The fake
// provider fires `onRefreshFailure` in a mount effect, which latches
// src/lib/sessionExpiry.ts and only then re-renders Layout into the notice.
// `page.goto` resolves at `load`, a commit earlier, and this recipe declares
// neither `selector` nor `cropToSelector` — so without an explicit wait the
// generated spec carries no visibility assertion at all and screenshots
// whatever happened to be painted. Both failure modes are silent in the same
// direction: a lost race, or staging that stopped landing, captures a healthy
// signed-in app and still exits green, proving the opposite of what the recipe
// description claims.
//
// Only meaningful when the web fixture is staged (the glyph line comes from
// attachStageScript, the eligibility answer from the fixture API), so callers
// gate this on stageEnv.
function captureReadyScript(recipe) {
  if (SUBSCRIBE_CTA_RECIPE_IDS.has(recipe?.id)) {
    return `
  await expect(page.locator('.subscribe-cta')).toBeEnabled({ timeout: 10000 });`
  }
  // The redirect recipe's evidence is a NEGATIVE plus a destination, and both
  // halves have to be asserted. `page.goto('/subscribe')` resolves at `load`,
  // one commit before the status RPC answers, so an unwaited capture
  // photographs the pre-verdict /subscribe render -- which looks like a page
  // that never redirected. The absent offer is the fix; the sessions table is
  // where the user actually landed.
  if (SUBSCRIBE_ACTIVE_RECIPE_IDS.has(recipe?.id)) {
    return `
  await expect(page.locator('.data-table-wrap')).toBeVisible({ timeout: 10000 });
  await expect(page.locator('.subscribe-actions')).toHaveCount(0);`
  }
  if (recipe?.id === 'web-session-expired') {
    return `
  await expect(page.getByText('Session expired')).toBeVisible({ timeout: 10000 });`
  }
  // The give-up notice arrives from a poll that has to fail first, so without
  // this gate the screenshot races it and photographs a healthy page.
  if (recipe?.id === 'web-sessions-daemons-give-up') {
    return `
  await expect(page.getByRole('button', { name: 'Try again' })).toBeVisible({ timeout: 10000 });`
  }

  // The reconnecting pill is the opposite race to the give-up gate above: it
  // arrives only after a poll has been left hanging AND its 10s deadline has
  // expired, ~15s in, so the timeout has to clear that -- the 10s used above
  // would expire on a still-healthy-looking page every single run.
  //
  // Asserted as TEXT INSIDE the live region, not as the region's visibility:
  // ConnectionNotice renders `[data-testid="connection-status"]` unconditionally
  // and empty, so `toBeVisible` on it is satisfied at first paint and would
  // photograph the healthy list -- green, proving the opposite of what the
  // recipe description claims.
  //
  // Both clauses are asserted because they fail independently. "Reconnecting…"
  // says the ladder is running; "Showing data from" says the pill actually grew
  // its staleness clause, which is the half that dies silently.
  //
  // A first-call hang is NOT what that second clause catches. With no successful
  // read behind it, Sessions returns its loading view instead of the notice
  // (`lastSuccessAt === null && read.status === 'reconnecting'`), so no pill is
  // rendered at all and BOTH locators time out -- the recipe fails loudly on the
  // first clause. What the second clause catches is a reconnecting pill whose
  // clause never arrives: exactly the shape a regression in `useEpisodeStartedAt`
  // produces, where a null episode clock renders "Reconnecting…" by itself.
  if (recipe?.id === 'web-sessions-reconnecting-dark') {
    return `
  const pill = page.locator('[data-testid="connection-status"]');
  await expect(pill).toContainText('Reconnecting…', { timeout: 25000 });
  await expect(pill).toContainText('Showing data from', { timeout: 25000 });`
  }

  // The subject only exists after a CLICK, and a still spec has no steps -- the
  // step list is a video-recipe affordance (buildVideoSpec). So the interaction
  // lives here, in the one hook the still path gives between the navigation and
  // the screenshot, bracketed by the two waits that keep it from racing: the
  // spinner must be up before the click (the control is in the page header and
  // is clickable while the body is still loading, so clicking early probes a
  // page whose fixture has not been installed), and the compatibility notice
  // must be painted before the capture.
  //
  // The trailing wait is also what makes an unstaged run LOUD. Without it a
  // fixture that stopped landing would photograph a healthy accounts table and
  // the spec would still pass; with it, the spec fails instead.
  //
  // The state it photographs is stable but not permanent. The passive read is
  // hung, not answered, so usePolledResource walks its retry ladder and
  // eventually gives up -- at which point the read owns the error and the page
  // legitimately becomes a full-page panel. The budget is auditable rather than
  // approximate: MAX_RETRY_ATTEMPTS (4) retries after the first attempt, each
  // bounded by ATTEMPT_TIMEOUT_MS (10s), separated by computeBackoffMs sleeps of
  // 1s, 2s, 4s and 8s at +/-25% jitter -- 5*10 + 15 = ~65s end to end, ~61-69s
  // once the jitter is taken at its extremes. Take ~61s as the budget, since
  // the low end is the one that bites.
  //
  // On the happy path the screenshot lands seconds in, but that is not the
  // margin to plan against: the gates below are allowed to burn their timeouts
  // and still pass. Worst case the two explicit `{ timeout: 10000 }` waits take
  // 10s each, the two added assertions take Playwright's default 5s expect
  // timeout each, and the click awaits the probe RPC on top -- ~30s plus the
  // click before the capture. So the slack that is actually GUARANTEED is
  // ~30s, not most of a minute. Still ample; not so ample that a loaded runner
  // can be ignored. If this recipe ever starts capturing a danger panel, that
  // ladder running to completion before the capture is the first thing to
  // check -- and the durable fix is to make the fake suppress the passive
  // read's terminal give-up too, so no wall-clock race exists at all.
  //
  // What holds the notice up across that window is the page's own probeError,
  // not the poll's: each rung of the ladder dispatches `reconnecting`, which
  // NULLS the poll error, so a capture keyed on the poll's error rather than on
  // the notice text would be racing the first rung instead of the last.
  if (recipe?.id === 'web-accounts-cold-start-probe-failure') {
    // The last two assertions are the ones that make this a REGRESSION gate
    // rather than a screenshot. Waiting for the compat notice alone cannot tell
    // the fixed page from the broken one: under BOS-1089 the cold-start branch
    // titled its full-page danger panel with the folded error, which for an
    // unsuperseded probe failure is that same "update bosso and try again"
    // string — and `getByText` with a plain string is a SUBSTRING match, so it
    // matches that title just as happily. The `Loading accounts` wait above the
    // click cannot cover it either: it is sequenced BEFORE the click, so it
    // says nothing about what survives the probe.
    //
    // So assert the two things the recipe's own description promises and the
    // regressed page cannot satisfy: the spinner is still up AFTER the notice
    // paints (the panel replaces the <AccountsView> subtree that owns it), and
    // the notice arrived through the INLINE route rather than the full-page
    // panel.
    //
    // An alert census used to be the second discriminator, on the reasoning
    // that EmptyState sets `role="alert"` when `intent="danger"` while this
    // page's inline notice stayed at `role="status"`. BOS-1090 retired that:
    // the give-up notice now takes `role="alert"` on EVERY page, because it
    // mounts already populated and a polite region is generally not announced
    // for its initial content. `pageOwnsStatusRole` hands only the
    // RECONNECTING PILL back to the page's convention, so both the fixed and
    // the regressed page now carry exactly one `.settings-content
    // [role="alert"]` and counting them proves nothing.
    //
    // The live region is the discriminator that survives it: ConnectionNotice
    // renders `[data-testid="connection-status"]` unconditionally, and the
    // cold-start branch renders no ConnectionNotice at all — it is the whole
    // subtree the danger panel replaces. Scoped to `.settings-content` so
    // nothing in the surrounding chrome can make the gate flap.
    return `
  await expect(page.getByText('Loading accounts')).toBeVisible({ timeout: 10000 });
  await page.locator('button[aria-label="Refresh account usage"]').click();
  await expect(page.getByText('update bosso and try again')).toBeVisible({ timeout: 10000 });
  await expect(page.getByText('Loading accounts')).toBeVisible();
  await expect(page.locator('.settings-content [data-testid="connection-status"]')).toHaveCount(1);`
  }

  if (recipe?.id === 'web-accounts-give-up-retry') {
    // The gate here waits for the HEALTHY pre-state, not for the give-up notice
    // the recipe is about: this clause rides the goto, and at that point the
    // notice does not exist yet -- it only appears after the refresh click,
    // several steps later, where the recipe's own `wait` step asserts it.
    //
    // What it is guarding is the half of accountsGiveUpStageScript that fails
    // silently. If the arming condition ever let the page's FIRST read fail,
    // the route would render the cold-start EmptyState instead of a table, and
    // every later step would still find a `[role="alert"]` and a "Try again" to
    // click -- a green video of the wrong component. Naming a fixture row makes
    // that substitution loud, because the cold-start branch renders no rows at
    // all.
    return `
  await expect(page.getByText('work@anthropic.com').first()).toBeVisible({ timeout: 10000 });`
  }
  if (recipe?.id !== 'web-chat-terminal') return ''
  return `
  await expect(page.locator('.xterm-rows').first()).toContainText(new RegExp(${JSON.stringify(
    CHAT_TERMINAL_GLYPH_ROW_SOURCE,
  )}), { timeout: 10000 });`
}

// attachStageScript makes the attach socket deterministic per recipe. Most
// captures need a healthy attached_clients frame so the terminal mounts; the
// reconnecting recipe closes it to exercise the transient reconnect state; the
// chat-terminal captures additionally replay a raw kind=0 data frame so the
// captured canvas carries visible terminal output instead of an empty pane —
// the page only clears its "Connecting…" overlay on the first data byte, so a
// capture without one shows a disconnected terminal; the upload recipe
// additionally answers the BOS-661 upload frames.
function attachStageScript(recipe) {
  if (recipe?.id === 'web-chat-terminal-reconnecting') {
    return `
  await page.routeWebSocket('**/ws/attach*', (ws) => {
    ws.close();
  });`
  }
  const initialData = CHAT_TERMINAL_DATA_REPLAY_IDS.has(recipe?.id)
    ? `
    const payload = Buffer.from(${JSON.stringify(CHAT_TERMINAL_GLYPH_LINE)}, 'utf-8');
    const header = Buffer.from([0, (payload.length >>> 16) & 0xff, (payload.length >>> 8) & 0xff, payload.length & 0xff]);
    ws.send(Buffer.concat([header, payload]));`
    : ''
  return `
  await page.routeWebSocket('**/ws/attach*', (ws) => {
    ws.send(Buffer.from([4, 0, 0, 2, 91, 93]));${initialData}${uploadServerScript(recipe)}${echoServerScript(recipe)}
  });${uploadFileChooserScript(recipe)}`
}

// echoServerScript makes the staged socket echo kind=0 data frames back, the
// way a real PTY does. Without it a paste is invisible: xterm never echoes
// locally — Terminal.paste() emits through onData, the page forwards the bytes,
// and only the far end's echo paints them. So the BOS-879 capture's final
// "text landed in the terminal" assertion has nothing to observe unless the
// fixture plays the PTY's part. Scoped to the paste recipe so no other
// capture's canvas gains unexpected content.
export function echoServerScript(recipe) {
  if (recipe?.id !== 'web-chat-terminal-paste') return ''
  return `
    ws.onMessage((message) => {
      const buf = Buffer.isBuffer(message) ? message : Buffer.from(message);
      // u8 kind | u24 len | payload — kind 0 is client input; ignore resize.
      if (buf.length < 5 || buf[0] !== 0) return;
      const payload = buf.subarray(4);
      const header = Buffer.from([0, (payload.length >>> 16) & 0xff, (payload.length >>> 8) & 0xff, payload.length & 0xff]);
      ws.send(Buffer.concat([header, payload]));
    });`
}

// uploadServerScript answers the browser's BOS-661 upload frames the way bosso
// + bossd do: record the declared size from kind=6 upload_start, acknowledge
// every kind=7 chunk with a kind=10 ack carrying the running byte count, then
// answer kind=8 upload_finish with the single terminal kind=11 result whose ok
// flag drives the 'Upload complete' banner. Wire format mirrors
// services/bosso/internal/server/ws_attach_upload.go — every integer is
// unsigned big-endian and every frame is `u8 kind | u24 len | payload`.
//
// The ok flag MUST stay conditional. An unconditional 0x01 makes the whole
// web-chat-terminal-upload video vacuous: a browser regression that sent
// upload_start + upload_finish and ZERO chunks — or short, duplicated or
// out-of-order chunks — would still paint 'Upload complete' and the capture
// would pass. So this responder holds the same three facts bosso holds
// (declared size, cumulative bytes, next expected seq) and fails the upload
// when they disagree, which turns the recipe's final wait into a timeout.
// Exported so proof-playwright-runner.test.mjs can drive the snippet against a
// fake socket and assert the failure paths, not just the happy one.
export function uploadServerScript(recipe) {
  if (recipe?.id !== 'web-chat-terminal-upload') return ''
  return `
    // Per-upload id: { declared, received, nextSeq }. Keyed rather than a
    // single counter so a second upload on the same socket starts clean.
    const __uploads = new Map();
    const __frame = (kind, payload) => {
      const header = Buffer.from([kind, (payload.length >>> 16) & 0xff, (payload.length >>> 8) & 0xff, payload.length & 0xff]);
      ws.send(Buffer.concat([header, payload]));
    };
    // u64 BE. Number is exact to 2^53, far above the 500 MiB upload cap.
    const __u64 = (buf, off) => buf.readUInt32BE(off) * 4294967296 + buf.readUInt32BE(off + 4);
    // flags=0x00: ok=0, can_retry=0. Every failure below is a protocol
    // violation by the sender, not transport loss, so a retry is pointless.
    const __fail = (id, message) => {
      __frame(11, Buffer.concat([id, Buffer.from([0x00]), Buffer.from(message, 'utf-8')]));
    };
    ws.onMessage((message) => {
      const buf = Buffer.isBuffer(message) ? message : Buffer.from(message);
      if (buf.length < 5) return;
      const kind = buf[0];
      if (kind !== 6 && kind !== 7 && kind !== 8) return; // ignore data/resize/cancel traffic
      const body = buf.subarray(4);
      const idLen = body[0];
      if (idLen === 0 || body.length < 1 + idLen) return;
      const id = body.subarray(0, 1 + idLen); // u8 idLen | id
      const key = id.toString('latin1');
      if (kind === 6) {
        // u8 idLen | id | u64 size_bytes | u16 filenameLen | filename
        if (body.length < 11 + idLen) return;
        __uploads.set(key, { declared: __u64(body, 1 + idLen), received: 0, nextSeq: 0 });
        return;
      }
      const state = __uploads.get(key);
      if (!state) {
        // A chunk or finish with no start (or after this upload already
        // settled) is exactly the regression the capture must not survive.
        __fail(id, 'unknown upload id');
        return;
      }
      if (kind === 7) {
        if (body.length < 9 + idLen) return;
        const seq = body.subarray(1 + idLen, 9 + idLen);
        if (__u64(body, 1 + idLen) !== state.nextSeq) {
          __uploads.delete(key);
          __fail(id, 'out-of-order chunk: expected seq ' + state.nextSeq);
          return;
        }
        state.nextSeq += 1;
        state.received += body.length - (9 + idLen);
        if (state.received > state.declared) {
          __uploads.delete(key);
          __fail(id, 'received more than the declared ' + state.declared + ' bytes');
          return;
        }
        const acked = Buffer.alloc(8);
        acked.writeUInt32BE(Math.floor(state.received / 4294967296), 0);
        acked.writeUInt32BE(state.received >>> 0, 4);
        __frame(10, Buffer.concat([id, seq, acked]));
        return;
      }
      // kind === 8 upload_finish: the single terminal frame for this id.
      __uploads.delete(key);
      if (state.received !== state.declared) {
        __fail(id, 'incomplete upload: received ' + state.received + ' of ' + state.declared + ' bytes');
        return;
      }
      // flags=0x01 (ok) with an EMPTY error_message: the wire spec says the
      // message is empty when ok, and this fixture is a reference for it.
      __frame(11, Buffer.concat([id, Buffer.from([0x01])]));
    });`
}

// uploadFileChooserScript answers the native file picker the upload button
// opens. Clicking the control is the affordance the video has to show, and the
// control's only job is to click the hidden <input type=file> — which Chromium
// reports as a filechooser event, the one hook Playwright gives us for it.
function uploadFileChooserScript(recipe) {
  if (recipe?.id !== 'web-chat-terminal-upload') return ''
  return `
  page.on('filechooser', async (chooser) => {
    await chooser.setFiles({
      name: ${JSON.stringify(CHAT_UPLOAD_FILENAME)},
      mimeType: 'text/plain',
      buffer: Buffer.from(${JSON.stringify(CHAT_UPLOAD_CONTENT)}, 'utf-8'),
    });
  });`
}

// notificationStageScript makes the BOS-492 notification surfaces deterministic
// per recipe (the browser Notification API's permission is otherwise
// environment-dependent and the priming modal mounts app-wide):
//   - the priming-modal and subscribe-gate recipes force permission 'default'
//     and leave the modal un-dismissed so regressions stay visible;
//   - the settings recipe forces permission 'denied' so the "blocked" note shows,
//     and pre-dismisses the modal so it does not overlay the settings card;
//   - every other web recipe pre-dismisses the modal so it never overlays the
//     captured surface.
function notificationStageScript(recipe) {
  const id = recipe?.id ?? ''
  if (id === 'web-notification-permission-modal' || id === 'web-subscribe-no-notification-prompt') {
    return notificationInitScript('default', false)
  }
  if (id === 'web-notification-settings') {
    return notificationInitScript('denied', true)
  }
  return dismissNotificationPromptScript()
}

function notificationInitScript(permission, dismissPrompt) {
  const dismiss = dismissPrompt
    ? "try { localStorage.setItem('bossanova.notificationPromptDismissed', 'true'); } catch (e) {}"
    : ''
  return `
  await page.addInitScript(() => {
    class FakeNotification {
      static permission = ${JSON.stringify(permission)};
      static async requestPermission() { return 'granted'; }
      constructor(title, options) { this.title = title; this.options = options || {}; }
      close() {}
    }
    window.Notification = FakeNotification;
    ${dismiss}
  });`
}

function dismissNotificationPromptScript() {
  return `
  await page.addInitScript(() => {
    try { localStorage.setItem('bossanova.notificationPromptDismissed', 'true'); } catch (e) {}
  });`
}
