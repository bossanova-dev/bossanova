#!/usr/bin/env node
import { fileURLToPath } from 'node:url'
import ts from 'typescript'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import {
  BINARY_EXT,
  trackedFiles,
  readTrackedText,
  assertNonVacuousScan,
} from '../dev/tree-scan-lib.mjs'
import { structuralLines, keyColumn, keyValue } from '../dev/workflow-yaml-lib.mjs'

/** @typedef {{file: string, line: number, kind: string, snippet: string}} Finding */
/** @param {string} file */
function fileClass(file) {
  if (
    BINARY_EXT.test(file) ||
    /(?:^|\/)node_modules\/|^(lib\/bossalib\/gen|services\/web\/src\/gen|services\/docs\/openapi)\//.test(
      file,
    )
  )
    return null
  if (/(^|\/)Makefile$|\.mk$/.test(file)) return 'make'
  if (/\.sh$|^\.husky\//.test(file)) return 'shell'
  if (/(^|\/)package\.json$/.test(file)) return 'package'
  if (/^\.github\/(workflows\/[^/]+\.ya?ml|actions\/.*\/action\.ya?ml)$/.test(file))
    return 'workflow'
  if (/\.(mjs|cjs|js|ts)$/.test(file)) return 'js'
  return null
}

/** Shell command words, retaining quoted arguments as indivisible tokens.
 * @param {string} line
 * @returns {string[][]}
 */
