// scripts/plan-scratch-token-scan.mjs
//
// One scanner for the `.linear-plans/…` path tokens a skill's prose cites.
//
// Two gates need it and they read different surfaces: skills-toolbox/plan-scratch-paths.test.mjs
// walks the published boss-plan payload directory on disk, while scripts/bs-plan-skill.test.mjs
// already holds the payload documents plus the repo-local CE draft extension in memory. A second
// copy of the extraction rules would let the two disagree about what counts as a token, and the
// gate that missed one would still report a clean scan — so the rules live here and both import
// them.
//
// Node built-ins only — cron worktrees are dependency-free.

import { readdirSync, readFileSync } from 'node:fs'
import { join, relative } from 'node:path'

// A token runs from `.linear-plans` to the first character that cannot be part of a cited path.
// Markdown delimiters (backticks, quotes, parens) and whitespace end it; `<>{},*` stay in, because
// the payload cites templates, brace lists and globs.
const TOKEN_RE = /\.linear-plans(?:\/[^\s`'"()\\|]*)?/g
const TRAILING_RE = /[.,;:!?—-]+$/

/**
 * Trim the punctuation a sentence or a shell expansion left on the end of a cited path.
 * @param {string} raw
 * @returns {string}
 */
export function trimScratchToken(raw) {
  let token = raw
  // A trailing `}` that closes a `${VAR:-default}` expansion is not part of the path. Drop
  // unbalanced closers before anything else.
  while (
    token.endsWith('}') &&
    (token.match(/\}/g) || []).length > (token.match(/\{/g) || []).length
  ) {
    token = token.slice(0, -1)
  }
  // A sentence-ending period is not part of the path, but `.json` / `.md` is.
  if (!/\.(json|md|rejected|sh|mjs)$/.test(token)) token = token.replace(TRAILING_RE, '')
  return token
}

/**
 * Every scratch token in one document, with the line it was cited on.
 * @param {string} text
 * @param {string} [file]  label carried into the violation message
 * @returns {{token: string, file: string, line: number}[]}
 */
export function scratchTokensIn(text, file = '<text>') {
  const found = []
  text.split('\n').forEach((line, i) => {
    for (const match of line.matchAll(TOKEN_RE)) {
      found.push({ token: trimScratchToken(match[0]), file, line: i + 1 })
    }
  })
  return found
}

/**
 * Every scratch token across a set of files on disk.
 * @param {string[]} files  absolute paths
 * @param {{relativeTo?: string}} [opts]  base the reported `file` label is relative to
 * @returns {{token: string, file: string, line: number}[]}
 */
export function scratchTokensInFiles(files, { relativeTo } = {}) {
  return files.flatMap((file) =>
    scratchTokensIn(readFileSync(file, 'utf8'), relativeTo ? relative(relativeTo, file) : file),
  )
}

/**
 * Every `.md` file under `dir`, recursively.
 * @param {string} dir
 * @param {string[]} [out]
 * @returns {string[]}
 */
export function markdownFilesUnder(dir, out = []) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) markdownFilesUnder(path, out)
    else if (entry.name.endsWith('.md')) out.push(path)
  }
  return out
}
