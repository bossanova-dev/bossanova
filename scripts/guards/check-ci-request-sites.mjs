// guard-id: ci-request-sites
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { extractFencedBlocks, SHELL_INFO_STRINGS } from './check-skill-shell.mjs'
import { trackedFiles } from '../dev/tree-scan-lib.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const SKILLS = 'lib/bossalib/contract/plugin/skillpayload/skills/'
const PUSH =
  /\bgit(?:\s+-C\s+(?:"[^"]*"|'[^']*'|\S+))?\s+push\b|\bnode\s+[^\n;]*\bpush-branch\.mjs\b/
const REQUEST = /\bnode\s+[^\n;]*\bci\/ci-request\.mjs["']?\s+request\b/
const CODE_PUSH =
  /\b(?:git|tryGit)\s*\(\s*(?:\[\s*)?['"]push['"]|\[\s*['"]push['"]|\b(?:execFileSync|execFile|spawnSync|spawn)\s*\(\s*['"]git['"]\s*,\s*\[\s*['"]push['"]/

function skipReason(text, markdown = false) {
  const pattern = markdown
    ? /<!--\s*ci-request: skip[ \t]+([^\n]*?)\s*-->/g
    : /(?:\/\/|#)[ \t]*ci-request: skip[ \t]+([^\n]*)/g
  for (const match of text.matchAll(pattern)) {
    const reason = match[1].trim()
    if (reason) return `skip ${reason}`
  }
  return null
}

function groupedSites(file, lines, offset, matches, disposition) {
  const sites = []
  for (let i = 0; i < lines.length; i++) {
    if (!matches(lines[i])) continue
    let end = i
    while (end + 1 < lines.length && matches(lines[end + 1])) end++
    sites.push({
      file,
      line: offset + i,
      command: lines
        .slice(i, end + 1)
        .join(' ')
        .trim(),
      disposition: disposition(end, i),
    })
    i = end
  }
  return sites
}

export function scanSites(file, text) {
  const lines = text.split(/\r?\n/)
  if (!file.endsWith('.md')) {
    const matches = (line) =>
      !/^\s*(?:\/\/|#)/.test(line) && (file.endsWith('.sh') ? PUSH : CODE_PUSH).test(line)
    return groupedSites(
      file,
      lines,
      1,
      matches,
      (end, start) =>
        skipReason(lines.slice(Math.max(0, start - 1), end + 1).join('\n')) ?? 'unaccounted',
    )
  }
  const sites = []
  const fenced = new Set()
  for (const block of extractFencedBlocks(text)) {
    const body = block.body.split('\n')
    const last = block.startLine + body.length
    for (let line = block.startLine; line <= last; line++) fenced.add(line)
    if (!SHELL_INFO_STRINGS.has(block.info)) continue
    sites.push(
      ...groupedSites(
        file,
        body,
        block.startLine + 1,
        (line) => !/^\s*#/.test(line) && PUSH.test(line),
        (end) => {
          const skip = skipReason(block.body)
          return (
            skip ??
            (body.slice(end + 1).some((line) => !/^\s*#/.test(line) && REQUEST.test(line))
              ? 'request'
              : 'unaccounted')
          )
        },
      ),
    )
  }
  for (let i = 0; i < lines.length; i++) {
    if (fenced.has(i + 1) || !lines[i].trim()) continue
    const start = i
    while (
      i + 1 < lines.length &&
      lines[i + 1].trim() &&
      !fenced.has(i + 2) &&
      !/^\s*(?:[-*+]\s|\d+[.)]\s|\|)/.test(lines[i + 1])
    )
      i++
    const scope = lines.slice(start, i + 1)
    const scopeText = scope.join('\n')
    const code = (line) => [...line.matchAll(/(`+)([^`]*?)\1/g)].map((match) => match[2])
    sites.push(
      ...groupedSites(
        file,
        scope,
        start + 1,
        (line) => code(line).some((span) => PUSH.test(span)),
        (end) =>
          skipReason(scopeText, true) ??
          (scope.slice(end + 1).some((line) => code(line).some((span) => REQUEST.test(span)))
            ? 'request'
            : 'unaccounted'),
      ),
    )
  }
  return sites.sort((a, b) => a.line - b.line)
}

/** @param {{root?: string, files?: string[]}} [options] */
export function checkSites({ root = ROOT, files } = {}) {
  const paths =
    files ??
    trackedFiles(root).filter(
      (file) =>
        (file.startsWith(SKILLS) && file.endsWith('.md') && !file.includes('/toolbox/')) ||
        (file.startsWith('skills-toolbox/') &&
          /\.(?:mjs|sh)$/.test(file) &&
          !file.endsWith('.test.mjs')),
    )
  return paths.flatMap((file) => scanSites(file, readFileSync(path.resolve(root, file), 'utf8')))
}

if (isMainModule(import.meta.url)) {
  try {
    const { values, positionals } = parseArgs({
      strict: true,
      allowPositionals: true,
      options: { list: { type: 'boolean' }, files: { type: 'boolean' } },
    })
    if (positionals.length && !values.files) throw new Error('paths require --files')
    if (values.files && positionals.length === 0) throw new Error('--files needs paths')
    const sites = checkSites({ files: values.files ? positionals : undefined })
    for (const site of sites) {
      const line = `${site.file}:${site.line}: ${site.disposition}: ${site.command}`
      if (values.list) console.log(line)
      if (site.disposition === 'unaccounted') console.error(line)
    }
    process.exitCode = sites.some((site) => site.disposition === 'unaccounted') ? 1 : 0
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 2
  }
}
