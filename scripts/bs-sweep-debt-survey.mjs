// scripts/bs-sweep-debt-survey.mjs
//
// Reference extraction for the bs-sweep-debt Phase 3 cheap-tier survey subagent (BOS-146).
// The survey runs the rotated-focus detectors (`make debt-*-<mod>`, `knip`) and must return a
// compact scored candidate list WITHOUT dropping any finding present in the raw detector
// output. This module is the deterministic, loss-free extraction that the brief specifies;
// scripts/bs-sweep-debt-skill.test.mjs feeds it a fixture and asserts the surfaced candidate
// set is a superset of every expected finding (the cheap-tier loss gate — D8).
//
// Node built-ins only — cron worktrees are dependency-free.

import fs from 'node:fs'
import path from 'node:path'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const AREA_RE = /^(services\/[^/\s]+|lib\/bossalib|plugins\/[^/\s]+|scripts|proto|docs)/

const DEFAULT_MODULE_ROOTS = {
  boss: 'services/boss',
  bossd: 'services/bossd',
  bossalib: 'lib/bossalib',
  bosso: 'services/bosso',
  docs: 'docs',
  proto: 'proto',
  scripts: 'scripts',
  web: 'services/web',
}

const DECOMPOSITION_MULTIPLE = 2

/** Top-level rotation area for a repo-relative path (or the raw path if unmatched). */
export function areaOf(file) {
  const m = AREA_RE.exec(file)
  return m ? m[1] : file
}

/** Stable identity of a candidate — used for the superset/loss check. */
export function candidateKey(c) {
  return `${c.category}::${c.path || c.file}::${c.evidence}`
}

// Which detector produced the block that follows a command line. The survey subagent runs
// each command and reads its output; the parser tracks the active detector the same way.
function detectorFor(line) {
  if (/\bmake debt-deadcode-/.test(line)) return 'deadcode'
  if (/\bmake debt-dupl-/.test(line)) return 'dupl'
  if (/\bmake debt-cyclo-/.test(line)) return 'cyclo'
  if (/\bmake debt-vuln-/.test(line)) return 'vuln'
  if (/\bmake debt-filesize-/.test(line)) return 'filesize'
  if (/\bknip\b/.test(line)) return 'knip'
  if (/\bjscpd\b/.test(line)) return 'jscpd'
  return null
}

function moduleForCommand(line) {
  let m
  if ((m = /\bmake debt-[a-z]+-([A-Za-z0-9_-]+)\b/.exec(line))) {
    const mod = m[1]
    return { command: `make ${/make (debt-[A-Za-z0-9_-]+)/.exec(line)[1]}`, module: mod }
  }
  if (/\bpnpm -C services\/web knip\b/.test(line))
    return { command: 'pnpm -C services/web knip', module: 'web' }
  if (/\bnpx jscpd services\/web\/src\b/.test(line))
    return { command: 'npx jscpd services/web/src', module: 'web' }
  return { command: line.replace(/^\$\s*/, '').trim(), module: '' }
}

const CATEGORY_OF = {
  deadcode: 'dead-code',
  dupl: 'duplication',
  cyclo: 'complexity-hotspot',
  vuln: 'security',
  filesize: 'complexity-hotspot',
  knip: 'dead-code',
  jscpd: 'duplication',
}

function moduleRoot(moduleName, moduleRoots = DEFAULT_MODULE_ROOTS) {
  if (moduleRoots[moduleName]) return moduleRoots[moduleName]
  if (moduleName.startsWith('bossd-plugin-')) return `plugins/${moduleName}`
  return `plugins/bossd-plugin-${moduleName}`
}

function isRepoRelativePath(candidatePath) {
  return (
    typeof candidatePath === 'string' &&
    candidatePath.length > 0 &&
    !path.isAbsolute(candidatePath) &&
    !candidatePath.split('/').includes('..')
  )
}

function pathInsideModule(candidatePath, moduleName, moduleRoots = DEFAULT_MODULE_ROOTS) {
  const root = moduleRoot(moduleName, moduleRoots)
  return candidatePath === root || candidatePath.startsWith(`${root}/`)
}

