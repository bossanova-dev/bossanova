#!/usr/bin/env node

// Prepare and audit a filtered full-history copy of this repository for publication.
//
//   prepare --source <path-or-url> --scratch <dir> [--spec <dir>] [--keep-ref <pattern> ...]
//           [--filter-repo <bin>]
//   audit   --repo <scratch> [--spec <dir>] --report <file>
//
// prepare mirror-clones the source into an empty scratch directory, drops every ref that is not
// kept, removes every remote, and runs git-filter-repo there with the spec in
// .github/scripts/public-history/. It never pushes: publication is a human step (see that
// directory's README). audit reads a prepared (or any) repository and writes a Markdown report;
// it exits 1 when a hard check fails, 0 when none does, and 2 on a usage or runtime error.

import { spawn, spawnSync } from 'node:child_process'
import { once } from 'node:events'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
export const DEFAULT_SPEC_DIR = path.join(REPO_ROOT, '.github', 'scripts', 'public-history')
export const DEFAULT_REPLACEMENT = '***REMOVED***'
const DEFAULT_TAG_KEEP = 'refs/tags/v*'
const MAX_BUFFER = 1024 * 1024 * 1024
const LARGEST_BLOBS = 20
const LIST_CAP = 50

// Variables that would point git at a repository other than the one named by cwd. A caller
// running inside a git hook inherits them, and they must never redirect the scratch clone.
const GIT_LOCATION_ENV = [
  'GIT_DIR',
  'GIT_WORK_TREE',
  'GIT_INDEX_FILE',
  'GIT_OBJECT_DIRECTORY',
  'GIT_ALTERNATE_OBJECT_DIRECTORIES',
  'GIT_COMMON_DIR',
  'GIT_NAMESPACE',
  'GIT_PREFIX',
]

export class UsageError extends Error {}

function gitEnv() {
  const env = { ...process.env }
  for (const name of GIT_LOCATION_ENV) delete env[name]
  return env
}

function git(cwd, args, { allowFailure = false, input } = {}) {
  const result = spawnSync('git', args, {
    cwd,
    env: gitEnv(),
    input,
    maxBuffer: MAX_BUFFER,
  })
  if (result.error) throw result.error
  if (result.status !== 0 && !allowFailure) {
    const stderr = result.stderr.toString('utf8').trim()
    throw new Error(`git ${args.join(' ')} failed (exit ${result.status}): ${stderr}`)
  }
  return { status: result.status, stdout: result.stdout, text: result.stdout.toString('utf8') }
}

// ---------------------------------------------------------------------------------------------
// Spec parsing. The rules mirror git-filter-repo's own readers so the audit checks exactly what
// the filter applied.

