#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import { rmSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

/** @typedef {{ line: string[], block: boolean }} CommentSyntax */

/** @type {CommentSyntax} */
const C_FAMILY = { line: ['//'], block: true }
/** @type {CommentSyntax} */
const HASH = { line: ['#'], block: false }

/** @type {Map<string, CommentSyntax>} */
const SYNTAX_BY_EXTENSION = new Map([
  ...['.go', '.js', '.mjs', '.cjs', '.ts', '.tsx', '.jsx', '.proto', '.swift', '.css'].map(
    (ext) => /** @type {[string, CommentSyntax]} */ ([ext, C_FAMILY]),
  ),
  ...['.sh', '.bash', '.zsh', '.py', '.yaml', '.yml', '.toml', '.rb', '.mk', '.bzl', '.bazel'].map(
    (ext) => /** @type {[string, CommentSyntax]} */ ([ext, HASH]),
  ),
  ['.sql', { line: ['--'], block: true }],
])
/** @type {Map<string, CommentSyntax>} */
const SYNTAX_BY_NAME = new Map(
  ['Makefile', 'Dockerfile', 'BUILD', 'WORKSPACE'].map((name) => [name, HASH]),
)

// Directives look like comments but change what a compiler, linter or tool does.
const DIRECTIVE =
  /^(?:\/\/|#|--|\/\*+|\*)\s*(?:!|go:|\+build|nolint|lint:|export\s|line\s|eslint|biome-ignore|prettier-ignore|@ts-|istanbul|c8\s|noqa|type:\s*ignore|shellcheck|gazelle:|buf:lint|keep-sorted|syntax=|SPDX|@license|@preserve|Code generated)/i
const GENERATED = /^(?:\/\/|#|--) Code generated .* DO NOT EDIT\.$/m

/** @param {string} file */
export function commentSyntax(file) {
  return SYNTAX_BY_NAME.get(path.basename(file)) ?? SYNTAX_BY_EXTENSION.get(path.extname(file))
}

/** @param {string} text @param {CommentSyntax} syntax */
function startsComment(text, syntax) {
  if (syntax.line.some((token) => text.startsWith(token))) return true
  return syntax.block && /^(?:\/\*|\*(?:\s|\/|$))/.test(text)
}

/** @param {string} line @param {CommentSyntax} syntax */
function isCommentLine(line, syntax) {
  const text = line.trim()
  return startsComment(text, syntax) && !DIRECTIVE.test(text)
}

/** @param {string} code */
function quotesBalanced(code) {
  const unescaped = code.replace(/\\./g, '')
  return ['"', "'", '`'].every((quote) => unescaped.split(quote).length % 2 === 1)
}

/** A trailing comment removed from a code line leaves the code exactly as it was.
 * @param {string} removed @param {string} added @param {CommentSyntax} syntax
 */
function isTrailingCommentRemoval(removed, added, syntax) {
  const code = added.trimEnd()
  if (code.trim() === '' || !removed.startsWith(code) || !quotesBalanced(code)) return false
  const rest = removed.slice(code.length)
  return /^\s/.test(rest) && isCommentLine(rest, syntax)
}

/** @param {string} diff */
function parseHunks(diff) {
  /** @type {{ removed: string[], added: string[] }[]} */
  const hunks = []
  for (const line of diff.split('\n')) {
    if (line.startsWith('@@')) hunks.push({ removed: [], added: [] })
    else if (hunks.length === 0 || line.startsWith('\\')) continue
    else if (line.startsWith('-')) hunks[hunks.length - 1].removed.push(line.slice(1))
    else if (line.startsWith('+')) hunks[hunks.length - 1].added.push(line.slice(1))
  }
  return hunks
}

/**
 * Why one file's edit is not a pure deletion of comments the branch added, or null when it is.
 * @param {{ diff: string, branchAdded: Set<string>, syntax: CommentSyntax }} input
 */
export function judgeFileEdit({ diff, branchAdded, syntax }) {
  let comments = 0
  for (const { removed, added } of parseHunks(diff)) {
    const unused = [...removed]
    for (const line of added) {
      const idx = unused.findIndex((r) => isTrailingCommentRemoval(r, line, syntax))
      if (idx === -1) return 'changes code'
      const [original] = unused.splice(idx, 1)
      if (!branchAdded.has(original)) return 'removes a comment the branch did not add'
      comments++
    }
    const blanks = unused.filter((line) => line.trim() === '')
    const dropped = unused.filter((line) => line.trim() !== '')
    if (dropped.some((line) => !isCommentLine(line, syntax))) return 'removes code or a directive'
    if (dropped.some((line) => !branchAdded.has(line)))
      return 'removes a comment the branch did not add'
    if (blanks.length > 0 && dropped.length === 0 && added.length === 0)
      return 'removes only blank lines'
    comments += dropped.length
  }
  return comments === 0 ? 'removes no comment' : null
}

/**
 * Keep the worktree's comment deletions inside the branch's own diff and restore every other
 * change to `from`, the tip recorded before the cleanup ran. HEAD and the index end at `from`.
 * @param {{ cwd: string, base: string, from: string }} options
 */
export function checkCommentDeletions({ cwd, base, from }) {
  /** @param {string[]} args */
  const git = (args) => execFileSync('git', args, { cwd, encoding: 'utf8', maxBuffer: 1 << 28 })
  const root = git(['rev-parse', '--show-toplevel']).trim()
  git(['reset', '-q', from])
  const inScope = new Set(
    git(['diff', '--no-renames', '--name-only', '-z', base, from]).split('\0'),
  )
  /** @type {{ path: string, reason: string }[]} */
  const rejected = []
  for (const file of git(['ls-files', '--others', '--exclude-standard', '-z']).split('\0')) {
    if (!file) continue
    rmSync(path.join(root, file), { recursive: true, force: true, maxRetries: 3 })
    rejected.push({ path: file, reason: 'creates a file' })
  }
  /** @type {string[]} */
  const accepted = []
  const status = git(['diff', '--no-renames', '--name-status', '-z', from]).split('\0')
  for (let i = 0; i + 1 < status.length; i += 2) {
    const [kind, file] = [status[i], status[i + 1]]
    const syntax = commentSyntax(file)
    let reason = null
    if (kind !== 'M') reason = 'adds or deletes a file'
    else if (!inScope.has(file)) reason = 'outside the branch diff'
    else if (!syntax) reason = 'unsupported file type'
    else if (file.split('/').includes('gen') || GENERATED.test(git(['show', `${from}:${file}`])))
      reason = 'generated file'
    else {
      const branchAdded = new Set(
        parseHunks(
          git(['diff', '-U0', '--no-color', '--no-ext-diff', base, from, '--', file]),
        ).flatMap((hunk) => hunk.added),
      )
      const diff = git(['diff', '-U0', '--no-color', '--no-ext-diff', from, '--', file])
      reason = judgeFileEdit({ diff, branchAdded, syntax })
    }
    if (reason === null) accepted.push(file)
    else {
      git(['restore', `--source=${from}`, '--worktree', '--', file])
      rejected.push({ path: file, reason })
    }
  }
  return { accepted: accepted.sort(), rejected }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { base: { type: 'string' }, from: { type: 'string' } },
    strict: true,
  })
  if (!values.base || !values.from) {
    console.error('usage: comment-deletions.mjs --base <merge-base> --from <pre-cleanup sha>')
    process.exit(2)
  }
  const result = checkCommentDeletions({ cwd: process.cwd(), base: values.base, from: values.from })
  console.log(JSON.stringify(result))
}
