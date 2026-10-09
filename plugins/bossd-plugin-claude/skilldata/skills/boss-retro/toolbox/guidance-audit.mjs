// Deterministic candidate audit, not a parser. Computed guidance reader names and
// cross-package readers, and languages other than JS/TS and Go, are out of reach.
import fs from 'node:fs'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { globToRegExp } from './skill-config.mjs'
import { RUNG_IDS } from './retro-ladder.mjs'
import { isMainModule } from './main-module.mjs'

const hash = (s) => createHash('sha256').update(s).digest('hex')
const normalize = (s) => s.trim().replace(/\s+/g, ' ')
const defaults = {
  guidanceGlobs: [
    '**/SKILL.md',
    '**/skills/**/references/**/*.md',
    'AGENTS.md',
    '**/AGENTS.md',
    'CLAUDE.md',
    '**/CLAUDE.md',
    'GEMINI.md',
    '.github/copilot-instructions.md',
    '.cursorrules',
    '.cursor/rules/**',
    '.claude/skills/**/*.md',
    '.codex/skills/**/*.md',
  ],
  rulesFiles: [
    'AGENTS.md',
    'CLAUDE.md',
    'GEMINI.md',
    '.github/copilot-instructions.md',
    '.cursorrules',
  ],
  testGlobs: [
    '**/*_test.go',
    ...['js', 'mjs', 'cjs', 'ts', 'tsx'].flatMap((ext) => [`**/*.test.${ext}`, `**/*.spec.${ext}`]),
  ],
  ignore: [],
  minFenceLines: 8,
  minParagraphChars: 200,
}
const matches = (file, globs) => globs.some((g) => globToRegExp(g).test(file))
export function inventory(root, config = {}, { testsOnly = false } = {}) {
  const options = { ...defaults, ...config }
  let names
  try {
    names = execFileSync('git', ['ls-files', '-z'], {
      cwd: root,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    })
      .split('\0')
      .filter(Boolean)
  } catch {
    names = []
    const walk = (dir) => {
      for (const entry of fs.readdirSync(path.join(root, dir), { withFileTypes: true })) {
        if (['.git', 'node_modules'].includes(entry.name)) continue
        const file = path.posix.join(dir, entry.name)
        if (entry.isDirectory()) walk(file)
        else names.push(file)
      }
    }
    walk('')
  }
  names.sort()
  const files = names
    .filter((f) => !matches(f, options.ignore))
    .filter(
      (f) => (!testsOnly && matches(f, options.guidanceGlobs)) || matches(f, options.testGlobs),
    )
    .flatMap((file) => {
      try {
        return [{ file, text: fs.readFileSync(path.join(root, file), 'utf8') }]
      } catch (error) {
        if (error.code === 'ENOENT') return []
        throw error
      }
    })
  const seen = new Set()
  const rules = options.rulesFiles.flatMap((file) => {
    const entry = files.find((f) => f.file === file)
    if (!entry) return []
    const real = fs.realpathSync(path.join(root, file))
    if (seen.has(real)) return []
    seen.add(real)
    return [entry]
  })
  const packageDirs = new Set(
    files.filter((f) => f.file.endsWith('_test.go')).map((f) => path.dirname(f.file)),
  )
  const readerFiles = names
    .filter(
      (file) =>
        file.endsWith('.go') &&
        !file.endsWith('_test.go') &&
        packageDirs.has(path.dirname(file)) &&
        !matches(file, options.ignore),
    )
    .flatMap((file) => {
      const text = fs.readFileSync(path.join(root, file), 'utf8')
      return guidanceNames(options).some((name) => text.includes(name)) && readVerb.test(text)
        ? [{ file, text, readerOnly: true }]
        : []
    })
  return {
    guidance: files.filter((f) => matches(f.file, options.guidanceGlobs)),
    tests: [...files.filter((f) => matches(f.file, options.testGlobs)), ...readerFiles],
    rules,
    options,
  }
}
export function mirrorFamilies(files) {
  const groups = new Map()
  for (const entry of files) {
    const match = entry.file.match(/(?:^|\/)(?:skills|skilldata)\/(.+)$/)
    const key = match ? match[1].replace(/^skills\//, '') : entry.file
    if (!groups.has(key)) groups.set(key, [])
    groups.get(key).push(entry)
  }
  return [...groups]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([key, members]) => ({
      key,
      members: members.sort((a, b) => a.file.localeCompare(b.file)),
    }))
}
const readVerb = /\b(?:readFileSync|readFile|ReadFile|Open|statSync)\s*\(/
const literal = /(?:['"`]([^'"`\n]+)['"`]|\/[^/\n]+\/[gimsuy]*)/
const identifiers = (s) => s.match(/\b[A-Za-z_$][\w$]*\b/g) || []
const escaped = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
function guidanceNames(config) {
  return [
    'SKILL.md',
    'AGENTS.md',
    'CLAUDE.md',
    'GEMINI.md',
    'references/',
    ...(config.rulesFiles || []).map((f) => path.basename(f)),
    ...(config.guidanceGlobs || []).map((f) => path.basename(f)).filter((f) => !/[?*]/.test(f)),
  ]
}
// Mask literals for code probes, and multiline fixture bodies for the line scan.
// This keeps detector tests containing source excerpts from becoming audit findings.
function lexicalLines(text) {
  const code = [...text],
    scan = [...text]
  const token =
    /\/\*[\s\S]*?\*\/|\/\/[^\n]*|'(?:\\[\s\S]|[^'\\])*'|"(?:\\[\s\S]|[^"\\])*"|`(?:\\[\s\S]|[^`\\])*`/g
  for (const match of text.matchAll(token)) {
    const comment = match[0].startsWith('//') || match[0].startsWith('/*')
    for (let i = match.index; i < match.index + match[0].length; i++) {
      if (text[i] === '\n') continue
      code[i] = ' '
      if (!comment && match[0].includes('\n')) scan[i] = ' '
    }
  }
  return { code: code.join('').split('\n'), scan: scan.join('').split('\n') }
}
function readers(files, names) {
  const result = new Map()
  for (const { text } of files) {
    const start =
      /(?:function\s+(\w+)\s*\(|(?:const|let)\s+(\w+)\s*=\s*(?:async\s*)?(?:\([^)]*\)|\w+)\s*=>|func\s+(\w+)\s*\()/g
    for (const match of text.matchAll(start)) {
      const bodyStart = match.index + match[0].length
      const brace = text.indexOf('{', bodyStart),
        newline = text.indexOf('\n', bodyStart)
      if (match[3] && /\)\s*error\s*$/.test(text.slice(bodyStart, brace).trim())) continue
      let body
      if (match[2] && (brace < 0 || (newline >= 0 && newline < brace)))
        body = text.slice(bodyStart, newline < 0 ? text.length : newline)
      else {
        if (brace < 0) continue
        let depth = 1,
          end = brace + 1
        for (; end < text.length && depth; end++) {
          if (text[end] === '{') depth++
          if (text[end] === '}') depth--
        }
        body = text.slice(brace, end)
      }
      if (readVerb.test(body) && readVerb.test(lexicalLines(body).code.join('\n')))
        result.set(match[1] || match[2] || match[3], names.find((n) => body.includes(n)) || null)
    }
  }
  return result
}
export function detectProsePins(files, config = {}) {
  const names = guidanceNames(config),
    findings = []
  const goPackages = new Map()
  for (const file of files.filter((f) => f.file.endsWith('.go'))) {
    const dir = path.dirname(file.file)
    if (!goPackages.has(dir)) goPackages.set(dir, [])
    goPackages.get(dir).push(file)
  }
  const packageReaders = new Map(
    [...goPackages].map(([dir, group]) => [
      dir,
      readers(
        group.filter(({ text }) => names.some((name) => text.includes(name))),
        names,
      ),
    ]),
  )
  for (const { file, text, readerOnly } of files) {
    if (readerOnly) continue
    if (!file.endsWith('.go') && !names.some((n) => text.includes(n))) continue
    const helpers = file.endsWith('.go')
      ? packageReaders.get(path.dirname(file))
      : readers([{ text }], names)
    if (
      !names.some((n) => text.includes(n)) &&
      ![...helpers.keys()].some((n) => text.includes(n + '('))
    )
      continue
    const taint = new Map(),
      treeTaint = new Set(),
      temps = new Set(),
      lines = lexicalLines(text).scan
    const tempReaders = [
      ...text.matchAll(
        /(?:function\s+(\w+)\s*\([^)]*\)|(?:const|let)\s+(\w+)\s*=)[^{\n]*\{[^}]*?(?:TempDir|MkdirTemp|mkdtemp|tmpdir)[^}]*?\}/g,
      ),
    ].map((m) => m[1] || m[2])
    const loopScopes = []
    const traversalPaths = new Set()
    let depth = 0
    for (let index = 0; index < lines.length; index++) {
      const line = lines[index]
      const codeLine = lexicalLines(line).code[0]
      if (/^\s*(?:test|it)\s*\(/.test(line) || /^func Test/.test(line)) loopScopes.length = 0
      while (loopScopes.length && depth < loopScopes.at(-1).depth) loopScopes.pop()
      const traversal =
        codeLine.match(/for\s*\(\s*(?:const|let|var)\s+(\w+)\s+(?:of|in)\b/) ||
        codeLine.match(/for\s+(?:\w+\s*,\s*)?(\w+)\s*:?=\s*range\b/) ||
        codeLine.match(/WalkDir\s*\([^\n]*?func\s*\(\s*(\w+)\s+string/)
      if (traversal && codeLine.includes('{'))
        loopScopes.push({ depth: depth + 1, variable: traversal[1] })
      const isTraversalPath = (value) =>
        identifiers(value).some(
          (id) => traversalPaths.has(id) || loopScopes.some((scope) => scope.variable === id),
        )
      const assignment = codeLine.match(
        /(?:\b(?:const|let|var)\s+)?([\w$]+)(?:\s*,\s*\w+)?\s*(?::=|=(?!=))\s*(.*)/,
      )
      if (assignment) {
        const [, target, codeRhs] = assignment
        const rhs = line.slice(assignment.index + assignment[0].length - codeRhs.length)
        if (
          /TempDir|MkdirTemp|mkdtemp|tmpdir|tmpDir/.test(rhs) ||
          identifiers(rhs).some((id) => temps.has(id) || tempReaders.includes(id))
        )
          temps.add(target)
        else temps.delete(target)
        const source = names.find((n) => rhs.includes(n))
        const helper = [...helpers].find(([name]) =>
          new RegExp('\\b' + escaped(name) + '\\s*\\(').test(lexicalLines(rhs).code[0]),
        )
        const parent = identifiers(codeRhs).find((id) => taint.has(id))
        const temporary = identifiers(rhs).some((id) => temps.has(id))
        if (isTraversalPath(codeRhs)) traversalPaths.add(target)
        else traversalPaths.delete(target)
        if (/frontmatter|yaml\.(?:parse|load)|parseFrontmatter/i.test(rhs)) taint.delete(target)
        else if (
          !temporary &&
          ((readVerb.test(lexicalLines(rhs).code[0]) && source) ||
            (helper && (helper[1] || source)) ||
            parent)
        ) {
          taint.set(target, source || helper?.[1] || taint.get(parent))
          if (
            (readVerb.test(lexicalLines(rhs).code[0]) && isTraversalPath(codeRhs)) ||
            treeTaint.has(parent)
          )
            treeTaint.add(target)
          else treeTaint.delete(target)
        } else taint.delete(target)
      }
      depth += (codeLine.match(/\{/g) || []).length - (codeLine.match(/\}/g) || []).length
      if (/guidance-audit:\s*allow/.test(line + '\n' + (lines[index - 1] || ''))) continue
      if (
        /\b(?:existsSync|os\.Stat|fs\.access|resolve)\s*\(/.test(
          codeLine.replace(/\/[^/\n]+\/[gimsuy]*/g, ' '),
        )
      )
        continue
      const tainted = identifiers(codeLine).find((id) => taint.has(id))
      const direct =
        readVerb.test(codeLine) &&
        !identifiers(line).some((id) => temps.has(id)) &&
        names.find((n) => line.includes(n))
      if (!tainted && !direct) continue
      const assertLine =
        /assert\.|assertContains\(|expect\(|strings\.Contains|MatchString|(?:\bif\b.*(?:==|!=|len\())/.test(
          line,
        )
      if (!assertLine) continue
      const equality = codeLine.match(
        /\b(?:equal|strictEqual|deepEqual|deepStrictEqual|Equal)\s*\(/,
      )
      if (equality) {
        const args = assertionOperands(codeLine.slice(equality.index + equality[0].length))
        // Diagnostic messages are not compared operands; Go assertions take t first.
        const operands =
          /Equal$/.test(equality[0].replace(/\s*\($/, '')) && file.endsWith('.go')
            ? args.slice(1, 3)
            : args.slice(0, 2)
        if (
          operands.length === 2 &&
          operands.every((operand) => {
            const value = operand.trim()
            const cast = file.endsWith('.go') && value.match(/^string\(\s*(\w+)\s*\)$/)
            return taint.has(value) || (cast && taint.has(cast[1]))
          })
        )
          continue
      }
      let kind
      if (/(?:\.length|\blen\(|byteLength|\.size)/.test(line) && /\b\d+\b/.test(line)) kind = 'size'
      else if (/(?:equal|Equal|==|!=)/.test(line) && literal.test(line)) kind = 'equality'
      else if (/match|Match|includes|Contains|toContain/.test(line)) {
        // The literal must be an assertion needle, not the guidance path itself.
        const needle =
          line.match(
            /(?:doesNotMatch|match|strings\.Contains|assert\.Contains|assertContains)\s*\([^,]*,\s*(?:[^,]*,\s*)?(['"`\/].*)/,
          ) || line.match(/(?:includes|toContain|toMatch|MustCompile)\s*\((['"`\/].*)/)
        if (!needle || !literal.test(needle[1])) continue
        const negative = /doesNotMatch|!\s*\w+\.includes|not\.to|if\s+strings\.Contains/.test(line)
        kind = negative ? 'prohibition' : /(?:\^?#|## )/.test(needle[1]) ? 'heading' : 'phrase'
      }
      if (
        !kind ||
        (kind === 'prohibition' &&
          (treeTaint.has(tainted) || (direct && isTraversalPath(codeLine))))
      )
        continue
      const guidance = taint.get(tainted) || direct
      const snippet = normalize(line)
      findings.push({
        file,
        line: index + 1,
        kind,
        guidance,
        snippet,
        fingerprint: hash(file + '\0' + kind + '\0' + guidance + '\0' + snippet),
      })
    }
  }
  return findings
}
export function weightSignals(families, snapshot, config = {}) {
  const options = { ...defaults, ...config },
    blocks = new Map(),
    paragraphs = new Map()
  const add = (map, text, key, file, line) => {
    const normalized = normalize(text)
    const id = hash(normalized)
    if (!map.has(id)) map.set(id, { text: normalized, occurrences: [] })
    map.get(id).occurrences.push({ family: key, file, line })
  }
  const weights = families.map(({ key, members }) => {
    const { file, text } = members[0],
      previous = snapshot?.families?.[key]
    const lines = text.split('\n')
    let fence = null,
      paragraph = []
    const flush = (line) => {
      const value = paragraph.join('\n')
      if (normalize(value).length >= options.minParagraphChars)
        add(paragraphs, value, key, file, line - paragraph.length)
      paragraph = []
    }
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i]
      if (/^\s*(```|~~~)/.test(line)) {
        flush(i + 1)
        if (fence) {
          if (fence.lines.length >= options.minFenceLines)
            add(blocks, fence.lines.join('\n'), key, file, fence.start)
          fence = null
        } else fence = { start: i + 1, lines: [] }
      } else if (fence) fence.lines.push(line)
      else if (!line.trim() || /^\s*#/.test(line)) flush(i + 1)
      else paragraph.push(line)
    }
    flush(lines.length + 1)
    const bytes = Buffer.byteLength(text)
    return {
      key,
      file,
      bytes,
      lines: lines.length,
      ...(previous
        ? {
            deltaBytes: bytes - previous.bytes,
            deltaLines: lines.length - previous.lines,
            takenAt: snapshot.takenAt,
            runId: snapshot.runId,
          }
        : { new: true }),
    }
  })
  const duplicates = (map) =>
    [...map.values()].filter((v) => new Set(v.occurrences.map((o) => o.family)).size > 1)
  const repeatedBlocks = duplicates(blocks),
    duplicatedParagraphs = duplicates(paragraphs)
  const hotspots = weights
    .filter((w) => w.deltaBytes > 0 || w.deltaLines > 0)
    .map((w) => ({ hotspotKey: w.key + ':growth', family: w.key, file: w.file, signal: 'growth' }))
  for (const [signal, items] of [
    ['fence', repeatedBlocks],
    ['paragraph', duplicatedParagraphs],
  ]) {
    for (const family of new Set(items.flatMap((v) => v.occurrences.map((o) => o.family))))
      hotspots.push({
        hotspotKey: family + ':' + signal,
        family,
        file: weights.find((w) => w.key === family).file,
        signal,
      })
  }
  return {
    weights,
    removed: Object.keys(snapshot?.families || {}).filter(
      (key) => !families.some((f) => f.key === key),
    ),
    repeatedBlocks,
    duplicatedParagraphs,
    hotspots,
  }
}
export function rulesEntries(files) {
  const entries = []
  for (const { file, text } of files) {
    let headings = [],
      block = [],
      start = 0,
      fenced = false
    const flush = () => {
      if (block.length) {
        const value = block.join('\n').trim()
        entries.push({
          file,
          line: start,
          headingPath: [...headings],
          text: value,
          id: hash(normalize(value)),
        })
        block = []
      }
    }
    text.split('\n').forEach((line, i) => {
      if (/^\s*(```|~~~)/.test(line)) {
        flush()
        fenced = !fenced
        return
      }
      if (fenced) return
      const heading = line.match(/^(#{1,6})\s+(.+)/)
      if (heading) {
        flush()
        headings = headings.slice(0, heading[1].length - 1)
        headings.push(heading[2])
        return
      }
      if (!line.trim()) {
        flush()
        return
      }
      if (/^(?:[-*+] |\d+\. )/.test(line)) flush()
      if (!block.length) start = i + 1
      block.push(line)
    })
    flush()
  }
  return entries
}
export function pickRuleOfRun(entries, rotation = {}) {
  const ids = new Set(entries.map((e) => e.id)),
    visited = (rotation.visited || []).filter((id) => ids.has(id))
  const remaining = entries.find((e) => !visited.includes(e.id))
  return {
    entry: remaining || entries[0] || null,
    cycle: (rotation.cycle || 0) + (entries.length && !remaining ? 1 : 0),
    visited: remaining ? visited : [],
  }
}
export function recordState(report, state, runId, takenAt = new Date().toISOString()) {
  const hotspots = {}
  for (const h of report.weight.hotspots)
    hotspots[h.hotspotKey] = [
      ...new Set([...(state?.hotspots?.[h.hotspotKey] || []), runId]),
    ].slice(-5)
  return {
    version: 1,
    snapshot: {
      takenAt,
      runId,
      families: Object.fromEntries(
        report.weight.weights.map((w) => [w.key, { bytes: w.bytes, lines: w.lines }]),
      ),
    },
    hotspots,
    rotation: {
      cycle: report.ruleOfRun.cycle,
      visited: [
        ...new Set([
          ...report.ruleOfRun.visited,
          ...(report.ruleOfRun.entry ? [report.ruleOfRun.entry.id] : []),
        ]),
      ],
    },
  }
}
export function toRetroNotes(report, state, runId) {
  const notes = []
  const note = (kind, repeatExempt, suggestedRung, statement, where, evidence, fix, run) => {
    if (suggestedRung !== null && !RUNG_IDS.includes(suggestedRung))
      throw new Error('unknown remedy rung')
    notes.push({
      kind,
      repeatExempt,
      suggestedRung,
      body: `${statement}\nWhere: ${where}\nWhy it matters: ${evidence}\nSuggested fix: ${fix}\nRun: ${run}`,
    })
  }
  for (const file of new Set(report.prosePins.map((p) => p.file)))
    note(
      'prose-pin',
      true,
      'prevent',
      'Prose-pin test asserts guidance wording',
      file,
      report.prosePins
        .filter((p) => p.file === file)
        .map((p) => `${p.line}:${p.kind}`)
        .join(', '),
      'Delete wording assertions; move the real rule into a helper and its tests.',
      runId,
    )
  for (const h of report.weight.hotspots)
    for (const run of new Set([...(state?.hotspots?.[h.hotspotKey] || []), runId]))
      note(
        'weight',
        false,
        'helper',
        `Guidance weight hot spot: ${h.signal}`,
        h.file,
        h.hotspotKey,
        'Move repeated procedures into a helper; reduce resident guidance.',
        run,
      )
  const entry = report.ruleOfRun.entry
  if (entry)
    note(
      'rule-of-the-run',
      true,
      null,
      'Promote one guidance rule',
      `${entry.file}:${entry.line}`,
      normalize(entry.text),
      'Judge whether this rule can become a check, helper, prevent, or be deleted.',
      runId,
    )
  return notes
}
export function gateCompare(pins, baseline = { pins: [] }) {
  const key = (p) => p.file + '\0' + p.fingerprint,
    counts = new Map()
  for (const p of baseline.pins || []) counts.set(key(p), (counts.get(key(p)) || 0) + 1)
  const added = []
  for (const p of pins) {
    const k = key(p),
      n = counts.get(k) || 0
    if (n) counts.set(k, n - 1)
    else added.push(p)
  }
  return {
    added,
    stale: [...counts].filter(([, n]) => n > 0).map(([key, count]) => ({ key, count })),
  }
}
function assertionOperands(value) {
  const args = []
  let depth = 0,
    start = 0
  for (let i = 0; i < value.length; i++) {
    if ('([{'.includes(value[i])) depth++
    if (')]}'.includes(value[i])) {
      if (!depth) {
        args.push(value.slice(start, i))
        break
      }
      depth--
    }
    if (value[i] === ',' && !depth) {
      args.push(value.slice(start, i))
      start = i + 1
    }
  }
  return args
}
function validState(state) {
  const object = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)
  const strings = (v) => Array.isArray(v) && v.every((item) => typeof item === 'string')
  const count = (v) => Number.isSafeInteger(v) && v >= 0
  return (
    object(state) &&
    state.version === 1 &&
    (state.snapshot === undefined ||
      (object(state.snapshot) &&
        typeof state.snapshot.takenAt === 'string' &&
        typeof state.snapshot.runId === 'string' &&
        object(state.snapshot.families) &&
        Object.values(state.snapshot.families).every(
          (v) => object(v) && count(v.bytes) && count(v.lines),
        ))) &&
    (state.hotspots === undefined ||
      (object(state.hotspots) && Object.values(state.hotspots).every(strings))) &&
    (state.rotation === undefined ||
      (object(state.rotation) &&
        (state.rotation.visited === undefined || strings(state.rotation.visited)) &&
        (state.rotation.cycle === undefined || count(state.rotation.cycle))))
  )
}
function readState(file, warn) {
  try {
    const state = JSON.parse(fs.readFileSync(file, 'utf8'))
    if (!validState(state)) throw new Error('invalid state shape or version')
    return state
  } catch (error) {
    if (error.code !== 'ENOENT') warn(`warning: ignoring state: ${error.message}`)
    return null
  }
}
function atomicWrite(file, value) {
  fs.mkdirSync(path.dirname(file), { recursive: true })
  const temp = file + `.${process.pid}.tmp`
  try {
    fs.writeFileSync(temp, JSON.stringify(value, null, 2) + '\n')
    fs.renameSync(temp, file)
  } finally {
    fs.rmSync(temp, { force: true })
  }
}
export function runCli(args = process.argv.slice(2), io = {}) {
  const out = io.out || ((s) => process.stdout.write(s + '\n')),
    warn = io.warn || ((s) => process.stderr.write(s + '\n'))
  try {
    const opts = {}
    for (let i = 0; i < args.length; i++) {
      const key = args[i]
      if (['--gate', '--json', '--notes', '--record'].includes(key)) opts[key.slice(2)] = true
      else if (
        [
          '--root',
          '--config',
          '--baseline',
          '--write-baseline',
          '--state-dir',
          '--run-id',
        ].includes(key) &&
        args[i + 1] &&
        !args[i + 1].startsWith('--')
      )
        opts[key.slice(2)] = args[++i]
      else throw new Error(`unknown or incomplete option ${key}`)
    }
    if ((opts.record || opts.notes) && !opts['run-id'])
      throw new Error('--record and --notes require --run-id')
    let root = path.resolve(opts.root || '.')
    if (!opts.root)
      try {
        root = execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim()
      } catch {}
    const config = opts.config ? JSON.parse(fs.readFileSync(opts.config, 'utf8')) : {}
    for (const key of Object.keys(config))
      if (!(key in defaults)) {
        warn(`warning: unknown config key ${key}`)
        delete config[key]
      }
    const files = inventory(root, config, { testsOnly: !!opts.gate })
    let common
    try {
      common = execFileSync('git', ['rev-parse', '--git-common-dir'], {
        cwd: root,
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'ignore'],
      }).trim()
    } catch {
      common = '.git'
    }
    const stateFile = path.join(
      opts['state-dir'] || path.resolve(root, common, 'boss-retro'),
      'guidance-audit.json',
    )
    const state = readState(stateFile, warn)
    const report = {
      prosePins: detectProsePins(files.tests, files.options),
      weight: weightSignals(mirrorFamilies(files.guidance), state?.snapshot, files.options),
      ruleOfRun: pickRuleOfRun(rulesEntries(files.rules), state?.rotation),
    }
    if (opts['write-baseline'])
      atomicWrite(opts['write-baseline'], {
        version: 1,
        pins: report.prosePins
          .map(({ file, kind, fingerprint }) => ({ file, kind, fingerprint }))
          .sort(
            (a, b) => a.file.localeCompare(b.file) || a.fingerprint.localeCompare(b.fingerprint),
          ),
      })
    if (opts.gate) {
      let baseline = { version: 1, pins: [] }
      if (opts.baseline)
        try {
          baseline = JSON.parse(fs.readFileSync(opts.baseline, 'utf8'))
          if (baseline.version !== 1 || !Array.isArray(baseline.pins))
            throw new Error('invalid baseline')
        } catch (error) {
          if (error.code !== 'ENOENT') throw error
        }
      const verdict = gateCompare(report.prosePins, baseline)
      for (const { key, count } of verdict.stale)
        warn(
          `warning: stale baseline entry ${key.replace('\0', ':')} (${count}); rewrite with --write-baseline`,
        )
      for (const p of verdict.added) out(`${p.file}:${p.line} ${p.kind} ${p.snippet}`)
      return verdict.added.length ? 1 : 0
    }
    if (opts.record) atomicWrite(stateFile, recordState(report, state, opts['run-id']))
    out(JSON.stringify(opts.notes ? toRetroNotes(report, state, opts['run-id']) : report, null, 2))
    return 0
  } catch (error) {
    warn(`guidance-audit: ${error.message}`)
    return 2
  }
}
if (isMainModule(import.meta.url)) process.exitCode = runCli()
