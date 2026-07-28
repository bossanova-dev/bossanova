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
const VIDEO_ACTIONS = new Set(['goto', 'click', 'type', 'wait', 'scroll', 'press'])
const DEFAULT_VIDEO_SLOWMO_MS = 350

// Only drive Playwright when invoked directly; importing this module (e.g. from
// the unit tests, to exercise buildSpec/validateRecipe) must not start a run.
const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)

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
  const testTitle = JSON.stringify(`proof screenshot: ${recipe.id}`)

  return `
import { expect, test } from '@playwright/test';

${collectProofAuditTextScript()}

test(${testTitle}, async ({ page }) => {
  ${stageWeb}
  await page.setViewportSize(${viewport});
  const response = await page.goto(${route});
  expect(response?.status(), 'proof route status').toBeLessThan(400);
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
        // BOS-541: mix agent names across chats so ChatListPanel renders the
        // has-agent-column phone layout — a single agent name (or none)
        // never exercises the fixed delete-button column at 390px.
        chats: [
          { id: 'chat-1', agentSessionId: 'claude-1', title: 'Proof chat', status: 'idle', agentName: 'claude' },
          { id: 'chat-e2e-2', agentSessionId: 'codex-e2e-2', title: 'A much longer chat title that should wrap onto a second line at 390px', status: 'idle', agentName: 'codex' },
          { id: 'chat-e2e-3', agentSessionId: 'claude-e2e-3', title: 'Another chat', status: 'idle', agentName: 'claude' },
        ],
      }, {
        // A Quick-Chat session with no PR: the header must show New chat/Archive
        // but NO Merge button and NO "Switch account" control (BOS-365 items 1 & 3).
        id: 'sess-e2e-quick',
        title: 'Proof quick chat',
        branchName: 'proof/quick-chat',
        baseBranch: 'main',
        repoId: 'repo-proof',
        repoDisplayName: 'bossanova',
        chats: [{ id: 'chat-2', agentSessionId: 'claude-2', title: 'Quick chat', status: 'idle' }],
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
        repoId: 'repo-proof',
        repoDisplayName: 'bossanova',
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
      // Seed a daemon-local repository so repository settings proof flows can
      // open its form and confirmation dialog without relying on a live daemon.
      daemons: [{ id: 'daemon-proof', displayName: 'Proof daemon' }],
      repos: [{
        id: 'repo-proof',
        displayName: 'Proof repository',
        setupScript: 'pnpm install',
        canAutoMerge: true,
        canAutoMergeDependabot: true,
        canAutoRepair: true,
        archiveSessionsAfterMerge: true,
        sentryOrg: 'proof',
        hasLinearKey: true,
        hasSentryKey: true,
      }],
    };
  });
  await page.routeWebSocket('**/ws/attach*', (ws) => {
    ws.send(Buffer.from([4, 0, 0, 2, 91, 93]));
  });
${notificationStageScript(recipe)}
`
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
