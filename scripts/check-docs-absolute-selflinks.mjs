#!/usr/bin/env node

// Guardrail: a docs page must link to another docs page RELATIVELY, so
// Docusaurus's `onBrokenLinks: 'throw'` can see the link and check it.
//
// `services/docs/docusaurus.config.ts` sets `onBrokenLinks: 'throw'` and
// `onBrokenMarkdownLinks: 'throw'`, but that gate only resolves links it can
// resolve — relative ones. An absolute `https://docs.bossanova.dev/guides/x`
// pointing at the same site is, as far as the build is concerned, an external
// URL: it is never resolved, never checked, and never red. So a forward link to
// a sibling page that is renamed, moved, or never lands becomes a permanent 404
// that no build, lint, or test notices. Writing the same link as
// `./x.md` puts it back under the gate that already exists.
//
// NECESSARY BUT NOT SUFFICIENT. A green run here proves only that no docs page
// links to the docs site by absolute URL. It does not prove the relative links
// resolve — that is the Docusaurus build's job (`make -C services/docs test`),
// and this gate exists precisely to keep links in front of it.
//
// Scope decisions, all deliberate:
//
//   * The bare site root (`https://docs.bossanova.dev`, with or without a
//     trailing slash) is allowed. It names no page, so there is nothing for
//     `onBrokenLinks` to check and nothing that can 404 — "the docs live at
//     docs.bossanova.dev" is a legitimate sentence. Only a URL carrying a path
//     is a page cross-link.
//
//   * Fenced code blocks are skipped. Markdown inside a fence is never
//     linkified, so a URL there cannot bypass `onBrokenLinks` — it is sample
//     text (a `curl`, a copy-pasteable address), and flagging it would push
//     authors to sprinkle escape-hatch markers through code samples to silence
//     a finding that was never real. Fence tracking follows CommonMark rather
//     than toggling on every ``` line, and an unterminated fence is a hard
//     error — see `scanAbsoluteSelfLinks` for why that direction matters.
//
//     Two fence constructs are knowingly NOT tracked: a fence inside a
//     blockquote (`> ```) and one indented four or more spaces inside a deep
//     list item. Both are absent from the docs tree today, and both fail in the
//     safe direction — a URL inside them is reported, which an author can see
//     and escape-hatch, rather than skipped, which nobody would see. They are
//     listed here so the next reader files them as scope, not as a bug.
//
//   * `<!-- absolute-link: intentional -->` on the offending line, or on the
//     line immediately before it, is the escape hatch for the remaining prose
//     case — a link deliberately written absolute (for a reader who will copy
//     it out of the page, say). It is an HTML comment so it renders as nothing,
//     and it is one fixed string so a reviewer can audit every use with
//     `grep -rn 'absolute-link: intentional' services/docs/docs`.
//
// Exercised by scripts/check-docs-absolute-selflinks.test.mjs and runnable via
// `node scripts/check-docs-absolute-selflinks.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// Docs tree whose links are checked. `DOCS_DIR` is derived from
// `DOCS_SITE_DIR` rather than spelled out again: the two are used as a pair
// below to tell "no docs site in this checkout" from "the docs site is here but
// its tree moved", and two independent literals could drift into a state where
// the site check looks at the old path while the tree check looks at the new
// one — silently restoring the very no-op this gate guards against.
const DOCS_SITE_DIR = path.join('services', 'docs')
const DOCS_DIR = path.join(DOCS_SITE_DIR, 'docs')
const DOC_EXTENSIONS = ['.md', '.mdx']

// The docs site's own origin, matched only when a path follows it. The
// character class stops at whatever ends a URL in Markdown or MDX: whitespace,
// a closing `)` of an inline link, a `]`, a quote around a JSX attribute, or
// the `<>` of an autolink.
//
// Scheme-tolerant and case-insensitive on purpose. `http://docs.bossanova.dev/x`
// and `https://Docs.Bossanova.dev/x` are the same unchecked self-link to a
// reader and to `onBrokenLinks`, so matching only the lowercase `https://`
// spelling would leave a free bypass sitting next to the rule.
const ABSOLUTE_SELF_LINK = /https?:\/\/docs\.bossanova\.dev\/[^\s)\]"'`<>]+/gi

// Trailing sentence punctuation is not part of the URL. Without this a bare
// link ending a sentence is reported as `…/guides/notes.` and the reader has to
// work out which character was prose.
const TRAILING_PUNCTUATION = /[.,;:!?]+$/