function readLines(file) {
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

// ---------------------------------------------------------------------------------------------
// prepare

function nearestExistingDir(target) {
  let current = target
  while (!fs.existsSync(current)) {
    const parent = path.dirname(current)
    if (parent === current) return current
    current = parent
  }
  return current
}

function realpathOrSelf(target) {
  try {
    return fs.realpathSync(target)
  } catch {
    return target
  }
}

function isWithin(child, parent) {
  const rel = path.relative(parent, child)
  return rel === '' || (!rel.startsWith('..') && !path.isAbsolute(rel))
}

// The scratch path with its existing prefix resolved through symlinks, so /tmp vs /private/tmp
// and similar aliases cannot slip past the containment checks.
function resolvedScratch(scratch) {
  const anchor = nearestExistingDir(scratch)
  return path.join(realpathOrSelf(anchor), path.relative(anchor, scratch))
}

export function scratchRefusal({ source, scratch }) {
  if (fs.existsSync(scratch)) {
    if (!fs.statSync(scratch).isDirectory())
      return `scratch ${scratch} exists and is not a directory`
    if (fs.readdirSync(scratch).length > 0) return `scratch ${scratch} exists and is not empty`
  }
  const resolved = resolvedScratch(scratch)
  if (fs.existsSync(source)) {
    const roots = []
    for (const flag of ['--show-toplevel', '--absolute-git-dir', '--git-common-dir']) {
      const out = git(source, ['rev-parse', '--path-format=absolute', flag], { allowFailure: true })
      if (out.status === 0 && out.text.trim()) roots.push(realpathOrSelf(out.text.trim()))
    }
    roots.push(realpathOrSelf(path.resolve(source)))
    const inside = roots.find((root) => isWithin(resolved, root))
    if (inside) return `scratch ${scratch} lies inside the source repository (${inside})`
  }
  const anchor = nearestExistingDir(scratch)
  const probe = git(anchor, ['rev-parse', '--is-inside-work-tree', '--is-inside-git-dir'], {
    allowFailure: true,
  })
  if (probe.status === 0 && probe.text.split('\n').includes('true')) {
    return `scratch ${scratch} lies inside a git repository or work tree (${anchor})`
  }
  return null
}

function refPatternToRegex(pattern) {
  const full = pattern.startsWith('refs/')
    ? [pattern]
    : [`refs/heads/${pattern}`, `refs/tags/${pattern}`]
  return full.map((p) => globToRegex(p))
}

export function selectKeptRefs(refs, patterns) {
  const res = patterns.flatMap(refPatternToRegex)
  return refs.filter((ref) => res.some((re) => re.test(ref)))
}

// Copies of the spec in the exact shape git-filter-repo reads: replacements without comments.
function writeFilterInputs(spec, dir) {
  const inputs = {
    paths: path.join(dir, 'paths.txt'),
    replacements: path.join(dir, 'replacements.txt'),
  }
  fs.copyFileSync(spec.files.paths, inputs.paths)
  const replacementLines = readLines(spec.files.replacements).filter(
    (line) => line !== '' && !line.startsWith('#'),
  )
  fs.writeFileSync(inputs.replacements, replacementLines.join('\n') + '\n')
  if (spec.hasMailmap) {
    inputs.mailmap = path.join(dir, 'mailmap')
    fs.copyFileSync(spec.files.mailmap, inputs.mailmap)
  }
  return inputs
}

export function filterRepoArgs(inputs) {
  const args = [
    '--force',
    '--replace-refs',
    'delete-no-add',
    '--invert-paths',
    '--paths-from-file',
    inputs.paths,
    '--replace-text',
    inputs.replacements,
    '--replace-message',
    inputs.replacements,
  ]
  if (inputs.mailmap) args.push('--mailmap', inputs.mailmap)
  return args
}

export function prepare({
  source,
  scratch,
  spec: specDir = DEFAULT_SPEC_DIR,
  keepRefs = [],
  filterRepo = 'git-filter-repo',
  log = (line) => process.stderr.write(`${line}\n`),
}) {
  if (!source) throw new UsageError('prepare needs --source')
  if (!scratch) throw new UsageError('prepare needs --scratch')
  const spec = loadSpec(specDir)
  const scratchDir = path.resolve(scratch)
  const refusal = scratchRefusal({ source, scratch: scratchDir })
  if (refusal) throw new UsageError(`refusing to prepare: ${refusal}`)

  fs.mkdirSync(scratchDir, { recursive: true })
  const cloneSource = fs.existsSync(source) ? path.resolve(source) : source
  git(path.dirname(scratchDir), [
    'clone',
    '--quiet',
    '--mirror',
    '--no-local',
    '--',
    cloneSource,
    scratchDir,
  ])

  const head = git(scratchDir, ['symbolic-ref', '-q', 'HEAD'], { allowFailure: true }).text.trim()
  const patterns = keepRefs.length > 0 ? keepRefs : [head, DEFAULT_TAG_KEEP].filter(Boolean)
  const refs = git(scratchDir, ['for-each-ref', '--format=%(refname)'])
    .text.split('\n')
    .filter(Boolean)
  const kept = selectKeptRefs(refs, patterns)
  if (kept.length === 0) {
    throw new UsageError(`no ref in ${cloneSource} matches --keep-ref ${patterns.join(', ')}`)
  }
  const dropped = refs.filter((ref) => !kept.includes(ref))
  if (dropped.length > 0) {
    git(scratchDir, ['update-ref', '--stdin'], {
      input: dropped.map((ref) => `delete ${ref}\n`).join(''),
    })
  }
  const keptBranches = kept.filter((ref) => ref.startsWith('refs/heads/'))
  if (!kept.includes(head) && keptBranches.length > 0) {
    git(scratchDir, ['symbolic-ref', 'HEAD', keptBranches[0]])
  }

  // Remove the remote configuration directly: `git remote remove` on a mirror clone also walks
  // its `+refs/*:refs/*` refspec, and the refs were already pruned above.
  for (const remote of git(scratchDir, ['remote']).text.split('\n').filter(Boolean)) {
    git(scratchDir, ['config', '--remove-section', `remote.${remote}`])
  }
  if (git(scratchDir, ['remote']).text.trim() !== '') {
    throw new Error(`remotes remain in ${scratchDir}`)
  }

  const inputDir = fs.mkdtempSync(path.join(os.tmpdir(), 'public-history-spec-'))
  try {
    const inputs = writeFilterInputs(spec, inputDir)
    const args = filterRepoArgs(inputs)
    log(`running ${filterRepo} in ${scratchDir}`)
    const run = spawnSync(filterRepo, args, {
      cwd: scratchDir,
      env: gitEnv(),
      encoding: 'utf8',
      maxBuffer: MAX_BUFFER,
    })
    for (const line of `${run.stdout ?? ''}${run.stderr ?? ''}`.split('\n').filter(Boolean)) {
      log(`  ${line}`)
    }
    if (run.error) {
      const hint =
        run.error.code === 'ENOENT' ? ' (install git-filter-repo or pass --filter-repo)' : ''
      throw new Error(`cannot run ${filterRepo}: ${run.error.message}${hint}`)
    }
    if (run.status !== 0) throw new Error(`${filterRepo} exited ${run.status}`)
  } finally {
    fs.rmSync(inputDir, { recursive: true, force: true })
  }

  const finalRefs = git(scratchDir, ['for-each-ref', '--format=%(refname)'])
    .text.split('\n')
    .filter(Boolean)
  const remotes = git(scratchDir, ['remote']).text.split('\n').filter(Boolean)
  if (remotes.length > 0)
    throw new Error(`remotes reappeared in ${scratchDir}: ${remotes.join(', ')}`)
  log(`prepared ${scratchDir}: kept ${finalRefs.join(', ')}; dropped ${dropped.length} refs`)
  return { scratch: scratchDir, keptRefs: finalRefs, droppedRefs: dropped }
}

// ---------------------------------------------------------------------------------------------
// audit: history walk

// Every (commit, path, blob) a tree in any ref ever held. `-m --root` makes each path appear in
// the diff of the commit that introduced it against every parent, so no tree entry is missed.
function walkHistory(repo) {
  const out = git(repo, [
    '-c',
    'core.quotePath=false',
    'log',
    '--all',
    '--no-renames',
    '-m',
    '--root',
    '--raw',
    '--no-abbrev',
    '-z',
    '--format=%x01%H',
  ]).stdout
  const pathsSeen = new Map() // path -> { commit, commits: Set }
  const blobs = new Map() // oid -> { commit, path }
  let commit = null
  let meta = null
  let start = 0
  for (let i = 0; i <= out.length; i++) {
    if (i < out.length && out[i] !== 0) continue
    let token = out.subarray(start, i).toString('utf8')
    start = i + 1
    if (token.startsWith('\n')) token = token.slice(1)
    if (token === '') continue
    if (token.startsWith('\x01')) {
      commit = token.slice(1)
      continue
    }
    if (meta === null && token.startsWith(':')) {
      meta = token
      continue
    }
    if (meta !== null) {
      const [, newMode, , newOid, status] = meta.slice(1).split(' ')
      meta = null
      if (status === 'D') continue
      const entry = pathsSeen.get(token) ?? { commit, commits: new Set() }
      entry.commit = commit // log is newest-first: the last write is the oldest commit
      entry.commits.add(commit)
      pathsSeen.set(token, entry)
      // A gitlink (submodule) path is still a path the history holds, so it stays in pathsSeen
      // for the path checks; its oid names a commit in another repository, not a blob here.
      if (newMode !== '160000') blobs.set(newOid, { commit, path: token })
    }
  }
  return { pathsSeen, blobs }
}

function readCommits(repo) {
  const out = git(repo, ['log', '--all', '--format=%x02%H%x00%an <%ae>%x00%cn <%ce>%x00%B']).text
  return out
    .split('\x02')
    .filter(Boolean)
    .map((record) => {
      const [sha, author, committer, ...message] = record.split('\x00')
      return { sha, author, committer, message: message.join('\x00') }
    })
}

function readTags(repo) {
  const out = git(repo, [
    'for-each-ref',
    'refs/tags',
    '--format=%02%(refname)%00%(objecttype)%00%(taggername) %(taggeremail)%00%(contents)',
  ]).text
  return out
    .split('\x02')
    .filter(Boolean)
    .map((record) => {
      const [ref, type, tagger, ...message] = record.split('\x00')
      return { ref, annotated: type === 'tag', tagger, message: message.join('\x00') }
    })
    .filter((tag) => tag.annotated)
}

class ByteQueue {
  constructor() {
    this.chunks = []
    this.length = 0
  }
  push(chunk) {
    this.chunks.push(chunk)
    this.length += chunk.length
  }
  indexOfNewline() {
    let offset = 0
    for (const chunk of this.chunks) {
      const at = chunk.indexOf(10)
      if (at !== -1) return offset + at
      offset += chunk.length
    }
    return -1
  }
  take(n) {
    const parts = []
    let need = n
    while (need > 0) {
      const head = this.chunks[0]
      if (head.length <= need) {
        parts.push(head)
        this.chunks.shift()
        need -= head.length
      } else {
        parts.push(head.subarray(0, need))
        this.chunks[0] = head.subarray(need)
        need = 0
      }
    }
    this.length -= n
    return parts.length === 1 ? parts[0] : Buffer.concat(parts, n)
  }
}

// Streams `git cat-file --batch` so a large history never has to fit in one buffer.
async function forEachBlob(repo, oids, visit) {
  const child = spawn('git', ['cat-file', '--batch'], { cwd: repo, env: gitEnv() })
  const stderr = []
  child.stderr.on('data', (chunk) => stderr.push(chunk))
  const exited = once(child, 'close')
  const writer = (async () => {
    for (const oid of oids) {
      if (!child.stdin.write(`${oid}\n`)) await once(child.stdin, 'drain')
    }
    child.stdin.end()
  })()
  const queue = new ByteQueue()
  const unreadable = [] // an object the walk named but cat-file could not return
  let header = null
  for await (const chunk of child.stdout) {
    queue.push(chunk)
    for (;;) {
      if (header === null) {
        const nl = queue.indexOfNewline()
        if (nl === -1) break
        const line = queue
          .take(nl + 1)
          .toString('utf8')
          .trimEnd()
        const [oid, type, size] = line.split(' ')
        if (type === 'missing' || size === undefined) {
          unreadable.push(line)
          continue
        }
        header = { oid, type, size: Number(size) }
      }
      if (queue.length < header.size + 1) break
      const data = queue.take(header.size)
      queue.take(1)
      visit(header.oid, data, header.size)
      header = null
    }
  }
  await writer
  const [code] = await exited
  if (code !== 0)
    throw new Error(`git cat-file --batch failed: ${Buffer.concat(stderr).toString()}`)
  // A partial or damaged clone can list a tree entry whose blob is absent; passing it unscanned
  // would let the audit pass history it never read.
  if (unreadable.length > 0) {
    throw new Error(
      `${unreadable.length} history object(s) could not be read (missing or unparseable), ` +
        `so their content was not scanned: ${unreadable.slice(0, 5).join(', ')}`,
    )
  }
}

// ---------------------------------------------------------------------------------------------
// audit: scanners

export const SECRET_FAMILIES = [
  {
    family: 'github-token',
    re: /\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})\b/g,
  },
  { family: 'slack-token', re: /\bxox[abeprs]-[A-Za-z0-9-]{10,}\b/g },
  { family: 'aws-access-key', re: /\b(?:AKIA|ASIA)[0-9A-Z]{16}\b/g },
  { family: 'google-api-key', re: /\bAIza[0-9A-Za-z_-]{35}\b/g },
  { family: 'stripe-key', re: /\b(?:sk|rk)_live_[0-9A-Za-z]{16,}\b/g },
  { family: 'anthropic-key', re: /\bsk-ant-[A-Za-z0-9_-]{20,}/g },
  { family: 'openai-key', re: /\bsk-(?:proj-)?[A-Za-z0-9]{32,}\b/g },
  { family: 'linear-key', re: /\blin_(?:api|oauth)_[A-Za-z0-9]{32,}\b/g },
  { family: 'npm-token', re: /\bnpm_[A-Za-z0-9]{36}\b/g },
  { family: 'sentry-token', re: /\bsntr[ysu]_[A-Za-z0-9_=+/-]{40,}/g },
  {
    family: 'private-key',
    re: /-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY( BLOCK)?-----/g,
  },
  { family: 'jwt', re: /\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}/g },
  {
    family: 'credentialed-url',
    re: /\b[a-z][a-z0-9+.-]{1,15}:\/\/[^\s/:@'"`<>]{1,64}:[^\s/:@'"`<>]{1,128}@[A-Za-z0-9.-]+/g,
  },
]

