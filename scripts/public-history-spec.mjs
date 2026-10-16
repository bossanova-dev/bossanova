import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
export const DEFAULT_SPEC_DIR = path.join(REPO_ROOT, '.github', 'scripts', 'public-history')
export const DEFAULT_REPLACEMENT = '***REMOVED***'

export class UsageError extends Error {}

// ---------------------------------------------------------------------------------------------
// Spec parsing. The rules mirror git-filter-repo's own readers so the audit checks exactly what
// the filter applied.

export function readLines(file) {
  return fs.readFileSync(file, 'utf8').split(/\r?\n/)
}

function compileRegex(source, where) {
  try {
    return new RegExp(source)
  } catch (error) {
    throw new UsageError(`${where}: regex does not compile in JavaScript: ${error.message}`)
  }
}

// fnmatch-style glob, as Python's fnmatch.fnmatch applies it: `*` and `?` also match `/`.
export function globToRegex(glob) {
  return new RegExp(`^(?:${globToRegexBody(glob)})$`, 's')
}

// The unanchored regex source for a glob, for callers that search inside text.
function globToRegexBody(glob) {
  let out = ''
  for (let i = 0; i < glob.length; i++) {
    const ch = glob[i]
    if (ch === '*') out += '.*'
    else if (ch === '?') out += '.'
    else if (ch === '[') {
      const end = glob.indexOf(']', i + 1)
      if (end === -1) out += '\\['
      else {
        let body = glob.slice(i + 1, end)
        if (body.startsWith('!')) body = '^' + body.slice(1)
        out += `[${body.replace(/\\/g, '\\\\')}]`
        i = end
      }
    } else out += ch.replace(/[.+^${}()|\\/]/g, '\\$&')
  }
  return out
}

// paths.txt: blank and `#` lines skipped; `regex:`, `glob:`, `literal:` or a bare literal.
// A literal matches the path itself or anything under it, anchored at the repository root.
export function parsePathsFile(file) {
  const rules = []
  readLines(file).forEach((line, index) => {
    const where = `${file}:${index + 1}`
    if (line === '' || line.startsWith('#')) return
    if (line.includes('==>')) {
      throw new UsageError(
        `${where}: renames (==>) are not supported; this spec only removes paths`,
      )
    }
    if (line.startsWith('regex:')) {
      rules.push({ kind: 'regex', text: line, re: compileRegex(line.slice(6), where) })
    } else if (line.startsWith('glob:')) {
      const glob = line.slice(5)
      const globs = [glob]
      if (!glob.endsWith('*')) globs.push(glob + (glob.endsWith('/') ? '*' : '/*'))
      rules.push({ kind: 'glob', text: line, res: globs.map(globToRegex) })
    } else {
      const literal = line.startsWith('literal:') ? line.slice(8) : line
      rules.push({ kind: 'literal', text: line, literal })
    }
  })
  return rules
}

export function pathRuleMatches(rule, pathname) {
  if (rule.kind === 'regex') return rule.re.test(pathname)
  if (rule.kind === 'glob') return rule.res.some((re) => re.test(pathname))
  const lit = rule.literal
  if (lit === '') return true
  if (!pathname.startsWith(lit)) return false
  return lit.endsWith('/') || pathname.length === lit.length || pathname[lit.length] === '/'
}

export function matchingPathRule(rules, pathname) {
  return rules.find((rule) => pathRuleMatches(rule, pathname)) ?? null
}

