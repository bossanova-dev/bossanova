#!/usr/bin/env node

import { spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { validateBrowserRoute } from './proof-lib.mjs';

const validSurfaces = new Set(['web', 'marketing']);
const validRecipeIdPattern = /^[a-z0-9][a-z0-9-]*$/;
const VIDEO_ACTIONS = new Set(['goto', 'click', 'type', 'wait']);

// Only drive Playwright when invoked directly; importing this module (e.g. from
// the unit tests, to exercise buildSpec/validateRecipe) must not start a run.
const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);

if (invokedDirectly) {
  try {
    run(process.argv.slice(2));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}

function run(argv) {
  const args = parseArgs(argv);
  const recipe = JSON.parse(fs.readFileSync(args.recipe, 'utf8'));
  validateRecipe(recipe);
  fs.mkdirSync(args['output-dir'], { recursive: true });

  const specDir = path.join(serviceSpecRoot(args.surface), `proof-${process.pid}`);
  fs.mkdirSync(specDir, { recursive: true });
  const specPath = path.join(specDir, 'proof.spec.ts');
  fs.writeFileSync(
    specPath,
    buildSpec({ recipe, outputDir: path.resolve(args['output-dir']), surface: args.surface }),
  );

  let cleaned = false;
  const cleanup = () => {
    if (!cleaned) {
      cleaned = true;
      fs.rmSync(specDir, { recursive: true, force: true });
    }
  };

  const child = spawn('pnpm', ['exec', 'playwright', 'test', specPath, '--project=chromium'], {
    cwd: process.cwd(),
    stdio: 'inherit',
    env: {
      ...process.env,
      VITE_E2E: args.surface === 'web' ? '1' : process.env.VITE_E2E,
    },
  });

  for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
    process.once(signal, () => {
      child.kill(signal);
      cleanup();
      process.exit(1);
    });
  }

  child.on('error', (error) => {
    cleanup();
    console.error(error.message);
    process.exitCode = 1;
  });

  child.on('exit', (code) => {
    cleanup();
    process.exitCode = code ?? 1;
  });
}

function parseArgs(argv) {
  const parsed = {};
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i];
    const value = argv[i + 1];
    if (!key?.startsWith('--') || !value) {
      throw new Error(`invalid argument near ${key ?? '<end>'}`);
    }
    parsed[key.slice(2)] = value;
  }
  for (const required of ['surface', 'recipe', 'output-dir']) {
    if (!parsed[required]) {
      throw new Error(`missing --${required}`);
    }
  }
  if (!validSurfaces.has(parsed.surface)) {
    throw new Error(`invalid --surface: ${parsed.surface}`);
  }
  return parsed;
}

export function validateRecipe(recipe) {
  if (!validRecipeIdPattern.test(String(recipe.id ?? ''))) {
    throw new Error(`invalid recipe id: ${recipe.id ?? '<missing>'}`);
  }
  if (recipe.capture === 'video') {
    if (!Array.isArray(recipe.steps) || recipe.steps.length === 0) {
      throw new Error('video recipe requires a non-empty steps array');
    }
    for (const step of recipe.steps) {
      if (!VIDEO_ACTIONS.has(step.action)) {
        throw new Error(`unsupported video step action: ${step.action ?? '<missing>'}`);
      }
      if (step.action === 'goto') {
        if (typeof step.route !== 'string' || step.route.length === 0) {
          throw new Error('video goto step requires route');
        }
        validateBrowserRoute(step.route);
      } else if (step.action === 'click') {
        requireVideoStepString(step, 'selector');
      } else if (step.action === 'type') {
        requireVideoStepString(step, 'selector');
        requireVideoStepString(step, 'value');
      } else if (step.action === 'wait' && step.selector !== undefined) {
        requireVideoStepString(step, 'selector');
      }
    }
    return;
  }
  validateBrowserRoute(recipe.route);
}