const EMAIL_RE = /[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b/g
const HOME_RE = /\/(Users|home)\/([A-Za-z0-9._-]+)/g
const HOSTNAME_RE =
  /\b[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9-]+)*\.(?:svc\.cluster\.local|cluster\.local)\b|\b[a-z0-9-]+(?:\.[a-z0-9-]+)+\.internal\b/g

export function redact(value) {
  return `${value.slice(0, 6)}… (${value.length} chars)`
}

function isBinary(data) {
  return data.subarray(0, 8192).includes(0)
}

function bump(map, key, init) {
  const entry = map.get(key) ?? init()
  map.set(key, entry)
  return entry
}

// Tallies every address in text under its lower-cased domain: domain -> Map(address -> count).
function countEmails(emails, text) {
  for (const match of text.matchAll(EMAIL_RE)) {
    const address = match[0]
    const domain = address.split('@').pop().toLowerCase()
    const perDomain = bump(emails, domain, () => new Map())
    perDomain.set(address, (perDomain.get(address) ?? 0) + 1)
  }
}

function short(sha) {
  return sha ? sha.slice(0, 12) : '(none)'
}

// Report-only scans: they never fail the audit, they feed the report's review sections.
function newReportScans() {
  return {
    secretHits: new Map(), // family|match|path -> hit
    allowlisted: new Map(),
    emails: new Map(), // domain -> Map(address -> count)
    homes: new Map(), // user -> { count, commit, path }
    hostnames: new Map(), // host -> { count, commit, path }
  }
}

