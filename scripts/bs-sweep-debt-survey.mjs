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

const AREA_RE = /^(services\/[^/\s]+|lib\/bossalib|plugins\/[^/\s]+|scripts|proto|docs)/

/** Top-level rotation area for a repo-relative path (or the raw path if unmatched). */
export function areaOf(file) {
  const m = AREA_RE.exec(file)
  return m ? m[1] : file
}

/** Stable identity of a candidate — used for the superset/loss check. */
export function candidateKey(c) {
  return `${c.category}::${c.file}::${c.evidence}`
}

// Which detector produced the block that follows a command line. The survey subagent runs
// each command and reads its output; the parser tracks the active detector the same way.
function detectorFor(line) {
  if (/\bmake debt-deadcode-/.test(line)) return 'deadcode'
  if (/\bmake debt-dupl-/.test(line)) return 'dupl'
  if (/\bmake debt-cyclo-/.test(line)) return 'cyclo'
  if (/\bmake debt-vuln-/.test(line)) return 'vuln'
  if (/\bknip\b/.test(line)) return 'knip'
  if (/\bjscpd\b/.test(line)) return 'jscpd'
  return null
}

const CATEGORY_OF = {
  deadcode: 'dead-code',
  dupl: 'duplication',
  cyclo: 'complexity-hotspot',
  vuln: 'security',
  knip: 'dead-code',
  jscpd: 'duplication',
}

/**
 * Parse combined detector output into normalized candidates. Each `$ make debt-*` / `knip`
 * command line switches the active detector; the following lines are parsed in that
 * detector's format. Every recognized finding becomes a candidate — none is dropped.
 * @param {string} text raw combined detector output
 * @returns {Array<{category: string, area: string, file: string, evidence: string}>}
 */
export function parseDetectorFindings(text) {
  const out = []
  let detector = null
  let jscpdPrimary = null
  let vulnId = null
  for (const raw of String(text).split('\n')) {
    const line = raw.trim()
    if (!line) continue
    const cmd = /^\$\s/.test(raw) ? detectorFor(raw) : null
    if (cmd) {
      detector = cmd
      jscpdPrimary = null
      vulnId = null
      continue
    }
    if (!detector) continue
    const push = (file, evidence) =>
      out.push({ category: CATEGORY_OF[detector], area: areaOf(file), file, evidence })

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
