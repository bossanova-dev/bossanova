#!/usr/bin/env node

// plan-image-guard — deterministic, LLM-untrusted guard that fails loud when boss-plan's
// rewritten Linear description drops any image the reporter embedded in the original (BOS-378).
//
// Linear does not expose description history to agents, so a "helpful" summary that paraphrases
// an inline `![](https://uploads.linear.app/…)` into `[screenshot: …]` text destroys the only copy
// of the URL. boss-plan Phase 4 runs this as a hard pre-write gate: it compares the set of image
// URLs in the original description against the rewritten one and aborts the write-back (no
// half-planned issue) when any image was dropped.
//
// Node builtins only — this runs in dependency-light cron worktrees, matching plan-upload.mjs.

import { readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// Inline markdown image: `![alt](dest)`, where dest may carry a title (`url "t"`) or be
// angle-bracketed (`<url>`). We capture the raw destination and clean it below.
const INLINE_IMAGE = /!\[[^\]]*\]\(\s*([^)]+?)\s*\)/g
// HTML <img …src="url"> — double OR single quoted.
const HTML_IMG_SRC = /<img\b[^>]*?\bsrc\s*=\s*(?:"([^"]*)"|'([^']*)')/gi
// Bare Linear upload/attachment asset URL in prose. Stop at whitespace and the quote/bracket/paren
// characters that fence markdown/HTML so surrounding syntax is never captured.
const BARE_LINEAR_URL = /https?:\/\/uploads\.linear\.app\/[^\s)<>\]"']*/gi

// Trailing prose/markdown punctuation to trim off a captured URL. Query strings are preserved.
const TRAILING_PUNCT = /[)\].,]+$/

function cleanInlineDestination(raw) {
  const s = String(raw).trim()
  const angle = /^<([^>]*)>/.exec(s)
  if (angle) return angle[1].trim()
  // Strip an optional markdown title: the destination is everything up to the first whitespace.
  return s.split(/\s+/)[0]
}

function normalizeUrl(url) {
  return String(url).trim().replace(TRAILING_PUNCT, '')
}

// Ordered, de-duplicated list of every image URL in document order.
function extractOrdered(markdown) {
  const md = String(markdown ?? '')
  const hits = []
  const push = (index, value) => {
    const url = normalizeUrl(value)
    if (url) hits.push({ index, url })
  }
  for (const m of md.matchAll(INLINE_IMAGE)) push(m.index, cleanInlineDestination(m[1]))
  for (const m of md.matchAll(HTML_IMG_SRC)) push(m.index, m[1] ?? m[2])
  for (const m of md.matchAll(BARE_LINEAR_URL)) push(m.index, m[0])
  hits.sort((a, b) => a.index - b.index)
  const seen = new Set()
  const ordered = []
  for (const { url } of hits) {
    if (seen.has(url)) continue
    seen.add(url)
    ordered.push(url)
  }
  return ordered
}

// extractImageRefs(markdown) -> Set<string>: every image URL, normalized + de-duplicated.
export function extractImageRefs(markdown) {
  return new Set(extractOrdered(markdown))
}

// findDroppedImages(originalMarkdown, rewrittenMarkdown) -> string[]: URLs present in the original
// but absent from the rewritten (order-stable by original document order, de-duplicated). Direction
// matters — extra images added by the rewrite are never reported.
export function findDroppedImages(originalMarkdown, rewrittenMarkdown) {
  const rewritten = extractImageRefs(rewrittenMarkdown)
  return extractOrdered(originalMarkdown).filter((url) => !rewritten.has(url))
}

export function parseImageGuardArgs(argv) {
  const args = { original: null, rewritten: null }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    if (flag === '--original') {
      args.original = argv[(i += 1)]
    } else if (flag === '--rewritten') {
      args.rewritten = argv[(i += 1)]
    }
  }
  if (!args.original) {
    throw new Error('--original <path> is required')
  }
  if (!args.rewritten) {
    throw new Error('--rewritten <path> is required')
  }
  return args
}

function main() {
  const { original, rewritten } = parseImageGuardArgs(process.argv.slice(2))
  const dropped = findDroppedImages(readFileSync(original, 'utf8'), readFileSync(rewritten, 'utf8'))
  if (dropped.length > 0) {
    for (const url of dropped) {
      console.error(url)
    }
    console.error(
      `plan-image-guard: ${dropped.length} image reference(s) dropped from the rewritten description`,
    )
    process.exitCode = 1
  }
}

const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
if (invokedDirectly) {
  try {
    main()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
