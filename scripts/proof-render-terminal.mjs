#!/usr/bin/env node

import fs from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { formatCaption, trimTerminalBlankLines } from './proof-lib.mjs'

if (import.meta.url === `file://${process.argv[1]}`) {
  const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))
  const require = createRequire(path.join(repoRoot, 'services/web/package.json'))
  const { chromium } = require('@playwright/test')

  const args = parseArgs(process.argv.slice(2))
  const text = trimTerminalBlankLines(fs.readFileSync(args.input, 'utf8'))

  const browser = await chromium.launch()
  try {
    const page = await browser.newPage({
      viewport: { width: 1400, height: 900 },
      deviceScaleFactor: 1,
    })
    await page.setContent(renderHtml({ title: args.title, text, caption: args.caption }))
    await page.locator('[data-proof-terminal]').screenshot({ path: args.output })
  } finally {
    await browser.close()
  }
}

function parseArgs(argv) {
  const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))
  const parsed = {}
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i]
    const value = argv[i + 1]
    if (!key?.startsWith('--') || !value) {
      throw new Error(`invalid argument near ${key ?? '<end>'}`)
    }
    parsed[key.slice(2)] = value
  }
  for (const required of ['input', 'output', 'title']) {
    if (!parsed[required]) {
      throw new Error(`missing --${required}`)
    }
  }
  parsed.input = resolveRepoPath(parsed.input, repoRoot)
  parsed.output = resolveRepoPath(parsed.output, repoRoot)
  fs.mkdirSync(path.dirname(parsed.output), { recursive: true })
  parsed.caption = parsed.caption ?? ''
  return parsed
}

function resolveRepoPath(value, repoRoot) {
  return path.isAbsolute(value) ? value : path.join(repoRoot, value)
}

export function renderHtml({ title, text, caption = '' }) {
  // Bound the caption to a single line <=140 chars (AC#4). formatCaption('' or
  // whitespace-only) === '', so the truthiness check still omits the bar.
  const formattedCaption = formatCaption(caption)
  const captionBar = formattedCaption
    ? `<div class="__proof-tui-caption" style="background:#1d4ed8;color:#fff;font:600 14px/1.5 sans-serif;padding:6px 14px;">${escapeHtml(formattedCaption)}</div>`
    : ''
  return `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<style>
  :root {
    color-scheme: dark;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    background: #0a0d13;
  }
  body {
    margin: 0;
    padding: 24px;
    background: #0a0d13;
  }
  [data-proof-terminal] {
    width: max-content;
    min-width: 1120px;
    overflow: hidden;
    border: 1px solid #293241;
    border-radius: 8px;
    background: #05070b;
    color: #e6edf3;
    box-shadow: 0 18px 60px rgb(0 0 0 / 45%);
  }
  .titlebar {
    display: flex;
    gap: 8px;
    align-items: center;
    height: 36px;
    padding: 0 14px;
    background: #111827;
    border-bottom: 1px solid #293241;
    color: #9ca3af;
    font: 13px ui-sans-serif, system-ui, sans-serif;
  }
  .dot {
    width: 10px;
    height: 10px;
    border-radius: 999px;
    background: #ef4444;
  }
  .dot:nth-child(2) { background: #f59e0b; }
  .dot:nth-child(3) { background: #22c55e; }
  .label { margin-left: 8px; }
  pre {
    margin: 0;
    padding: 18px;
    white-space: pre;
    font-size: 14px;
    line-height: 1.25;
  }
</style>
<body>
  <section data-proof-terminal>
    <div class="titlebar">
      <span class="dot"></span><span class="dot"></span><span class="dot"></span>
      <span class="label">${escapeHtml(title)}</span>
    </div>${captionBar}
    <pre>${escapeHtml(text)}</pre>
  </section>
</body>
</html>`
}

function escapeHtml(value) {
  return String(value)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
}
