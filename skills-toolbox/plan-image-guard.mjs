#!/usr/bin/env node

// plan-image-guard — deterministic, LLM-untrusted guard that fails loud when boss-plan's
// rewritten Linear description drops any image the reporter embedded in the original.
//
// Linear does not expose description history to agents, so a "helpful" summary that paraphrases
// an inline `![](https://uploads.linear.app/…)` into `[screenshot: …]` text destroys the only copy
// of the URL. boss-plan Phase 4 runs this as a hard pre-write gate: it compares the set of image
// URLs in the original description against the rewritten one and aborts the write-back (no
// half-planned issue) when any image was dropped.
//
// Node builtins only — this runs in dependency-light cron worktrees.

import { readFileSync, realpathSync } from 'node:fs'
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
  const args = { original: null, rewritten: null, allowEmptyOriginal: false }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    if (flag === '--original') {
      args.original = argv[(i += 1)]
    } else if (flag === '--rewritten') {
      args.rewritten = argv[(i += 1)]
    } else if (flag === '--allow-empty-original') {
      args.allowEmptyOriginal = true
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
  const { original, rewritten, allowEmptyOriginal } = parseImageGuardArgs(process.argv.slice(2))
  const originalText = readFileSync(original, 'utf8')
  const rewrittenText = readFileSync(rewritten, 'utf8')

  // Refuse to CERTIFY a comparison we could not meaningfully perform. `findDroppedImages` is right
  // to return [] here — empty-in/empty-out is correct set-difference semantics, and it stays that
  // way — but the CLI is the gate, and a gate that blesses an unverifiable input is not a gate.
  // An empty original is ambiguous between "the upstream extraction broke, so the reporter's images
  // are invisible to us" and "the ticket genuinely has no description"; we see only files, so we
  // cannot tell them apart. Fail closed on the ambiguity and make the benign case an explicit,
  // auditable opt-out — a flag in the transcript — rather than a silent default. This is deliberate:
  // whatever breaks the caller's extraction usually empties BOTH scratch files at once, which is
  // exactly the input the old vacuous pass was most confident about.
  if (!allowEmptyOriginal && originalText.trim() === '') {
    console.error(
      'plan-image-guard: original is empty — cannot verify image parity; ' +
        'pass --allow-empty-original only if the source description is genuinely empty',
    )
    process.exitCode = 1
    return
  }

  const dropped = findDroppedImages(originalText, rewrittenText)
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

// Main-module detection, resolved through symlinks on BOTH sides. Node realpaths the main module
// before setting import.meta.url but leaves process.argv[1] as typed, so a plain string compare is
// false whenever ANY component of the invoking path is a symlink — a symlinked skills dir, or just
// macOS's /tmp -> /private/tmp and /var -> /private/var. The CLI body would then never run and the
// process would exit 0 SILENTLY: catastrophic here, because boss-plan Phase 4 treats exit 0 as
// "no image dropped" and proceeds with the description write-back. This guard must only ever fail
// CLOSED, so compare real paths and fall back to the literal compare only if realpath itself fails.
const invokedDirectly = (() => {
  if (!process.argv[1]) return false
  const self = fileURLToPath(import.meta.url)
  try {
    return realpathSync(process.argv[1]) === realpathSync(self)
  } catch {
    return path.resolve(process.argv[1]) === self
  }
})()
if (invokedDirectly) {
  try {
    main()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
