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

function fileAxisExclusion(detector, evidence) {
  if (detector !== 'filesize') return null
  const m = /^(\d+)\s+lines\s+\(limit\s+(\d+)\)$/.exec(evidence)
  if (!m) return null
  const lines = Number(m[1])
  const limit = Number(m[2])
  if (lines > limit * DECOMPOSITION_MULTIPLE) {
    return `file-axis decomposition candidate: ${lines} lines exceeds ${DECOMPOSITION_MULTIPLE}x limit ${limit}`
  }
  return null
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
  let vulnId = null
  for (const raw of String(text).split('\n')) {
    const line = raw.trim()
    if (!line) continue
    const cmd = /^\$\s/.test(raw) ? detectorFor(raw) : null
    if (cmd) {
      detector = cmd
      ;({ command, module } = moduleForCommand(raw))
      jscpdPrimary = null
      vulnId = null
      continue
    }
    if (!detector) continue
    const push = (candidatePath, evidence) => {
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
      const excluded = fileAxisExclusion(detector, evidence)
      if (excluded) candidate.excluded = excluded
      out.push(candidate)
    }

    let m
    if (
      detector === 'deadcode' &&
      (m = /^(\S+\.go):\d+:\d+:\s+unreachable func:\s+(\S+)/.exec(line))
    ) {
      push(m[1], m[2])
    } else if (detector === 'cyclo' && (m = /^\d+\s+\S+\s+(\S+)\s+(\S+\.go):\d+/.exec(line))) {
      push(m[2], m[1])
    } else if (detector === 'dupl' && (m = /^(\S+\.go):\d+,\d+\s+(\S+\.go):\d+,\d+/.exec(line))) {
      push(m[1], m[2])
    } else if (detector === 'vuln') {
      // govulncheck's default text output is multi-line: the advisory ID sits on a
      // `Vulnerability #N: GO-YYYY-NNNN` header, and each reachable call site on an indented
      // `#N: <file>.go:line:col: <trace>` line under "Example traces found:". Carry the active
      // ID across lines so every reachable trace surfaces as a candidate (loss-free).
      if ((m = /^Vulnerability #\d+:\s+(GO-\d+-\d+|CVE-\d+-\d+)/.exec(line))) {
        vulnId = m[1]
      } else if (vulnId && (m = /^#\d+:\s+(\S+\.go):\d+/.exec(line))) {
        push(m[1], vulnId)
      }
    } else if (
      detector === 'filesize' &&
      // revive's `default` formatter emits ONE line per finding carrying all three values:
      // "<file>.go:<pos>: file length is N lines, which exceeds the limit of M". Keeping the
      // limit in the evidence is load-bearing — the complexity-hotspot playbook selects its
      // axis on "an eligible file exceeds 2x the detector's limit", and the threshold is
      // overridable, so a bare line count would leave that rule uncomputable downstream.
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
      valid.push(candidate)
    } else {
      dropped.push({ candidate, reason: verdict.reason })
    }
  }
  return { dropped, valid }
}

function runCli(argv) {
  const [cmd, json = '[]'] = argv
  if (cmd !== 'validate-candidates') return 0
  const { dropped, valid } = filterValidSurveyCandidates(JSON.parse(json), {
    repoRoot: process.cwd(),
  })
  for (const drop of dropped) {
    process.stderr.write(
      `bs-sweep-debt survey: dropped ${drop.candidate.path || drop.candidate.file || '<missing path>'}: ${drop.reason}\n`,
    )
  }
  process.stdout.write(JSON.stringify(valid))
  return 0
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