function scanReportOnly(scans, allowlist, text, origin) {
  for (const { family, re } of SECRET_FAMILIES) {
    for (const match of text.matchAll(re)) {
      const value = match[0]
      const allow = allowlist.find((a) => a.pathRe.test(origin.path) && a.re.test(value))
      const target = allow ? scans.allowlisted : scans.secretHits
      const hit = bump(target, `${family}\0${value}\0${origin.path}`, () => ({
        family,
        sample: redact(value),
        commit: origin.commit,
        path: origin.path,
        allowedBy: allow?.text,
        count: 0,
      }))
      hit.count += 1
    }
  }
  countEmails(scans.emails, text)
  for (const match of text.matchAll(HOME_RE)) {
    const user = `/${match[1]}/${match[2]}`
    const entry = bump(scans.homes, user, () => ({
      count: 0,
      commit: origin.commit,
      path: origin.path,
    }))
    entry.count += 1
  }
  for (const match of text.matchAll(HOSTNAME_RE)) {
    const entry = bump(scans.hostnames, match[0], () => ({
      count: 0,
      commit: origin.commit,
      path: origin.path,
    }))
    entry.count += 1
  }
}

export async function audit({ repo, spec: specDir = DEFAULT_SPEC_DIR, report }) {
  if (!repo) throw new UsageError('audit needs --repo')
  if (!report) throw new UsageError('audit needs --report')
  const spec = loadSpec(specDir)
  const repoDir = path.resolve(repo)
  if (git(repoDir, ['rev-parse', '--git-dir'], { allowFailure: true }).status !== 0) {
    throw new UsageError(`${repoDir} is not a git repository`)
  }

  const hard = []
  const { pathsSeen, blobs } = walkHistory(repoDir)

  // Hard: forbidden paths in any commit of any ref.
  for (const [pathname, entry] of [...pathsSeen].sort(([a], [b]) => a.localeCompare(b))) {
    const rule = matchingPathRule(spec.pathRules, pathname)
    if (rule) {
      hard.push({
        check: 'forbidden-path',
        rule: rule.text,
        commit: entry.commit,
        path: pathname,
        where: `path (in ${entry.commits.size} commit(s))`,
      })
    }
  }

  // Hard: replacement left-hand sides in paths, messages, identities.
  const lhsHit = (where, text, commit, pathname) => {
    for (const rule of spec.replacements) {
      if (replacementRuleMatches(rule, text)) {
        hard.push({ check: 'replacement-lhs', rule: rule.text, commit, path: pathname, where })
      }
    }
  }
  for (const [pathname, entry] of pathsSeen) lhsHit('path', pathname, entry.commit, pathname)
  const commits = readCommits(repoDir)
  for (const c of commits) {
    lhsHit('commit message', c.message, c.sha, '')
    lhsHit('author identity', c.author, c.sha, '')
    lhsHit('committer identity', c.committer, c.sha, '')
  }
  const tags = readTags(repoDir)
  for (const tag of tags) {
    lhsHit('tag message', tag.message, '', tag.ref)
    lhsHit('tagger identity', tag.tagger, '', tag.ref)
  }

  // Tip: HEAD's tree, for history-only paths and replacement markers.
  const tipTree = git(repoDir, ['-c', 'core.quotePath=false', 'ls-tree', '-r', '-z', 'HEAD'], {
    allowFailure: true,
  })
  const tipBlobs = new Map() // oid -> [paths]
  const tipPaths = new Set()
  if (tipTree.status === 0) {
    for (const record of tipTree.text.split('\0').filter(Boolean)) {
      const tab = record.indexOf('\t')
      const [, type, oid] = record.slice(0, tab).split(' ')
      const pathname = record.slice(tab + 1)
      tipPaths.add(pathname)
      if (type === 'blob') bump(tipBlobs, oid, () => []).push(pathname)
    }
  }
  const markers = spec.replacements
    .map((rule) => ({ rule, marker: replacementMarker(rule) }))
    .filter((m) => m.marker)

  // Blob pass: hard LHS check, report-only scans, sizes, tip markers.
  const scans = newReportScans()
  const { secretHits, allowlisted, emails, homes, hostnames } = scans
  const sizes = []
  const tipChanged = new Map() // path -> Set(rule)
  await forEachBlob(repoDir, [...blobs.keys()], (oid, data, size) => {
    const origin = blobs.get(oid)
    sizes.push({ oid, size, path: origin.path, commit: origin.commit })
    const binary = isBinary(data)
    const text = data.toString(binary ? 'latin1' : 'utf8')
    for (const rule of spec.replacements) {
      if (replacementRuleMatches(rule, text)) {
        hard.push({
          check: 'replacement-lhs',
          rule: rule.text,
          commit: origin.commit,
          path: origin.path,
          where: binary ? 'blob (binary; filter-repo skips binary blobs)' : 'blob',
        })
      }
    }
    const tipAt = tipBlobs.get(oid)
    if (tipAt && !binary) {
      for (const { rule, marker } of markers) {
        if (text.includes(marker)) {
          for (const p of tipAt) bump(tipChanged, p, () => new Set()).add(rule.text)
        }
      }
    }
    if (!binary) scanReportOnly(scans, spec.allowlist, text, origin)
  })
  for (const c of commits) {
    countEmails(emails, `${c.author}\n${c.committer}`)
  }

  const refs = git(repoDir, ['for-each-ref', '--format=%(refname) %(objectname)'])
    .text.split('\n')
    .filter(Boolean)
  const counts = Object.fromEntries(
    git(repoDir, ['count-objects', '-v'])
      .text.split('\n')
      .filter(Boolean)
      .map((line) => line.split(': ')),
  )
  const commitCount = Number(git(repoDir, ['rev-list', '--all', '--count']).text.trim() || 0)
  const historyOnly = [...pathsSeen.keys()].filter((p) => !tipPaths.has(p)).sort()
  sizes.sort((a, b) => b.size - a.size)

  const result = {
    repo: repoDir,
    spec: spec.dir,
    hardFindings: hard,
    secretHits: [...secretHits.values()],
    allowlisted: [...allowlisted.values()],
    emails,
    homes,
    hostnames,
    refs,
    commitCount,
    objectCount: Number(counts.count ?? 0) + Number(counts['in-pack'] ?? 0),
    packedKiB: Number(counts['size-pack'] ?? 0),
    largestBlobs: sizes.slice(0, LARGEST_BLOBS),
    historyOnly,
    tipChanged: [...tipChanged]
      .map(([p, rules]) => ({ path: p, rules: [...rules] }))
      .sort((a, b) => a.path.localeCompare(b.path)),
  }
  fs.mkdirSync(path.dirname(path.resolve(report)), { recursive: true })
  fs.writeFileSync(report, renderReport(result))
  return result
}