// replacements.txt: git-filter-repo itself treats every non-blank line as a rule (it has no
// comment syntax), so prepare hands it a copy with `#` lines removed and this reader skips them.
export function parseReplacementsFile(file) {
  const rules = []
  readLines(file).forEach((line, index) => {
    const where = `${file}:${index + 1}`
    if (line === '' || line.startsWith('#')) return
    let lhs = line
    let replacement = DEFAULT_REPLACEMENT
    const arrow = line.lastIndexOf('==>')
    if (arrow !== -1) {
      lhs = line.slice(0, arrow)
      replacement = line.slice(arrow + 3)
    }
    if (lhs.startsWith('regex:')) {
      const re = compileRegex(lhs.slice(6), where)
      rules.push({ kind: 'regex', text: lhs, re, replacement, where })
    } else if (lhs.startsWith('glob:')) {
      const re = new RegExp(globToRegexBody(lhs.slice(5)), 's')
      rules.push({ kind: 'glob', text: lhs, re, replacement, where })
    } else {
      const literal = lhs.startsWith('literal:') ? lhs.slice(8) : lhs
      if (literal === '') return
      rules.push({ kind: 'literal', text: lhs, literal, replacement, where })
    }
  })
  return rules
}

export function replacementRuleMatches(rule, text) {
  if (rule.kind === 'literal') return text.includes(rule.literal)
  rule.re.lastIndex = 0
  return rule.re.test(text)
}

// The literal text a rule's replacement leaves behind, used to find tip files the filter
// changed. Group references (\1, \g<name>) are removed; a marker shorter than 4 characters is
// too unspecific to search for.
export function replacementMarker(rule) {
  const marker = rule.replacement.replace(/\\g<[^>]*>|\\[0-9]+/g, '')
  return marker.length >= 4 ? marker : null
}

// secret-allowlist.txt: `<path glob> <regex>` per line. A secret-scan hit is allowlisted when
// one of the blob's paths matches the glob and the matched text matches the regex.
export function parseAllowlistFile(file) {
  if (!fs.existsSync(file)) return []
  const entries = []
  readLines(file).forEach((line, index) => {
    const where = `${file}:${index + 1}`
    const trimmed = line.trim()
    if (trimmed === '' || trimmed.startsWith('#')) return
    const match = /^(\S+)\s+(.+)$/.exec(trimmed)
    if (!match) throw new UsageError(`${where}: expected "<path glob> <regex>"`)
    entries.push({
      text: trimmed,
      pathRe: globToRegex(match[1].replace(/\*\*/g, '*')),
      re: compileRegex(match[2], where),
    })
  })
  return entries
}

export function loadSpec(specDir = DEFAULT_SPEC_DIR) {
  const dir = path.resolve(specDir)
  const files = {
    paths: path.join(dir, 'paths.txt'),
    replacements: path.join(dir, 'replacements.txt'),
    mailmap: path.join(dir, 'mailmap'),
    allowlist: path.join(dir, 'secret-allowlist.txt'),
  }
  for (const key of ['paths', 'replacements']) {
    if (!fs.existsSync(files[key])) throw new UsageError(`spec file missing: ${files[key]}`)
  }
  const pathRules = parsePathsFile(files.paths)
  const replacements = parseReplacementsFile(files.replacements)
  // A replacement that re-creates a left-hand side would make the audit fail on every run.
  for (const rule of replacements) {
    for (const other of replacements) {
      if (replacementRuleMatches(other, rule.replacement)) {
        throw new UsageError(
          `${rule.where}: replacement text "${rule.replacement}" matches the rule ${other.text}`,
        )
      }
    }
  }
  return {
    dir,
    files,
    pathRules,
    replacements,
    hasMailmap: fs.existsSync(files.mailmap),
    allowlist: parseAllowlistFile(files.allowlist),
  }
}

// The export boundary: PRIVATE_PATHS=( ... ) in .github/scripts/export-public-tree.sh.
export function parsePrivatePaths(scriptText) {
  const match = /^PRIVATE_PATHS=\(([^)]*)\)/m.exec(scriptText)
  if (!match) throw new Error('PRIVATE_PATHS=( ... ) not found in the export script')
  return match[1].split(/\s+/).filter(Boolean)
}

// Entries of the export boundary that paths.txt does not remove (as a path and as a directory).
export function uncoveredPrivatePaths(privatePaths, pathRules) {
  return privatePaths.filter(
    (entry) =>
      !matchingPathRule(pathRules, entry) || !matchingPathRule(pathRules, `${entry}/nested`),
  )
}
