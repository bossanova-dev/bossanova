#!/usr/bin/env node

// check-await-mechanism — a skill that ORCHESTRATES an awaited dispatch must name the concrete
// mechanism it awaits with, and (where it may) the explicit long Bash timeout that keeps the await
// inside the turn.
//
// THE DEFECT (BOS-1278). "Await it — never `run_in_background`" is a mandate with no mechanism
// attached. An orchestrator following it reaches for the only awaiting thing it has — a poll
// invoked through the harness's Bash tool — and a Bash call issued with no explicit `timeout` is
// silently BACKGROUNDED at 120s. Nothing errors. The mandated in-turn await has become a background
// task, the orchestrator walks on, and the run reports a clean pass over a dispatch it never
// awaited. Naming the helper closes the first half (there is a concrete thing to await with);
// naming the timeout closes the second (the await survives the tool boundary).
//
// THE CLASS IS DISCOVERED, NOT LISTED. Enumerating the sites by hand is what let the class grow to
// four more members than the source notes named. Instead the roster is derived: a skill is IN CLASS
// when its own markdown carries a dispatch signal (see DISPATCH_SIGNALS) — the literal vocabulary a
// skill uses when it dispatches and must hold the turn. A skill that is DISPATCHED rather than
// dispatching carries none of it and is out of class with no exclusion rule needed, which is the
// discriminator the BOS-1278 enumeration settled on: the obligation belongs where the dispatch is
// orchestrated.
//
// WHY THE TIMEOUT OBLIGATION IS SCOPED TO REPO-LOCAL SKILLS. `timeout:` is one agent's Bash-tool
// parameter. A PUBLISHED core installs into every user's global skill tree and runs on harnesses
// that have no such parameter, and docs/skills/dispatch-graph-audit.md already requires a core's
// await prose to name the agent-neutral shape "rather than only one agent's binding" — so a core
// naming a concrete `timeout:` value would trade one defect for a different one. The cores state
// their bound agent-neutrally instead (`BOSS_SKILL_EXTENSION_TIMEOUT_MS`, which
// `bs-dispatch-await.mjs` itself reads). Repo-local `.claude/skills/*` run on this harness only, so
// there the concrete value is both statable and required.
//
// The exemption SUBSTITUTES that obligation, it does not delete it. An `if (published) continue`
// would leave the reasoning above documented and unenforced — a core that names the helper and
// states no bound at all would pass, which is the shape this whole check exists to reject. So the
// published branch requires the agent-neutral bound by name.
//
// Exercised by scripts/check-await-mechanism.test.mjs and runnable via
// `node scripts/check-await-mechanism.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

/** The authoring roots. `.codex/skills` is GENERATED and deliberately absent. */
export const SKILL_ROOTS = ['.claude/skills', 'services/boss/internal/skillinstall/skills']

/** The root whose skills are published into every user's global tree. */
export const PUBLISHED_ROOT = 'services/boss/internal/skillinstall/skills'

/**
 * The vocabulary a skill uses when IT dispatches. A skill that is dispatched — a lens, a round, a
 * methodology extension, a brief read by a worker — carries none of these, so the class partitions
 * without an exclusion list to keep in sync.
 */
export const DISPATCH_SIGNALS = ['run_in_background', 'subagent_type', 'spawn_agent']

/** The shared awaited-dispatch helper. Both the repo-root and vendored spellings satisfy it. */
export const AWAIT_MECHANISM = 'bs-dispatch-await.mjs'

/**
 * An explicit harness Bash timeout, in the tool's own parameter spelling. The floor is the
 * backgrounding threshold: a `timeout:` at or below 120000 is not a LONG timeout, it is the default
 * restated, and it would satisfy the letter of the rule while leaving the defect in place.
 */
export const BASH_TIMEOUT_PATTERN = /\btimeout:\s*(\d{6,})\b/g
export const BASH_TIMEOUT_FLOOR_MS = 300_000

/**
 * The agent-neutral bound a PUBLISHED core must state in place of one agent's `timeout:` parameter.
 * `bs-dispatch-await.mjs` reads it (`legTimeoutMsFromEnv`), so it is a real bound on every harness
 * rather than a second spelling of the same Claude-only knob.
 */
export const NEUTRAL_TIMEOUT_BOUND = 'BOSS_SKILL_EXTENSION_TIMEOUT_MS'

