#!/usr/bin/env node
import { execFileSync } from 'node:child_process'
import { readFileSync, statSync } from 'node:fs'
import { resolve } from 'node:path'
import { parseArgs } from 'node:util'
import {
  CITATION_PATTERN,
  identifierAnchor,
  repoRootIsReadable,
  resolveCitationCoordinate,
} from './citation-coordinate.mjs'
import { isMainModule } from './main-module.mjs'
import {
  fixedStringNeedle,
  loadSkillConfig,
  parsePlanCheckItems,
  scanFences,
  tokenizeSimpleShell,
} from './skill-config.mjs'

const ANCHOR_WINDOW = 5
const MAX_HITS = 5
const FIXED_SEARCH_FLAGS = new Set(['--fixed-strings', '--fixed-regexp'])
function tracked(root) {
  try {
    return execFileSync('git', ['ls-files', '-z'], {
      cwd: root,
      encoding: 'utf8',
      maxBuffer: 8 * 1024 * 1024,
    })
      .split('\0')
      .filter(Boolean)
  } catch {
    return null
  }
}
function locations(body, file, anchors) {
  return body.split('\n').flatMap((line, index) =>
    anchors.some((anchor) => {
      const escaped = anchor.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
      return new RegExp(`(?<![\\w$])${escaped}(?![\\w$])`).test(line)
    })
      ? [{ file, line: index + 1 }]
      : [],
  )
}
function relocate(root, anchors) {
  const hits = []
  for (const anchor of anchors) {
    try {
      const output = execFileSync('git', ['grep', '-n', '-I', '-F', '-w', '--', anchor], {
        cwd: root,
        encoding: 'utf8',
        maxBuffer: 1024 * 1024,
      })
      for (const row of output.trimEnd().split('\n')) {
        const match = /^(.*?):(\d+):/.exec(row)
        // git grep can see a tracked symlink; never accept a coordinate outside the root.
        if (match && resolveCitationCoordinate(root, match[1]).ok)
          hits.push({ file: match[1], line: Number(match[2]) })
      }
    } catch (error) {
      if (error.status !== 1) return { locations: [], relocation: 'unknown' }
    }
  }
  return {
    locations: hits.slice(0, MAX_HITS),
    ambiguous: hits.length > MAX_HITS,
    relocation: hits.length ? 'found' : 'missing',
  }
}
function citation(root, file, line, anchors) {
  const coordinate = resolveCitationCoordinate(root, file, line)
  if (!coordinate.ok && coordinate.code !== 'short-file') {
    return {
      kind: 'path-missing',
      reason: coordinate.code,
      ...(coordinate.code !== 'escapes-root' && anchors.length ? relocate(root, anchors) : {}),
    }
  }
  if (!anchors.length) return { kind: coordinate.ok ? 'current' : 'short-file', unanchored: true }
  const body = coordinate.ok ? coordinate.body : resolveCitationCoordinate(root, file).body
  const hits = locations(body, file, anchors)
  if (hits.length)
    return {
      kind: hits.some((hit) => Math.abs(hit.line - line) <= ANCHOR_WINDOW) ? 'current' : 'moved',
      locations: hits,
    }
  const elsewhere = relocate(root, anchors)
  return {
    kind:
      elsewhere.relocation === 'found'
        ? 'relocated'
        : elsewhere.relocation === 'unknown'
          ? 'unknown'
          : 'anchor-missing',
    ...elsewhere,
  }
}