// The escape hatch, on the match's own line or the line immediately before it.
const ESCAPE_HATCH = /<!--\s*absolute-link:\s*intentional\s*-->/

// The repo root relative to this file, so the gate works from any cwd — it runs
// from the repo root via `node scripts/check-docs-absolute-selflinks.mjs` and
// from `scripts/` via the scripts Makefile `lint` target.
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// A CommonMark fence opener: three or more backticks or tildes, indented by at
// most three spaces. The delimiter run is captured because closing is not
// symmetric with opening — see `closesFence`.
const FENCE_OPEN = /^ {0,3}(`{3,}|~{3,})/

// A fence closes only on a run of the SAME character, at least as long as the
// opener, with nothing after it but whitespace.
const FENCE_CLOSE_ONLY = /^ {0,3}[`~]+[ \t]*$/

function fenceDelimiter(line) {
  const match = FENCE_OPEN.exec(line)
  return match === null ? null : match[1]
}

function closesFence(line, openDelimiter) {
  const delimiter = fenceDelimiter(line)
  if (delimiter === null) return false
  return (
    delimiter[0] === openDelimiter[0] &&
    delimiter.length >= openDelimiter.length &&
    FENCE_CLOSE_ONLY.test(line)
  )
}

// Split a Markdown/MDX document's absolute self-links into `unmarked` (the
// findings, each with its 1-based line number) and `marked` (a count of the
// escape-hatched ones). Fenced occurrences are neither: they are dropped here
// rather than reported-and-filtered, so a caller cannot count them as findings.
// `unterminatedFence` is the 1-based line of a fence that never closed, or null.
//
// One walk, all three answers. Findings and marked exceptions are decided by
// the same fence tracking and the same escape-hatch lookup, so they are counted
// together: two walks over the same rules would be two places for the fence
// state or the marker's reach to drift apart, and a divergence would show up as
// a miscounted exception rather than a visible error.
//
// FENCE TRACKING IS THE ONE PLACE THIS GATE CAN GO QUIET. Every other edge case
// it gets wrong fails red — a false finding an author can see and argue with.
// Mis-tracked fence state fails the other way: the scanner believes it is
// inside code while the renderer knows it is in prose, and it reads none of the
// rest of the file. So the rule is CommonMark's, not a `^\s*``` ` toggle: a
// toggle flips on an inner ``` inside a ````-delimited block (and never enters
// a ~~~ block at all), silently blinding the gate from there to EOF. And a
// fence that never closes is reported as an error rather than skipped, because
// "the rest of this file was never checked" must be something the run says out
// loud.
export function scanAbsoluteSelfLinks(markdown) {
  const unmarked = []
  // Split on either ending. A lone '\n' split leaves a trailing \r on every line
  // of a CRLF file, and \r is not whitespace to `FENCE_CLOSE_ONLY` — so a fence
  // that is plainly closed reads as unterminated, reddening `make lint` on a
  // valid page with a message that sends the author hunting a bug that is not
  // there. Nothing normalizes line endings on the way in: `.gitattributes`
  // declares no `text`/`eol` rule for markdown and `.prettierrc` sets
  // `endOfLine: auto`, which preserves whatever the file already has.
  const lines = markdown.split(/\r?\n/)
  let openFence = null
  let openFenceLine = 0
  let marked = 0

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]

    if (openFence !== null) {
      if (closesFence(line, openFence)) openFence = null
      continue
    }

    const delimiter = fenceDelimiter(line)
    if (delimiter !== null) {
      openFence = delimiter
      openFenceLine = index + 1
      continue
    }

    const previous = index > 0 ? lines[index - 1] : ''
    const excused = ESCAPE_HATCH.test(line) || ESCAPE_HATCH.test(previous)

    for (const match of line.matchAll(ABSOLUTE_SELF_LINK)) {
      if (excused) {
        marked += 1
        continue
      }
      unmarked.push({ url: match[0].replace(TRAILING_PUNCTUATION, ''), line: index + 1 })
    }
  }

  return { unmarked, marked, unterminatedFence: openFence === null ? null : openFenceLine }
}