export function validateSurveyCandidate(candidate, options = {}) {
  const candidatePath = candidate.path
  const moduleName = candidate.module
  const repoRoot = options.repoRoot || process.cwd()
  const moduleRoots = options.moduleRoots || DEFAULT_MODULE_ROOTS

  if (!moduleName) return { ok: false, reason: 'missing module' }
  if (!candidatePath) return { ok: false, reason: 'missing path' }
  if (!isRepoRelativePath(candidatePath)) {
    return { ok: false, reason: 'path must be repo-root-relative' }
  }
  if (!fs.existsSync(path.join(repoRoot, candidatePath))) {
    return { ok: false, reason: 'path does not exist at repo root' }
  }
  if (!pathInsideModule(candidatePath, moduleName, moduleRoots)) {
    return { ok: false, reason: `path is outside declared module ${moduleName}` }
  }
  return { ok: true, reason: null }
}

const FILE_SIZE = /^(\d+)\s+lines\s+\(limit\s+(\d+)\)$/
const PLATFORM_SUFFIX =
  /_(?:aix|android|darwin|dragonfly|freebsd|illumos|ios|js|linux|netbsd|openbsd|plan9|solaris|wasip1|windows|386|amd64|arm|arm64|loong64|mips|mipsle|mips64|mips64le|ppc64|ppc64le|riscv64|s390x|wasm)(?:_test)?\.go$/

function loadAllowlist(repoRoot, errWrite) {
  const file = path.join(repoRoot, 'scripts/debt/portability-known-intentional.json')
  const entries = fs.existsSync(file) ? JSON.parse(fs.readFileSync(file, 'utf8')) : []
  warnStaleAllowlist(entries, repoRoot, errWrite)
  return entries
}

function warnStaleAllowlist(entries, repoRoot, errWrite) {
  for (const entry of entries) {
    const file = path.join(repoRoot, entry.path)
    if (!fs.existsSync(file) || !fs.readFileSync(file, 'utf8').includes(entry.contains)) {
      errWrite(`bs-sweep-debt survey: stale intentional entry ${entry.path}: ${entry.contains}\n`)
    }
  }
}

/** Stamp hard exclusions without dropping detector evidence. */
export function classifyCandidate(candidate, { repoRoot = process.cwd(), allowlist } = {}) {
  const c = { ...candidate }
  const command = String(c.confirmationCommand || '')
  const goDeadcode =
    c.category === 'dead-code' &&
    !/\bknip\b/.test(command) &&
    (String(c.path).endsWith('.go') || /debt-deadcode-/.test(command))
  const symbol = String(c.evidence || '')
    .split('.')
    .at(-1)
  if (goDeadcode && /^\p{Lu}/u.test(symbol)) {
    c.excluded = `exported-symbol: ${c.evidence}`
  } else if (c.category === 'security' && c.vulnScope === 'stdlib') {
    c.excluded = `toolchain-only: ${c.evidence}`
  } else if (c.category === 'portability') {
    const entry = (allowlist || loadAllowlist(repoRoot, () => {})).find(
      (e) => e.path === c.path && String(c.findingLine || '').includes(e.contains),
    )
    if (entry) c.excluded = `known-intentional: ${entry.reason}`
  } else if (c.category === 'complexity-hotspot' && FILE_SIZE.test(c.evidence)) {
    const [, lines, limit] = FILE_SIZE.exec(c.evidence).map(Number)
    if (lines > limit * DECOMPOSITION_MULTIPLE) {
      c.excluded = `decomposition: ${lines} lines exceeds ${DECOMPOSITION_MULTIPLE}x limit ${limit}`
    } else {
      const file = isRepoRelativePath(c.path) ? path.join(repoRoot, c.path) : null
      if (
        PLATFORM_SUFFIX.test(c.path) ||
        (file && fs.existsSync(file) && /^\s*\/\/go:build\s/m.test(fs.readFileSync(file, 'utf8')))
      ) {
        c.excluded = `build-constrained: ${c.path}`
      }
    }
  }
  return c
}