function requireVideoStepString(step, field) {
  if (typeof step[field] !== 'string' || step[field].length === 0) {
    throw new Error(`video ${step.action} step requires ${field}`);
  }
}

function serviceSpecRoot(surface) {
  return path.resolve(surface === 'marketing' ? 'tests/e2e' : 'tests/e2e/specs');
}

export function buildSpec({ recipe, outputDir, surface }) {
  if (recipe.capture === 'video') {
    return buildVideoSpec({ recipe, outputDir, surface });
  }
  const fileName = `${recipe.id}.png`;
  const target = JSON.stringify(path.join(outputDir, fileName));
  const auditTarget = JSON.stringify(path.join(outputDir, 'audit.txt'));
  const route = JSON.stringify(recipe.route);
  const selector = JSON.stringify(recipe.selector ?? '');
  const cropToSelector = JSON.stringify(recipe.cropToSelector ?? '');
  const viewport = JSON.stringify(recipe.viewport ?? { width: 1440, height: 1000 });
  const fullPage = Boolean(recipe.fullPage);
  const stageWeb = surface === 'web' ? webStageScript() : '';
  const testTitle = JSON.stringify(`proof screenshot: ${recipe.id}`);

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
`;
}

function buildVideoSpec({ recipe, outputDir, surface }) {
  const webmPath = JSON.stringify(path.join(outputDir, `${recipe.id}.webm`));
  const posterPath = JSON.stringify(path.join(outputDir, `${recipe.id}.png`));
  const auditTarget = JSON.stringify(path.join(outputDir, 'audit.txt'));
  const outputDirJson = JSON.stringify(outputDir);
  const viewport = JSON.stringify(recipe.viewport ?? { width: 1440, height: 1000 });
  const stageWeb = surface === 'web' ? webStageScript() : '';
  const testTitle = JSON.stringify(`proof video: ${recipe.id}`);
  const stepLines = recipe.steps.map(renderVideoStep).join('\n');

  // Video records at the context level, so this spec owns its own context (the
  // shared `page` fixture cannot be told to record). The webm finalizes only on
  // context.close(); we screenshot a poster first, then rename the finalized
  // file to a stable <id>.webm.
  return `
import { expect, test } from '@playwright/test';
import fs from 'node:fs';

${collectProofAuditTextScript()}

test(${testTitle}, async ({ browser, baseURL }) => {
  const context = await browser.newContext({
    baseURL,
    viewport: ${viewport},
    recordVideo: { dir: ${outputDirJson}, size: ${viewport} },
  });
  const page = await context.newPage();
  ${stageWeb}
  await page.setViewportSize(${viewport});
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
});
`;
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
    .replace(/\u2029/g, '\\u2029')}'`;
}

// Unroll one video step into an inline statement (values baked in, not a
// runtime loop) so the generated spec is self-contained and inspectable.
function renderVideoStep(step) {
  switch (step.action) {
    case 'goto':
      return `  {
    const response = await page.goto(${jsString(step.route)});
    expect(response?.status(), 'proof route status').toBeLessThan(400);
  }`;
    case 'click':
      return `  await page.locator(${jsString(step.selector)}).first().click();`;
    case 'type':
      return `  await page.locator(${jsString(step.selector)}).first().pressSequentially(${jsString(step.value)});`;
    case 'wait':
      return step.selector
        ? `  await expect(page.locator(${jsString(step.selector)}).first()).toBeVisible();`
        : `  await page.waitForTimeout(${Number(step.timeoutMs) || 500});`;
    default:
      throw new Error(`unsupported video step action: ${step.action ?? '<missing>'}`);
  }
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
`;
}

function webStageScript() {
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
        chats: [{ id: 'chat-1', agentSessionId: 'claude-1', title: 'Proof chat', status: 'idle' }],
      }],
    };
  });
  await page.routeWebSocket('**/ws/attach*', (ws) => {
    ws.send(Buffer.from([4, 0, 0, 2, 91, 93]));
  });
`;
}