// Reject syntax rather than executing it. Quoted literal metacharacters remain valid search text.
function simpleCommand(command) {
  let quote = null
  for (const char of command) {
    if (quote) {
      if (quote === '"' && /[$`\\]/.test(char)) return false
      if (char === quote) quote = null
      continue
    }
    if (char === "'" || char === '"') {
      quote = char
      continue
    }
    if (/[;|&$`\\>\n\r()]/.test(char)) return false
  }
  return quote === null
}
function premise(root, item) {
  const command = item.check || ''
  const pending = { kind: 'not-evaluated', command }
  if (!simpleCommand(command)) return pending
  const tokens = tokenizeSimpleShell(command)
  const negate = tokens[0] === '!'
  if (negate) tokens.shift()
  if (tokens[0] === 'git' && tokens[1] === 'grep') tokens.shift()
  const program = tokens.shift()
  const claim = item.text
    .split(/—\s*check:/i)[0]
    .replace(/`[^`]*`/g, '')
    .replace(new RegExp(CITATION_PATTERN, 'g'), '')
  const numbers = claim.match(/\b\d+\b/g) || []
  let measured
  if (program === 'wc') {
    const mode = tokens.shift()
    if (tokens[0] === '<') tokens.shift()
    if (negate || !['-c', '-l'].includes(mode) || tokens.length !== 1 || numbers.length !== 1)
      return pending
    const read = resolveCitationCoordinate(root, tokens[0])
    if (!read.ok) return { kind: 'refuted', command, reason: read.code }
    measured =
      mode === '-c' ? readFileSync(read.path).length : (read.body.match(/\n/g) || []).length
  } else if (program === 'grep' || program === 'rg') {
    // Only flags whose semantics are implemented below are accepted. Unknown options stay manual.
    let count = false,
      word = false,
      needle = null
    const paths = []
    let options = true
    for (const token of tokens) {
      if (options && token === '--') {
        options = false
        continue
      }
      if (options && token.startsWith('-')) {
        if (FIXED_SEARCH_FLAGS.has(token)) continue
        if (!/^-[FncwqhrR]+$/.test(token)) return pending
        count ||= token.includes('c')
        word ||= token.includes('w')
      } else if (needle === null) needle = token
      else paths.push(token)
    }
    if (!needle || fixedStringNeedle(command) !== needle || tokens.includes('<')) return pending
    if (count && (paths.length !== 1 || numbers.length !== 1 || negate)) return pending
    let files = paths.length ? paths : tracked(root)
    if (!files) return pending
    const expanded = []
    for (const file of files) {
      const read = resolveCitationCoordinate(root, file)
      if (read.ok) {
        expanded.push(file)
        continue
      }
      if (!read.path) return pending
      let directory = false
      try {
        directory = statSync(read.path).isDirectory()
      } catch {}
      if (
        !directory ||
        (program === 'grep' && !tokens.some((token) => /^-[A-Za-z]*[rR]/.test(token)))
      )
        return pending
      const all = tracked(root)
      if (!all) return pending
      const prefix = resolve(root, file) + '/'
      expanded.push(...all.filter((path) => resolve(root, path).startsWith(prefix)))
    }
    files = expanded
    let hits = 0
    for (const file of files) {
      const read = resolveCitationCoordinate(root, file)
      if (!read.ok) return pending
      const lines = read.body.split('\n')
      if (lines.at(-1) === '') lines.pop()
      hits += word
        ? locations(lines.join('\n'), file, [needle]).length
        : lines.filter((line) => line.includes(needle)).length
    }
    if (!count)
      return {
        kind: (negate ? hits === 0 : hits > 0) ? 'confirmed' : 'refuted',
        command,
        measured: hits,
      }
    measured = hits
  } else return pending
  const claimed = Number(numbers[0])
  return { kind: measured === claimed ? 'confirmed' : 'count-drift', command, claimed, measured }
}

export function recheckPlan(config, planText, { root = process.cwd() } = {}) {
  root = resolve(root)
  if (typeof planText !== 'string' || !repoRootIsReadable(root))
    return { verdict: 'unknown', root, checked: 0, items: [] }
  const parsed = parsePlanCheckItems(config, planText)
  const items = []
  const premiseTexts = parsed.premises.map((item) => item.text)
  // Join continuation lines into their list item so anchors can accompany multiline citations.
  const blocks = []
  for (const { line } of scanFences(parsed.text).lines) {
    if (/^\s+\S/.test(line) && blocks.length) blocks[blocks.length - 1] += '\n' + line
    else blocks.push(line)
  }
  for (const text of blocks) {
    const inPremise = premiseTexts.some(
      (p) =>
        text.includes(p) ||
        (p.includes(text.replace(/^\s*-\s*\[[ xX]\]\s*/, '')) && text.trim() !== ''),
    )
    const spans = [...text.matchAll(/`+([^`\n]+)`+/g)].map((match) => match[1])
    const anchors = spans.map(identifierAnchor).filter(Boolean)
    for (const match of text.matchAll(new RegExp(CITATION_PATTERN, 'g'))) {
      const file = match[1],
        line = Number(match[2])
      items.push({
        source: 'citation',
        text,
        inPremise,
        file,
        line,
        anchors,
        ...citation(root, file, line, anchors),
      })
    }
    for (const file of spans.filter(
      (span) => /\//.test(span) && /\.[\w-]+$/.test(span) && !/\s/.test(span),
    )) {
      const read = resolveCitationCoordinate(root, file)
      items.push({
        source: 'path',
        text,
        inPremise,
        file,
        kind: read.ok ? 'present' : 'absent',
        ...(read.ok ? {} : { reason: read.code }),
      })
    }
  }
  for (const item of parsed.premises)
    items.push({ source: 'premise', text: item.text, inPremise: true, ...premise(root, item) })
  const stale = items.some(
    (item) =>
      item.inPremise &&
      ['anchor-missing', 'path-missing', 'refuted', 'count-drift'].includes(item.kind),
  )
  // Missing paths without a premise need a new location; absent paths may be planned additions.
  const drift = items.some(
    (item) =>
      ['moved', 'relocated', 'anchor-missing', 'path-missing', 'short-file'].includes(item.kind) ||
      (item.kind === 'absent' && !item.inPremise),
  )
  return {
    verdict: stale ? 'stale' : drift ? 'drifted' : 'current',
    root,
    checked: items.length,
    items,
  }
}

export function main(args = process.argv.slice(2)) {
  let result
  try {
    const { values, positionals } = parseArgs({
      args,
      strict: true,
      allowPositionals: true,
      options: { root: { type: 'string' }, help: { type: 'boolean' } },
    })
    if (values.help) {
      process.stdout.write('Usage: plan-recheck.mjs <plan> [--root <dir>]\n')
      return 0
    }
    if (positionals.length !== 1) throw new Error('Expected one plan path')
    result = recheckPlan(loadSkillConfig(), readFileSync(positionals[0], 'utf8'), {
      root: values.root,
    })
  } catch (error) {
    result = { verdict: 'unknown', checked: 0, items: [], error: error.message }
  }
  process.stdout.write(JSON.stringify(result, null, 2) + '\n')
  return result.verdict === 'unknown' ? 2 : result.verdict === 'current' ? 0 : 1
}
if (isMainModule(import.meta.url)) process.exitCode = main()