export function complexityAxis(candidates) {
  const eligible = candidates.filter((c) => c.category === 'complexity-hotspot' && !c.excluded)
  if (
    eligible.some((c) => {
      const match = FILE_SIZE.exec(c.evidence)
      return (
        match &&
        Number(match[1]) > Number(match[2]) &&
        Number(match[1]) <= Number(match[2]) * DECOMPOSITION_MULTIPLE
      )
    })
  )
    return { axis: 'file', reason: 'eligible file-axis finding' }
  if (eligible.some((c) => /debt-cyclo-/.test(c.confirmationCommand || '')))
    return { axis: 'function', reason: 'only function-axis findings remain' }
  return { axis: 'none', reason: 'no eligible complexity finding' }
}

export function noChangeEvidence(read, validationDrops = '') {
  const payload = read.status === 'ok' ? read.payload || {} : {}
  const categories = [...new Set(payload.surveyedCategories || [])]
  const areas = [...new Set(payload.surveyedAreas || [])]
  const rejected = (payload.candidates || [])
    .filter((c) => c.rejected || c.excluded)
    .sort(
      (a, b) =>
        Number(Boolean(b.rejected)) - Number(Boolean(a.rejected)) ||
        (b.score || 0) - (a.score || 0),
    )
    .map((c) => `- ${c.path || c.file} (${c.category}): ${c.rejected || c.excluded}`)
  for (const line of validationDrops.split('\n')) {
    const m = /dropped (.+?): (.+)$/.exec(line)
    if (m) rejected.push(`- ${m[1]} (validation): ${m[2]}`)
  }
  const gaps = []
  if (categories.length < 2) gaps.push('surveyed categories floor (2) not proven')
  if (areas.length < 3) gaps.push('surveyed areas floor (3) not proven')
  if (rejected.length < 3) gaps.push('fewer than 3 reasoned rejections')
  return {
    body: `Surveyed categories: ${categories.join(', ')}\nSurveyed areas: ${areas.join(', ')}\nTop rejected candidates:\n${rejected.join('\n')}\n`,
    gaps,
  }
}

/**
 * Parse combined detector output into normalized candidates. Each `$ make debt-*` / `knip`
 * command line switches the active detector; the following lines are parsed in that
 * detector's format. Every recognized finding becomes a candidate — none is dropped.
 * @param {string} text raw combined detector output
 * @returns {Array<{category: string, area: string, module: string, path: string, file: string, evidence: string, confirmationCommand: string, findingLine: string, excluded?: string}>}
 */
