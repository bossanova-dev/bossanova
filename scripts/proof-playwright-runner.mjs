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
const CHAT_TERMINAL_DATA_REPLAY_IDS = new Set(['web-chat-terminal', 'web-chat-terminal-upload'])

// BOS-661 staged upload file for web-chat-terminal-upload. Small on purpose:
// one chunk fits inside a single kind=7 frame, so the capture never depends on
// the ack window draining. The name is what the completion banner echoes.
const CHAT_UPLOAD_FILENAME = 'agent-brief.txt'
const CHAT_UPLOAD_CONTENT = 'Fixture upload for the BOS-661 chat file upload proof.\n'

// Only drive Playwright when invoked directly; importing this module (e.g. from
// the unit tests, to exercise buildSpec/validateRecipe) must not start a run.
import { isMainModule } from '../skills-toolbox/main-module.mjs'

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
    auditText = await page.locator('body').evaluate(collectProofAuditText);
    const size = page.viewportSize() ?? ${viewport};
    const height = Math.min(size.height, Math.ceil(box.y + box.height + 24));
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
      const stillBlock = `  {
    const __h = await captureStill(page, ${outPath}, ${cropToSelector}, ${viewport});
    __stills.push({ fileName: ${fileNameJson}, label: ${labelJson} });
    if (__h === null) __disableCrop = true;
    else if (!__disableCrop) __cropHeight = Math.max(__cropHeight ?? 0, __h);
  }`
      return `${action}\n${stillBlock}`
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

function collectProofAuditTextScript() {
  return `
function collectProofAuditText(element) {
  const values = [];
  const push = (value) => {
    const text = String(value ?? '').trim();
    if (text) values.push(text);
  };
  const visit = (node) => {
    push(node.textContent);
    push(node.getAttribute?.('aria-label'));
    push(node.getAttribute?.('title'));
    push(node.getAttribute?.('alt'));
    if ('value' in node) push(node.value);
    if ('placeholder' in node) push(node.placeholder);
  };
  visit(element);
  for (const node of element.querySelectorAll?.('input, textarea, select, option, img, [aria-label], [title], [alt]') ?? []) {
    visit(node);
  }
  return values.join('\\n');
}
`
}

function webStageScript(recipe) {
  return `
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
          health: 'ok',
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
`
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
// Only meaningful when the web fixture is staged (the glyph line comes from
// attachStageScript), so callers gate this on stageEnv.
function captureReadyScript(recipe) {
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
    ws.send(Buffer.from([4, 0, 0, 2, 91, 93]));${initialData}${uploadServerScript(recipe)}
  });${uploadFileChooserScript(recipe)}`
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
//   - the priming-modal recipe forces permission 'default' and leaves the modal
//     un-dismissed so it renders;
//   - the settings recipe forces permission 'denied' so the "blocked" note shows,
//     and pre-dismisses the modal so it does not overlay the settings card;
//   - every other web recipe pre-dismisses the modal so it never overlays the
//     captured surface.
function notificationStageScript(recipe) {
  const id = recipe?.id ?? ''
  if (id === 'web-notification-permission-modal') {
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