function markdownFilesUnder(dir) {
  const out = []
  let entries
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true })
  } catch {
    return out
  }
  for (const entry of entries) {
    const full = path.join(dir, entry.name)
    if (entry.isDirectory()) out.push(...markdownFilesUnder(full))
    else if (entry.isFile() && entry.name.endsWith('.md')) out.push(full)
  }
  return out
}

/**
 * Every skill directory under the authoring roots, with the concatenated text of its own markdown.
 * @param {{root?: string}} [opts]
 * @returns {{slug: string, dir: string, root: string, published: boolean, files: string[], text: string}[]}
 */
export function discoverSkills({ root = REPO_ROOT } = {}) {
  const skills = []
  for (const skillRoot of SKILL_ROOTS) {
    let entries
    try {
      entries = fs.readdirSync(path.join(root, skillRoot), { withFileTypes: true })
    } catch {
      continue
    }
    for (const entry of entries) {
      if (!entry.isDirectory()) continue
      const dir = path.join(skillRoot, entry.name)
      const files = markdownFilesUnder(path.join(root, dir)).map((f) =>
        path.relative(root, f).split(path.sep).join('/'),
      )
      if (files.length === 0) continue
      skills.push({
        slug: entry.name,
        dir,
        root: skillRoot,
        published: skillRoot === PUBLISHED_ROOT,
        files,
        text: files.map((f) => fs.readFileSync(path.join(root, f), 'utf8')).join('\n'),
      })
    }
  }
  return skills.sort((a, b) => a.dir.localeCompare(b.dir))
}

/** True iff this skill's own markdown shows it orchestrating a dispatch. */
export function orchestratesDispatch(text) {
  return DISPATCH_SIGNALS.some((signal) => text.includes(signal))
}

/** The longest explicit Bash timeout the text states, or `null` when it states none. */
export function statedBashTimeoutMs(text) {
  let longest = null
  for (const match of String(text).matchAll(BASH_TIMEOUT_PATTERN)) {
    const ms = Number(match[1])
    if (Number.isFinite(ms) && (longest === null || ms > longest)) longest = ms
  }
  return longest
}

/**
 * Audit every in-class skill against its obligations.
 * @param {{root?: string}} [opts]
 * @returns {{ok: boolean, failures: string[], inClass: string[], checked: number}}
 */
export function checkAwaitMechanism({ root = REPO_ROOT } = {}) {
  const failures = []
  const inClass = []
  for (const skill of discoverSkills({ root })) {
    if (!orchestratesDispatch(skill.text)) continue
    inClass.push(skill.dir)
    if (!skill.text.includes(AWAIT_MECHANISM)) {
      failures.push(
        `${skill.dir}: mandates an awaited dispatch but names no await mechanism — cite ` +
          `${AWAIT_MECHANISM} in one of its markdown files`,
      )
    }
    if (skill.published) {
      // Substitution, not exemption: a core may not state one agent's `timeout:`, so it must state
      // the bound that holds on every agent instead.
      if (!skill.text.includes(NEUTRAL_TIMEOUT_BOUND)) {
        failures.push(
          `${skill.dir}: is published, so it may not state one agent's \`timeout:\` — but it ` +
            `states no bound at all; name \`${NEUTRAL_TIMEOUT_BOUND}\` as the await's bound`,
        )
      }
      continue
    }
    const stated = statedBashTimeoutMs(skill.text)
    if (stated === null) {
      failures.push(
        `${skill.dir}: names no explicit Bash timeout — an awaited poll invoked with none is ` +
          `silently backgrounded at 120s, so state \`timeout: ${BASH_TIMEOUT_FLOOR_MS}\` or longer`,
      )
    } else if (stated < BASH_TIMEOUT_FLOOR_MS) {
      failures.push(
        `${skill.dir}: states \`timeout: ${stated}\`, below the ${BASH_TIMEOUT_FLOOR_MS} floor — ` +
          'a short explicit timeout restates the default rather than surviving it',
      )
    }
  }
  if (inClass.length === 0) {
    failures.push('no orchestrating skill was discovered — the scan found nothing to check')
  }
  return { ok: failures.length === 0, failures, inClass, checked: inClass.length }
}

if (isMainModule(import.meta.url)) {
  const result = checkAwaitMechanism()
  if (!result.ok) {
    process.stderr.write(`${result.failures.join('\n')}\n`)
    process.exit(1)
  }
  process.stdout.write(`Verified the await mechanism in ${result.checked} orchestrating skills\n`)
}