// Just the findings, for a caller that has no use for the exception count.
//
// NOT FOR GATING. This drops `unterminatedFence`, which is the one signal that
// says "the rest of that file was never read" — a caller that gates on this
// helper's empty array re-opens exactly the quiet pass the fence rules close.
// Gate on `scanAbsoluteSelfLinks`; this is for tests and for callers that only
// want to know what was found.
export function findAbsoluteSelfLinks(markdown) {
  return scanAbsoluteSelfLinks(markdown).unmarked
}

// Every Markdown/MDX doc under `docsDir`, as absolute paths in stable (sorted)
// order. A missing directory yields [] so a partial checkout without the docs
// site is not a failure.
export function discoverDocFiles(docsDir) {
  if (!fs.existsSync(docsDir)) return []

  const docs = []
  const walk = (dir) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const entryPath = path.join(dir, entry.name)
      if (entry.isDirectory()) {
        walk(entryPath)
      } else if (entry.isFile() && DOC_EXTENSIONS.some((ext) => entry.name.endsWith(ext))) {
        docs.push(entryPath)
      }
    }
  }
  walk(docsDir)

  return docs.sort()
}

export function checkDocsAbsoluteSelfLinks(repoRoot = REPO_ROOT) {
  const docsDir = path.join(repoRoot, DOCS_DIR)
  const docFiles = discoverDocFiles(docsDir)

  // Narrowing tripwire. This gate's success state is "found nothing", which is
  // byte-identical to a run that LOOKED at nothing: `DOCS_DIR` is a hardcoded
  // path, and a docs-tree relocation, a Docusaurus layout change, or a typo in
  // that constant leaves `docFiles` empty and prints a green line. The sibling
  // gate (`check-docs-mcp-props.mjs`) can also bound its finding count; this one
  // cannot, because zero findings is the goal — so the file count is the only
  // coverage floor available, and the unit tests are what prove the detector
  // still fires.
  //
  // The two cases are told apart by the docs SITE, not the docs tree: no
  // `services/docs` at all is a partial checkout with nothing to check, while a
  // site that is present with no Markdown under `DOCS_DIR` means this gate has
  // gone stale and must say so.
  const docsSite = path.join(repoRoot, DOCS_SITE_DIR)
  if (docFiles.length === 0 && fs.existsSync(docsSite)) {
    console.error(`Found ${DOCS_SITE_DIR} but no .md/.mdx under ${DOCS_DIR}.`)
    console.error(
      'The docs tree this gate checks has moved; update DOCS_SITE_DIR/DOCS_DIR in ' +
        'scripts/check-docs-absolute-selflinks.mjs so it stops passing without checking anything.',
    )
    return false
  }

  const misses = []
  const unterminated = []
  let marked = 0
  for (const docFile of docFiles) {
    const relativeDoc = path.relative(repoRoot, docFile)
    const scan = scanAbsoluteSelfLinks(fs.readFileSync(docFile, 'utf8'))
    for (const { url, line } of scan.unmarked) {
      misses.push(`${relativeDoc}:${line}: ${url}`)
    }
    if (scan.unterminatedFence !== null) {
      unterminated.push(`${relativeDoc}:${scan.unterminatedFence}`)
    }
    marked += scan.marked
  }

  // Reported before the findings, and fatal on its own: everything after an
  // unterminated fence was read as code and so was never checked at all. A
  // clean "no offenders" line over a file the scanner stopped reading is
  // exactly the silent pass this gate exists to prevent.
  if (unterminated.length > 0) {
    console.error(
      'Docs pages have an unterminated code fence, so the rest of the file was ' +
        'never checked for absolute self-links:',
    )
    for (const fence of unterminated) {
      console.error(`${fence}: fence opened here and never closed`)
    }
    console.error('Close the fence (a closing run of the same character, at least as long).')
    return false
  }

  if (misses.length > 0) {
    console.error('Docs pages link to the docs site by absolute URL, bypassing onBrokenLinks:')
    for (const miss of misses) {
      console.error(miss)
    }
    console.error(
      'Rewrite each as a file-relative Markdown link (e.g. ./notes.md) so the Docusaurus ' +
        'build checks it, or mark a deliberately absolute link with ' +
        '<!-- absolute-link: intentional --> on that line or the line above it.',
    )
    return false
  }

  console.log(
    `Docs absolute self-links OK (${docFiles.length} doc file(s) scanned, ` +
      `${marked} marked exception(s))`,
  )
  return true
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) {
  if (!checkDocsAbsoluteSelfLinks()) process.exit(1)
}
