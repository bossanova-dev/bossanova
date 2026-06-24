#!/usr/bin/env node

/**
 * Renders the branded intro card to a PNG via Playwright's bundled Chromium.
 * Ported from wondercanvas-mono/apps/e2e/scripts/render-intro-card.ts.
 *
 *   node scripts/proof-render-intro-card.mjs \
 *     --out <png> --width W --height H --label "<label>" --title "<title>"
 *
 * Must be run with cwd inside a workspace package that has @playwright/test
 * installed (e.g. services/web or services/marketing); proof-lib.mjs
 * introCardCommand() sets this up via `pnpm --dir <serviceDir> exec node`.
 * @playwright/test is resolved relative to cwd (see main()) because this script
 * lives under scripts/ where the dependency is not installed.
 */

import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { buildIntroCardHtml } from './proof-video-intro.mjs';

// Only drive Playwright when invoked directly; importing this module (e.g. from
// tests to verify the invokedDirectly guard) must not launch a browser.
const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);

if (invokedDirectly) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}

async function main(argv) {
  const args = parseArgs(argv);

  // Resolve @playwright/test from the current working directory (the workspace
  // service package that owns the dependency), not from this script's location
  // under scripts/. ESM resolves bare specifiers relative to the importing
  // module's URL, which would walk up to the repo root where the package is not
  // installed; anchoring createRequire at cwd finds services/web's copy.
  const requireFromCwd = createRequire(path.join(process.cwd(), 'package.json'));
  const { chromium } = requireFromCwd('@playwright/test');
  const browser = await chromium.launch();
  try {
    const page = await browser.newPage({
      viewport: { width: args.width, height: args.height },
      deviceScaleFactor: 1,
    });
    await page.setContent(buildIntroCardHtml({ label: args.label, title: args.title }), {
      waitUntil: 'load',
    });
    await page.screenshot({
      path: args.out,
      clip: { x: 0, y: 0, width: args.width, height: args.height },
    });
  } finally {
    await browser.close();
  }
}

function parseArgs(argv) {
  const parsed = {};
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i];
    const value = argv[i + 1];
    if (!key?.startsWith('--') || value === undefined) {
      throw new Error(`invalid argument near ${key ?? '<end>'}`);
    }
    parsed[key.slice(2)] = value;
  }
  for (const required of ['out', 'width', 'height', 'label']) {
    if (!parsed[required]) {
      throw new Error(`missing --${required}`);
    }
  }
  const width = parseInt(parsed.width, 10);
  const height = parseInt(parsed.height, 10);
  if (!width || !height) {
    throw new Error('--width and --height must be positive integers');
  }
  return {
    out: parsed.out,
    width,
    height,
    label: parsed.label,
    title: parsed.title ?? '',
  };
}
