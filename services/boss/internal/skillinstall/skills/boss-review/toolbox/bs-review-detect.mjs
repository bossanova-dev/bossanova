// Pure helpers for the bs-review skill: match a changed-file list against the
// review-lens registry and pick the cross-agent "second voice". Node built-ins
// only (cron worktrees are dependency-free). The lens registry lives in the
// declarative skill-config surface (.boss-skills.json) via the sibling
// skill-config.mjs (BOS-192); glob matching reuses that module's globToRegExp
// (no new dependency). Vendored into the boss-review skill toolbox (BOS-196).

import { loadSkillConfig, globToRegExp } from './skill-config.mjs'

/** The glob(s) a lens entry matches on: a `globs` array, else the single `glob`. */
function entryGlobs(entry) {
  if (Array.isArray(entry.globs)) return entry.globs
  if (typeof entry.glob === 'string') return [entry.glob]
  return []
}

/**
 * Match changed files against the review-lens registry. Specialist lenses are
 * ADDITIVE ONLY: a file that matches no lens is still fully reviewed by the
 * always-on rounds, so an empty/absent registry degrades to "no specialist lens"
 * (an empty array), never to "no review" or an error.
 * @param {string[]} files changed file paths
 * @param {Array<{id:string,skill:string,fallbackRubric?:string,glob?:string,globs?:string[]}>} lenses
 *   the lens registry (e.g. loadSkillConfig().lensMap)
 * @returns {Array<{lens:string,skill:string,fallbackRubric:(string|undefined),files:string[]}>}
 */
export function matchLenses(files, lenses) {
  const fileList = Array.isArray(files) ? files : []
  const registry = Array.isArray(lenses) ? lenses : []
  const out = []
  for (const entry of registry) {
    const globs = entryGlobs(entry).filter((g) => typeof g === 'string' && g.length > 0)
    const matched = fileList.filter((f) => globs.some((g) => globToRegExp(g).test(f)))
    if (matched.length > 0) {
      out.push({
        lens: entry.id,
        skill: entry.skill,
        fallbackRubric: entry.fallbackRubric,
        files: matched,
      })
    }
  }
  return out
}

/**
 * Pick the opposite agent for the second-opinion round.
 * Anything that is not "codex" yields "codex" (claude is the common host).
 * @param {string} currentAgent
 * @returns {"codex"|"claude"}
 */
export function secondVoiceAgent(currentAgent) {
  return currentAgent === 'codex' ? 'claude' : 'codex'
}

// Thin CLI. Two modes (invoked from the skill's bundled toolbox/):
//   node <toolbox>/bs-review-detect.mjs --second-voice <agent>   -> prints the opposite agent
//   node <toolbox>/bs-review-detect.mjs --lenses                 -> reads a newline-separated
//     changed-file list on stdin and prints the matched specialist lenses as JSON
//     (MatchedLens[]). The lens registry is loaded from the repo-root .boss-skills.json
//     so a consuming repo's config drives the CLI. `--lenses` is also the default when no
//     recognized flag is given.
if (import.meta.url === `file://${process.argv[1]}`) {
  const argv = process.argv.slice(2)
  const svIdx = argv.indexOf('--second-voice')
  if (svIdx !== -1) {
    // --second-voice needs no lens registry: don't load (or fail on) .boss-skills.json
    // for a flag that never depended on the config file.
    process.stdout.write(secondVoiceAgent(argv[svIdx + 1] ?? '') + '\n')
  } else {
    const lenses = loadSkillConfig().lensMap
    if (process.stdin.isTTY) {
      // No piped file list on an interactive TTY: reading stdin would block forever
      // waiting for 'end'. Treat it as an empty change set so the caller gets a
      // deterministic empty match set instead of a hang.
      process.stdout.write(JSON.stringify(matchLenses([], lenses)) + '\n')
    } else {
      const chunks = []
      process.stdin.on('data', (c) => chunks.push(c))
      process.stdin.on('end', () => {
        const files = chunks
          .join('')
          .split('\n')
          .map((s) => s.trim())
          .filter(Boolean)
        process.stdout.write(JSON.stringify(matchLenses(files, lenses)) + '\n')
      })
    }
  }
}