function commandWords(line) {
  /** @type {string[]} */
  const tokens = line.match(/"(?:\\.|[^"\\])*"|'[^']*'|&&|\|\||\$\(|[;|()`]|[^\s;|()`]+/g) ?? []
  /** @type {string[][]} */
  const segments = [[]]
  for (const token of tokens) {
    if (/^(?:&&|\|\||[;|()`]|\$\(|--)$/.test(token)) segments.push([])
    else if (token.startsWith('#')) break
    else segments.at(-1).push(token)
  }
  const substitutions = tokens
    .filter((token) => token.startsWith('"'))
    .flatMap((token) =>
      [...token.matchAll(/(?<!\\)\$\(([^()]*)\)|(?<!\\)`([^`]*)`/g)].flatMap((match) =>
        commandWords(match[1] ?? match[2]),
      ),
    )
  return [
    ...substitutions,
    ...segments.map((words) => {
      while (words.length && /^[@+-]+$/.test(words[0])) words.shift()
      if (words.length) words[0] = words[0].replace(/^[@+-]+/, '')
      while (
        words.length &&
        (/^\w+=/.test(words[0]) ||
          /^(exec|env|time|nohup|sudo|xargs|then|do|else)$/.test(words[0]) ||
          (words[0] === 'command' && words[1] !== '-v'))
      )
        words.shift()
      return words
    }),
  ]
}
/** @param {string} line */
function bareCommand(line) {
  return commandWords(line).some((words) => {
    const first = words[0]?.replace(/^(['"])(.*)\1$/, '$2')
    return (
      first === 'buf' ||
      (first === 'command' && (words[1] === 'buf' || (words[1] === '-v' && words[2] === 'buf'))) ||
      (/^(which|type)$/.test(first ?? '') && words[1] === 'buf')
    )
  })
}

/** @param {string} file @param {string} text @returns {Finding[]} */
function javascriptCalls(file, text) {
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true)
  /** @type {Finding[]} */
  const findings = []
  /** @param {ts.Node} node @param {string} kind */
  function add(node, kind) {
    const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1
    findings.push({ file, line, kind, snippet: text.split('\n')[line - 1].trim() })
  }
  /** @param {ts.Node} node */
  function visit(node) {
    if (ts.isCallExpression(node)) {
      const name = ts.isIdentifier(node.expression)
        ? node.expression.text
        : ts.isPropertyAccessExpression(node.expression)
          ? node.expression.name.text
          : ''
      const arg = node.arguments[0]
      if (arg && ts.isStringLiteralLike(arg)) {
        if (
          (/^(spawn|spawnSync|execFile|execFileSync|execa|fork)$/.test(name) &&
            arg.text === 'buf') ||
          (/^(exec|execSync)$/.test(name) && /^buf(?:\s|$)/.test(arg.text))
        )
          add(node, 'js-exec')
      }
    }
    if (
      ts.isBinaryExpression(node) &&
      [ts.SyntaxKind.BarBarToken, ts.SyntaxKind.QuestionQuestionToken].includes(
        node.operatorToken.kind,
      )
    ) {
      if (
        ts.isStringLiteralLike(node.right) &&
        node.right.text === 'buf' &&
        /\b(?:process\.)?env(?:\.BUF|\[['"]BUF['"]\])/.test(node.left.getText(source))
      )
        add(node, 'js-fallback')
    }
    ts.forEachChild(node, visit)
  }
  visit(source)
  return findings
}

/** @param {string} text @returns {{line: number, command: string, kind: string}[]} */
function workflowCalls(text) {
  const structure = structuralLines(text)
  const raw = text.split('\n')
  const calls = []
  for (const entry of structure) {
    const kv = keyValue(entry.text)
    if (kv?.key === 'uses' && /^bufbuild\/buf-(?:setup-)?action@/.test(kv.value)) {
      calls.push({ line: entry.line, command: entry.text, kind: 'workflow-action' })
    }
    if (kv?.key !== 'run') continue
    if (/^[|>]/.test(kv.value)) {
      for (let n = entry.line; n < raw.length; n++) {
        if (raw[n].trim() && raw[n].length - raw[n].trimStart().length <= keyColumn(entry)) break
        if (bareCommand(raw[n])) calls.push({ line: n + 1, command: raw[n], kind: 'workflow' })
      }
    } else if (bareCommand(kv.value))
      calls.push({ line: entry.line, command: kv.value, kind: 'workflow' })
  }
  return calls
}

/** @param {{file: string, text: string}[]} entries @returns {Finding[]} */
export function findBareBufCalls(entries) {
  /** @type {Finding[]} */
  const findings = []
  for (const { file, text } of entries) {
    const kind = fileClass(file)
    if (!kind) continue
    if (kind === 'js') {
      findings.push(...javascriptCalls(file, text))
      continue
    }
    if (kind === 'workflow') {
      findings.push(
        ...workflowCalls(text).map((call) => ({
          file,
          line: call.line,
          kind: call.kind,
          snippet: call.command.trim(),
        })),
      )
      continue
    }
    if (kind === 'package') {
      const scripts = JSON.parse(text).scripts ?? {}
      for (const [name, command] of Object.entries(scripts)) {
        if (typeof command === 'string' && bareCommand(command)) {
          const offset = text.indexOf(JSON.stringify(name))
          findings.push({
            file,
            line: text.slice(0, offset).split('\n').length,
            kind,
            snippet: command,
          })
        }
      }
      continue
    }
    const lines = text.split('\n')
    for (let i = 0; i < lines.length; i++) {
      const line = i + 1
      let command = lines[i]
      if (kind === 'make' && !command.startsWith('\t')) {
        const shell = command.match(/\$\(shell\s+(.+)/)
        if (!shell) continue
        command = shell[1]
      }
      while (/\\\s*$/.test(command) && i + 1 < lines.length)
        command = command.replace(/\\\s*$/, ' ') + lines[++i].trimStart()
      if (bareCommand(command)) findings.push({ file, line, kind, snippet: command.trim() })
    }
  }
  return findings
}

/** @param {string} root */
export function scanRepository(root) {
  /** @type {Record<string, string[]>} */
  const classes = { make: [], shell: [], package: [], workflow: [], js: [] }
  const entries = []
  for (const file of trackedFiles(root)) {
    const kind = fileClass(file)
    if (!kind) continue
    classes[kind].push(file)
    entries.push({ file, text: readTrackedText(file, root) })
  }
  for (const [kind, files] of Object.entries(classes))
    assertNonVacuousScan(files, files.length || 1, `pinned-buf ${kind}`)
  return {
    findings: findBareBufCalls(entries),
    counts: Object.fromEntries(
      Object.entries(classes).map(([kind, files]) => [kind, files.length]),
    ),
  }
}

function main() {
  const root = fileURLToPath(new URL('../..', import.meta.url))
  const result = scanRepository(root)
  for (const [kind, count] of Object.entries(result.counts))
    console.log(`pinned-buf: scanned ${count} ${kind} files`)
  for (const finding of result.findings)
    console.error(`${finding.file}:${finding.line} ${finding.kind} ${finding.snippet}`)
  if (result.findings.length) {
    console.error(
      'Use "$(BUF)" in Make, scripts/dev/pinned-buf.mjs in scripts, or make buf-path in workflows; the shared installer falls back to the Go module proxy.',
    )
    process.exitCode = 1
  }
}
if (isMainModule(import.meta.url)) main()