// ---------------------------------------------------------------------------------------------
// report

function cell(value) {
  return String(value ?? '')
    .replace(/\|/g, '\\|')
    .replace(/\n/g, ' ')
}

function table(headers, rows) {
  if (rows.length === 0) return '_None._\n'
  const lines = [`| ${headers.join(' | ')} |`, `| ${headers.map(() => '---').join(' | ')} |`]
  for (const row of rows) lines.push(`| ${row.map(cell).join(' | ')} |`)
  return lines.join('\n') + '\n'
}

function code(value) {
  return value ? `\`${String(value).replace(/`/g, "'")}\`` : ''
}

export function renderReport(r) {
  const out = []
  const verdict =
    r.hardFindings.length === 0
      ? 'PASS — no hard findings'
      : `FAIL — ${r.hardFindings.length} hard finding(s)`
  out.push('# Public history audit\n')
  out.push(`- Repository: ${code(r.repo)}`)
  out.push(`- Spec: ${code(r.spec)}`)
  out.push(`- Verdict: **${verdict}**\n`)

  out.push('## Hard checks\n')
  const byCheck = (check) => r.hardFindings.filter((f) => f.check === check)
  out.push(`### Forbidden paths (paths.txt) — ${byCheck('forbidden-path').length}\n`)
  out.push(
    table(
      ['Path', 'Rule', 'Oldest commit', 'Where'],
      byCheck('forbidden-path').map((f) => [
        code(f.path),
        code(f.rule),
        code(short(f.commit)),
        f.where,
      ]),
    ),
  )
  out.push(
    `### Replacement left-hand sides (replacements.txt) — ${byCheck('replacement-lhs').length}\n`,
  )
  out.push(
    table(
      ['Rule', 'Where', 'Commit', 'Path or ref'],
      byCheck('replacement-lhs').map((f) => [
        code(f.rule),
        f.where,
        code(f.commit ? short(f.commit) : ''),
        code(f.path),
      ]),
    ),
  )

  out.push('## Report-only (human triage)\n')
  out.push(`### Secret scan — ${r.secretHits.length} hit(s), ${r.allowlisted.length} allowlisted\n`)
  out.push('Samples are redacted. Rotate anything live before publishing.\n')
  out.push(
    table(
      ['Family', 'Commit', 'Path', 'Sample', 'Occurrences'],
      r.secretHits.map((h) => [
        h.family,
        code(short(h.commit)),
        code(h.path),
        code(h.sample),
        h.count,
      ]),
    ),
  )
  out.push('#### Allowlisted (secret-allowlist.txt)\n')
  out.push(
    table(
      ['Family', 'Commit', 'Path', 'Sample', 'Allowlist entry'],
      r.allowlisted.map((h) => [
        h.family,
        code(short(h.commit)),
        code(h.path),
        code(h.sample),
        code(h.allowedBy),
      ]),
    ),
  )

  out.push('### Email addresses by domain\n')
  out.push(
    table(
      ['Domain', 'Addresses', 'Occurrences', 'Examples'],
      [...r.emails]
        .map(([domain, addrs]) => {
          const list = [...addrs.keys()].sort()
          const total = [...addrs.values()].reduce((a, b) => a + b, 0)
          const shown = list.slice(0, LIST_CAP).join(', ')
          const more = list.length > LIST_CAP ? ` … and ${list.length - LIST_CAP} more` : ''
          return [domain, list.length, total, shown + more]
        })
        .sort((a, b) => b[2] - a[2]),
    ),
  )
  out.push('### Home-directory paths by user\n')
  out.push(
    table(
      ['Home', 'Occurrences', 'First seen (commit)', 'Path'],
      [...r.homes]
        .sort((a, b) => b[1].count - a[1].count)
        .map(([home, e]) => [code(home), e.count, code(short(e.commit)), code(e.path)]),
    ),
  )
  out.push('### Cluster-internal hostnames\n')
  out.push(
    table(
      ['Hostname', 'Occurrences', 'Commit', 'Path'],
      [...r.hostnames]
        .sort((a, b) => b[1].count - a[1].count)
        .map(([host, e]) => [code(host), e.count, code(short(e.commit)), code(e.path)]),
    ),
  )

  out.push('## Size and shape\n')
  out.push(`- Commits: ${r.commitCount}`)
  out.push(`- Objects: ${r.objectCount}`)
  out.push(`- Packed size: ${(r.packedKiB / 1024).toFixed(2)} MiB\n`)
  out.push(`### Kept refs — ${r.refs.length}\n`)
  out.push(
    table(
      ['Ref', 'Object'],
      r.refs.map((line) => line.split(' ')).map(([ref, oid]) => [code(ref), code(short(oid))]),
    ),
  )
  out.push(`### ${LARGEST_BLOBS} largest blobs\n`)
  out.push(
    table(
      ['Size (bytes)', 'Path', 'Commit', 'Blob'],
      r.largestBlobs.map((b) => [b.size, code(b.path), code(short(b.commit)), code(short(b.oid))]),
    ),
  )
  out.push(`### Tip files changed by replacements — ${r.tipChanged.length}\n`)
  out.push(
    'Files in the tip (HEAD) that contain a replacement marker, so the filter changed them.\n',
  )
  out.push(
    table(
      ['Path', 'Rules'],
      r.tipChanged.map((t) => [code(t.path), t.rules.map(code).join(', ')]),
    ),
  )
  out.push(`### History-only paths — ${r.historyOnly.length}\n`)
  out.push('Paths some commit holds that the tip (HEAD) does not.\n')
  out.push('<details><summary>Show paths</summary>\n')
  out.push(
    r.historyOnly.length === 0 ? '_None._' : r.historyOnly.map((p) => `- ${code(p)}`).join('\n'),
  )
  out.push('\n</details>\n')
  return out.join('\n') + '\n'
}