export function parseDetectorFindings(text) {
  const out = []
  let detector = null
  let command = ''
  let module = ''
  let jscpdPrimary = null
  let duplPrimary = null
  let vulnId = null
  let vulnScope
  let fixedIn
  for (const raw of String(text).split('\n')) {
    const line = raw.trim()
    if (!line) continue
    const cmd = /^\$\s/.test(raw) ? detectorFor(raw) : null
    if (cmd) {
      detector = cmd
      ;({ command, module } = moduleForCommand(raw))
      jscpdPrimary = null
      duplPrimary = null
      vulnId = null
      vulnScope = undefined
      fixedIn = undefined
      continue
    }
    if (!detector) continue
    const normalizeDetectorPath = (file) => {
      if (path.isAbsolute(file)) {
        const relative = path.relative(process.cwd(), file)
        return isRepoRelativePath(relative) ? relative : file
      }
      const relative = file.replace(/^\.\//, '')
      if (!isRepoRelativePath(relative)) return relative
      if (/^(?:services|lib|plugins|scripts|proto|docs)\//.test(relative)) return relative
      const root = moduleRoot(module)
      const runsAtRepoRoot =
        detector === 'jscpd' || (detector === 'deadcode' && root.startsWith('lib/'))
      return runsAtRepoRoot ? relative : root + '/' + relative
    }
    const push = (candidatePath, evidence) => {
      candidatePath = normalizeDetectorPath(candidatePath)
      if (detector === 'dupl' || detector === 'jscpd') evidence = normalizeDetectorPath(evidence)
      const candidate = {
        category: CATEGORY_OF[detector],
        area: areaOf(candidatePath),
        module,
        path: candidatePath,
        file: candidatePath,
        evidence,
        confirmationCommand: command,
        findingLine: line,
      }
      if (detector === 'vuln') {
        candidate.vulnScope = vulnScope
        candidate.fixedIn = fixedIn
      }
      out.push(classifyCandidate(candidate))
    }

    let m
    if (
      detector === 'deadcode' &&
      (m = /^(\S+\.go):\d+:\d+:\s+unreachable func:\s+(\S+)/.exec(line))
    ) {
      push(m[1], m[2])
    } else if (detector === 'cyclo' && (m = /^\d+\s+\S+\s+(\S+)\s+(\S+\.go):\d+/.exec(line))) {
      push(m[2], m[1])
    } else if (detector === 'dupl') {
      if (/^found \d+ clones:$/.test(line)) {
        duplPrimary = null
      } else if ((m = /^(\S+\.go):\d+,\d+\s+(\S+\.go):\d+,\d+/.exec(line))) {
        push(m[1], m[2])
        duplPrimary = null
      } else if ((m = /^(\S+\.go):\d+,\d+$/.exec(line))) {
        // dupl's default reporter lists all clones beneath a group header. Keep the
        // first as the anchor and surface every partner, including groups larger than two.
        if (duplPrimary) push(duplPrimary, line)
        else duplPrimary = m[1]
      } else {
        duplPrimary = null
      }
    } else if (detector === 'vuln') {
      // govulncheck's default text output is multi-line: the advisory ID sits on a
      // `Vulnerability #N: GO-YYYY-NNNN` header, and each reachable call site on an indented
      // `#N: <file>.go:line:col: <trace>` line under "Example traces found:". Carry the active
      // ID across lines so every reachable trace surfaces as a candidate (loss-free).
      if ((m = /^Vulnerability #\d+:\s+(GO-\d+-\d+|CVE-\d+-\d+)/.exec(line))) {
        vulnId = m[1]
        vulnScope = undefined
        fixedIn = undefined
      } else if (/^Standard library\b/.test(line)) {
        vulnScope = 'stdlib'
      } else if (/^Module:/.test(line)) {
        vulnScope = 'module'
      } else if ((m = /^Fixed in:\s*(.+)/.exec(line))) {
        fixedIn = m[1]
      } else if (vulnId && (m = /^#\d+:\s+(\S+\.go):\d+/.exec(line))) {
        push(m[1], fixedIn ? `${vulnId}; Fixed in: ${fixedIn}` : vulnId)
      }
    } else if (
      detector === 'filesize' &&
      // revive's `default` formatter emits ONE line per finding carrying all three values:
      // "<file>.go:<pos>: file length is N lines, which exceeds the limit of M".
      // Both count and overridable limit are needed for the classification verdict.
      (m =
        /^(\S+\.go):\d+(?::\d+)?:\s+file length is\s+(\d+)\s+lines,\s+which exceeds the limit of\s+(\d+)/.exec(
          line,
        ))
    ) {
      push(m[1], `${m[2]} lines (limit ${m[3]})`)
    } else if (detector === 'knip') {
      // knip's default (symbols) reporter groups findings by type. Surface all reachable types,
      // not just unused exports: unused exports (`<symbol>  <file>:line:col`), unused files (a
      // bare source path), and unused dependencies (`<name>  <manifest>`). Section titles
      // ("Unused exports (N)", …) and hint lines match none of these shapes.
      if ((m = /^(\S+)(?:\s+\S+)*\s+(\S+\.(?:ts|tsx|js|jsx|mts|cts|mjs|cjs)):\d+:\d+/.exec(line))) {
        push(m[2], m[1])
      } else if ((m = /^(\S+)\s+(\S*package\.json)\b/.exec(line))) {
        push(m[2], m[1])
      } else if ((m = /^(\S+\.(?:ts|tsx|js|jsx|mts|cts|mjs|cjs))$/.exec(line))) {
        push(m[1], 'unused file')
      }
    } else if (detector === 'jscpd' && (m = /^-\s+(\S+\.(?:js|jsx|ts|tsx))\s+\[/.exec(line))) {
      if (jscpdPrimary) {
        push(jscpdPrimary, m[1])
        jscpdPrimary = null
      } else {
        jscpdPrimary = m[1]
      }
    }
  }
  return out
}

export function filterValidSurveyCandidates(candidates, options = {}) {
  const dropped = []
  const valid = []
  for (const candidate of candidates) {
    const verdict = validateSurveyCandidate(candidate, options)
    if (verdict.ok) {
      valid.push(classifyCandidate(candidate, options))
    } else {
      dropped.push({ candidate, reason: verdict.reason })
    }
  }
  return { dropped, valid }
}

// The command surface. This entrypoint used to return 0 for ANY command it did not
// recognise, having printed nothing at all — so `--help` reported success and did
// nothing, and a typo'd command read as an empty-but-successful survey. Both now
// print this block; only `--help` exits 0.
export const DEBT_SURVEY_USAGE = `usage: node scripts/bs-sweep-debt-survey.mjs <command> [args]

commands:
  extract <detector-output-file>
      Print classified detector candidates as JSON; retain exclusions.
  axis <json-array>
      Print the eligible complexity axis verdict.
  no-change-evidence <survey-read-json> [validation-drop-lines]
      Print rejection evidence before cleanup; exit 3 when breadth/reasons are incomplete.
  validate-candidates <json-array>
      Filter a raw detector candidate list to the module-attributed, repo-relative
      candidates the survey is allowed to report. Prints the surviving candidates as
      JSON on stdout and one "dropped <path>: <reason>" line per rejection on stderr.
      The json-array argument defaults to [].

  --help, -h, help
      Print this message and exit 0.
`

/**
 * Dispatch one survey command. Returns the process exit code; never calls
 * process.exit directly so it is unit-testable.
 * @param {string[]} argv
 * @param {{write?: (s: string) => void, errWrite?: (s: string) => void}} [io]
 * @returns {number}
 */
export function runCli(
  argv,
  { write = (s) => process.stdout.write(s), errWrite = (s) => process.stderr.write(s) } = {},
) {
  const [cmd, arg = '[]', drops = ''] = argv
  if (cmd === '--help' || cmd === '-h' || cmd === 'help') {
    write(DEBT_SURVEY_USAGE)
    return 0
  }
  if (!['extract', 'axis', 'no-change-evidence', 'validate-candidates'].includes(cmd)) {
    errWrite(`bs-sweep-debt survey: unknown command: ${cmd ?? '(none)'}\n`)
    errWrite(DEBT_SURVEY_USAGE)
    return 2
  }
  try {
    const repoRoot = process.cwd()
    const allowlist = loadAllowlist(repoRoot, errWrite)
    if (cmd === 'extract') {
      if (!argv[1]) throw new Error('extract requires a detector-output file')
      write(
        JSON.stringify(
          parseDetectorFindings(fs.readFileSync(arg, 'utf8')).map((c) =>
            classifyCandidate(c, { repoRoot, allowlist }),
          ),
        ),
      )
    } else if (cmd === 'axis') {
      write(
        JSON.stringify(
          complexityAxis(JSON.parse(arg).map((c) => classifyCandidate(c, { repoRoot, allowlist }))),
        ),
      )
    } else if (cmd === 'no-change-evidence') {
      const { body, gaps } = noChangeEvidence(JSON.parse(arg), drops)
      write(body)
      for (const gap of gaps) errWrite(`incomplete: ${gap}\n`)
      return gaps.length ? 3 : 0
    } else if (cmd === 'validate-candidates') {
      const { dropped, valid } = filterValidSurveyCandidates(JSON.parse(arg), {
        repoRoot,
        allowlist,
      })
      for (const drop of dropped)
        errWrite(
          `bs-sweep-debt survey: dropped ${drop.candidate.path || drop.candidate.file || '<missing path>'}: ${drop.reason}\n`,
        )
      write(JSON.stringify(valid))
    }
    return 0
  } catch (error) {
    errWrite(`bs-sweep-debt survey: ${error.message}\n`)
    return 2
  }
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
