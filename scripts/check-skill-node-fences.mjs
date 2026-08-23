#!/usr/bin/env node

import { execFile } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import {
  SHELL_INFO_STRINGS,
  SKILL_ROOTS,
  extractFencedBlocks,
  findSkillMarkdownFiles,
} from './check-skill-shell.mjs'

const CONCURRENCY = 16
const NODE_DELIMITERS = new Set(['NODE', 'JS', 'MJS', 'NODE_SCRIPT'])

export { SKILL_ROOTS }

// Generated `.codex/skills` mirrors stay excluded through the imported authoring roots; scanning
// them would duplicate every `.claude/skills` finding at a path authors should not edit.

function stripComment(line) {
  let quote = null
  for (let i = 0; i < line.length; i++) {
    const ch = line[i]
    if (quote) {
      if (ch === '\\' && quote === '"' && i + 1 < line.length) i++
      else if (ch === quote) quote = null
      continue
    }
    if (ch === "'" || ch === '"') {
      quote = ch
      continue
    }
    if (ch === '\\' && i + 1 < line.length) {
      i++
      continue
    }
    if (ch === '#' && (i === 0 || /[\s;&|(){}]/.test(line[i - 1]))) return line.slice(0, i)
  }
  return line
}

function unquoteDelimiter(raw) {
  if ((raw.startsWith("'") && raw.endsWith("'")) || (raw.startsWith('"') && raw.endsWith('"'))) {
    return raw.slice(1, -1)
  }
  return raw
}

function shellWords(text) {
  const words = []
  let word = ''
  let quote = null
  const push = () => {
    if (word !== '') words.push(word)
    word = ''
  }
  for (let i = 0; i < text.length; i++) {
    const ch = text[i]
    if (quote) {
      if (ch === '\\' && quote === '"' && i + 1 < text.length) word += text[++i]
      else if (ch === quote) quote = null
      else word += ch
      continue
    }
    if (ch === "'" || ch === '"') {
      quote = ch
      continue
    }
    if (ch === '\\' && i + 1 < text.length) {
      word += text[++i]
      continue
    }
    if (/[\s;&|(){}]/.test(ch)) {
      push()
      continue
    }
    word += ch
  }
  push()
  return words
}

function commandInvokesNode(commandText) {
  const commandSubstitution = commandText.lastIndexOf('$(')
  if (
    commandSubstitution !== -1 &&
    commandInvokesNode(commandText.slice(commandSubstitution + 2))
  ) {
    return true
  }

  const words = shellWords(commandText)
  for (let i = 0; i < words.length; i++) {
    const word = words[i]
    if (/^[A-Za-z_][A-Za-z0-9_]*=.*/.test(word)) continue
    if (word === 'env') {
      while (i + 1 < words.length) {
        const next = words[i + 1]
        if (/^[A-Za-z_][A-Za-z0-9_]*=.*/.test(next)) {
          i++
          continue
        }
        if (next === '-u' || next === '--unset' || next === '-C' || next === '--chdir') {
          i += 2
          continue
        }
        if (next.startsWith('-')) {
          i++
          continue
        }
        break
      }
      continue
    }
    if (['command', 'exec', 'sudo', 'time'].includes(word)) continue
    if (word.startsWith('-') && i > 0) continue
    return path.basename(word) === 'node' || path.basename(word) === 'nodejs'
  }
  return false
}

export function extractNodeHeredocsFromShell(body) {
  const lines = body.split('\n')
  const heredocs = []
  for (let i = 0; i < lines.length; i++) {
    const line = stripComment(lines[i])
    const opener = /(^|[\s;&|])(?:\d+)?<<-?\s*("[^"]+"|'[^']+'|[A-Za-z_][A-Za-z0-9_]*)/.exec(line)
    if (!opener) continue

    const rawDelimiter = opener[2]
    const delimiter = unquoteDelimiter(rawDelimiter)
    if (!NODE_DELIMITERS.has(delimiter)) continue
    if (!commandInvokesNode(line.slice(0, opener.index + opener[1].length))) continue

    const indented = line.slice(opener.index).startsWith('<<-') || /<<-\s*/.test(line)
    const startLine = i
    const contents = []
    i += 1
    for (; i < lines.length; i++) {
      const candidate = indented ? lines[i].replace(/^\t+/, '') : lines[i]
      if (candidate === delimiter) break
      contents.push(lines[i])
    }
    heredocs.push({ lineOffset: startLine, delimiter, body: contents.join('\n') })
  }
  return heredocs
}

async function runPool(items, limit, worker) {
  const results = new Array(items.length)
  let next = 0
  const runners = Array.from({ length: Math.min(limit, items.length) }, async () => {
    for (;;) {
      const i = next++
      if (i >= items.length) return
      results[i] = await worker(items[i], i)
    }
  })
  await Promise.all(runners)
  return results
}

function makeNodeCheckRunner(tmpDir) {
  return (source, index) =>
    new Promise((resolve) => {
      const file = path.join(tmpDir, `node-heredoc-${index}.mjs`)
      fs.writeFileSync(file, `${source}\n`)
      execFile(process.execPath, ['--check', file], (error, _stdout, stderr) => {
        const message = String(stderr || '')
          .split('\n')
          .map((line) => line.split(file).join('<node heredoc>').trim())
          .filter(Boolean)
          .join('\n')
        resolve({ ok: !error, message: message || 'node --check exited non-zero with no message' })
      })
    })
}

export async function checkSkillNodeFencesInRepo(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  const pending = []

  for (const root of SKILL_ROOTS) {
    const absRoot = path.join(repoRoot, root)
    if (!fsImpl.existsSync(absRoot)) continue
    for (const file of findSkillMarkdownFiles(absRoot, deps)) {
      const rel = path.relative(repoRoot, file)
      const markdown = fsImpl.readFileSync(file, 'utf8')
      for (const block of extractFencedBlocks(markdown)) {
        if (!block.terminated || !SHELL_INFO_STRINGS.has(block.info)) continue
        for (const heredoc of extractNodeHeredocsFromShell(block.body)) {
          pending.push({
            file: rel,
            line: block.startLine + 1 + heredoc.lineOffset,
            body: heredoc.body,
          })
        }
      }
    }
  }

  if (deps.onStats) deps.onStats({ checked: pending.length })

  if (pending.length === 0) return []

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'check-skill-node-fences-'))
  const run = deps.runNodeCheck || makeNodeCheckRunner(tmpDir)
  try {
    const results = await runPool(pending, deps.concurrency || CONCURRENCY, (item, i) =>
      run(item.body, i),
    )
    return results
      .map((result, i) =>
        result.ok
          ? null
          : {
              file: pending[i].file,
              line: pending[i].line,
              kind: 'node-syntax',
              message: result.message,
            },
      )
      .filter(Boolean)
      .sort((a, b) => a.file.localeCompare(b.file) || a.line - b.line)
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true })
  }
}

async function main() {
  const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
  const findings = await checkSkillNodeFencesInRepo(repoRoot)
  if (findings.length === 0) return

  console.error('check-skill-node-fences: node heredocs in skill shell fences failed node --check:')
  for (const finding of findings) {
    console.error(`  - ${finding.file}:${finding.line} [${finding.kind}] ${finding.message}`)
  }
  process.exit(1)
}

if (isMainModule(import.meta.url)) await main()