// ---------------------------------------------------------------------------------------------
// CLI

const USAGE = `usage:
  public-history.mjs prepare --source <path-or-url> --scratch <dir> [--spec <dir>]
                             [--keep-ref <pattern> ...] [--filter-repo <bin>]
  public-history.mjs audit --repo <scratch> [--spec <dir>] --report <file>`

const COMMAND_OPTIONS = {
  prepare: {
    source: { type: 'string' },
    scratch: { type: 'string' },
    spec: { type: 'string' },
    'keep-ref': { type: 'string', multiple: true },
    'filter-repo': { type: 'string' },
  },
  audit: {
    repo: { type: 'string' },
    spec: { type: 'string' },
    report: { type: 'string' },
  },
}

export async function main(argv, { stdout = process.stdout, stderr = process.stderr } = {}) {
  try {
    const [command, ...rest] = argv
    const options = Object.hasOwn(COMMAND_OPTIONS, command ?? '') ? COMMAND_OPTIONS[command] : null
    if (!options) throw new UsageError(`unknown or missing subcommand: ${command ?? '(none)'}`)
    const { values } = parseArgs({ args: rest, options, strict: true, allowPositionals: false })
    if (command === 'prepare') {
      const result = prepare({
        source: values.source,
        scratch: values.scratch,
        spec: values.spec,
        keepRefs: values['keep-ref'] ?? [],
        filterRepo: values['filter-repo'],
        log: (line) => stderr.write(`${line}\n`),
      })
      stdout.write(`${JSON.stringify({ scratch: result.scratch, keptRefs: result.keptRefs })}\n`)
      return 0
    }
    const result = await audit({ repo: values.repo, spec: values.spec, report: values.report })
    stdout.write(
      `${JSON.stringify({
        report: path.resolve(values.report),
        hardFindings: result.hardFindings.length,
        secretHits: result.secretHits.length,
      })}\n`,
    )
    return result.hardFindings.length === 0 ? 0 : 1
  } catch (error) {
    const usage = error instanceof UsageError || error?.code?.startsWith?.('ERR_PARSE_ARGS')
    stderr.write(`public-history: ${error.message}\n${usage ? `${USAGE}\n` : ''}`)
    return 2
  }
}

if (isMainModule(import.meta.url)) {
  process.exitCode = await main(process.argv.slice(2))
}
