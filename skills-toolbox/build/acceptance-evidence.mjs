// acceptance-evidence.mjs — evidence certification may use besides running a criterion's own
// plan `check:` command. Both rules come from the repo's `.boss-skills.json`, never from here, so
// the published cores stay project-agnostic.
//
//   derivedCheck     a criterion with no command that names a test file the repo maps to a runner
//                    (`acceptance.derivedChecks: [{glob, command}]`) gets that runner as its check.
//   isFullGateCheck  a check made only of the repo's full gate commands (`acceptance.
//                    fullGateCommands`, default `commands.lint` and `commands.test`), each in a
//                    directory it counts in, is what CI runs, so a recorded green CI wait on the
//                    head proves it (../ci/ci-wait-record.mjs).

import { globToRegExp } from '../skill-config.mjs'

// A path-like token: no spaces, at least one dot. `file.test.mjs:140` yields `file.test.mjs`.
const PATH_TOKEN = /[\w@./-]*\w\.[\w.-]*\w/g
const SAFE_SHELL_WORD = /^[\w@%+=:,./-]+$/

const shellQuote = (value) =>
  SAFE_SHELL_WORD.test(value) ? value : `'${value.replace(/'/g, `'\\''`)}'`

/** The repo's `{glob, command}` rules, malformed entries dropped. */
function derivedCheckRules(config) {
  const rules = config?.acceptance?.derivedChecks
  if (!Array.isArray(rules)) return []
  return rules
    .filter(
      (rule) =>
        typeof rule?.glob === 'string' &&
        rule.glob.trim() &&
        typeof rule.command === 'string' &&
        /\{files?\}/.test(rule.command),
    )
    .map((rule) => ({ pattern: globToRegExp(rule.glob.trim()), command: rule.command }))
}

/**
 * Resolve each path-like token of `text` to exactly one tracked file: the path itself, or the one
 * file whose path ends with `/<token>`. An ambiguous or unknown name resolves to nothing.
 */
function namedFiles(text, files) {
  const tracked = new Set(files)
  const found = []
  for (const raw of String(text).match(PATH_TOKEN) ?? []) {
    const token = raw.replace(/^\.\//, '')
    const hits = tracked.has(token) ? [token] : files.filter((f) => f.endsWith(`/${token}`))
    if (hits.length === 1 && !found.includes(hits[0])) found.push(hits[0])
  }
  return found
}

/**
 * The check a criterion with no plan command can still run: the configured runner for every test
 * file it names, joined with `&&`. `{file}` runs once per file; `{files}` once for all of a rule's
 * files. Null when the repo maps nothing or the criterion names no mapped file.
 *
 * @param {object} config skill config
 * @param {string} text the criterion text
 * @param {string[]} files the repo's tracked paths
 * @returns {{ command: string, files: string[] } | null}
 */
export function derivedCheck(config, text, files) {
  const rules = derivedCheckRules(config)
  if (!rules.length) return null
  const byRule = new Map()
  for (const file of namedFiles(text, files)) {
    const rule = rules.find((r) => r.pattern.test(file))
    if (!rule) continue
    if (!byRule.has(rule)) byRule.set(rule, [])
    byRule.get(rule).push(file)
  }
  if (!byRule.size) return null
  const commands = []
  for (const [rule, ruleFiles] of byRule) {
    if (rule.command.includes('{files}'))
      commands.push(rule.command.replaceAll('{files}', ruleFiles.map(shellQuote).join(' ')))
    else
      commands.push(...ruleFiles.map((file) => rule.command.replaceAll('{file}', shellQuote(file))))
  }
  return { command: commands.join(' && '), files: [...byRule.values()].flat() }
}

const normalise = (command) => command.trim().replace(/\s+/g, ' ')

// A directory a gate step can follow: no absolute, home, option or shell-expanded path.
const PLAIN_DIR = /^(?![-~/])[\w@%+=:,./-]+$/

/** The repo-relative directory (`.` for the root) `dir` reaches from `cwd`; null above the root. */
function within(cwd, dir) {
  if (typeof dir !== 'string' || !PLAIN_DIR.test(dir)) return null
  const parts = cwd === '.' ? [] : cwd.split('/')
  for (const part of dir.split('/')) {
    if (part === '..') {
      if (!parts.length) return null
      parts.pop()
    } else if (part && part !== '.') parts.push(part)
  }
  return parts.length ? parts.join('/') : '.'
}

// `make`'s directory options: `-C <dir>`, `-C<dir>`, `--directory <dir>`, `--directory=<dir>`.
const MAKE_DIRECTORY = /^(?:-C(.*)|--directory(?:=(.*))?)$/

function makeDirectory(words, i) {
  const match = MAKE_DIRECTORY.exec(words[i])
  if (!match) return null
  const dir = match[1] || match[2]
  return dir ? { dir, next: i + 1 } : { dir: words[i + 1], next: i + 2 }
}

/**
 * The `{dir, command}` steps an `&&` chain runs, `dir` repo-relative (`.` for the root): a
 * `cd <dir>` step moves the steps after it, and `make -C <dir> <args>` is `make <args>` run in
 * `<dir>`. null when a step is empty or a directory cannot be followed.
 * @param {string} command
 * @returns {{dir: string, command: string}[] | null}
 */
export function gateSteps(command) {
  let cwd = '.'
  const steps = []
  for (const raw of String(command).split('&&')) {
    const words = normalise(raw).split(' ').filter(Boolean)
    if (words[0] === 'cd') {
      cwd = words.length === 2 ? within(cwd, words[1]) : null
      if (cwd === null) return null
      continue
    }
    if (!words.length) return null
    let dir = cwd
    const kept = words.slice(0, 1)
    for (let i = 1; i < words.length;) {
      const option = words[0] === 'make' ? makeDirectory(words, i) : null
      if (!option) {
        kept.push(words[i++])
        continue
      }
      dir = within(dir, option.dir)
      if (dir === null) return null
      i = option.next
    }
    steps.push({ dir, command: kept.join(' ') })
  }
  return steps
}

/** The declared gate entries: `acceptance.fullGateCommands`, else `commands.lint`/`.test`. */
function declaredGates(config) {
  const declared = config?.acceptance?.fullGateCommands
  return Array.isArray(declared) ? declared : [config?.commands?.lint, config?.commands?.test]
}

/**
 * The repo's full gates as `{command, dir}`, `dir` the RegExp of the directories a gate counts in
 * (null for any). A plain entry counts anywhere unless it moves directory itself (`make -C web
 * test` is `make test` in `web`); a `{dir, command}` entry counts in the directories its glob
 * matches. Entries that are not one command, such as a bare `cd <dir>`, are left out.
 */
function fullGates(config) {
  const gates = []
  for (const entry of declaredGates(config)) {
    const scoped = typeof entry?.dir === 'string' && typeof entry?.command === 'string'
    const steps = gateSteps(scoped ? entry.command : typeof entry === 'string' ? entry : '')
    if (steps?.length !== 1 || !steps[0].command) continue
    const [{ dir, command }] = steps
    if (scoped) {
      const glob =
        entry.dir
          .trim()
          .replace(/^(?:\.\/)+/, '')
          .replace(/\/+$/, '') || '.'
      if (dir !== '.') continue
      gates.push({ command, dir: globToRegExp(glob) })
    } else gates.push({ command, dir: dir === '.' ? null : globToRegExp(dir) })
  }
  return gates
}

/** The pre-directory rule: every `&&` step is literally one of the plain entries. */
function literalGateCheck(config, check) {
  const literal = new Set(
    declaredGates(config)
      .filter((c) => typeof c === 'string' && c.trim())
      .map(normalise),
  )
  return literal.size > 0 && check.split('&&').every((step) => literal.has(normalise(step)))
}

/** Whether every step of `check`, in the directory it runs in, is one of the repo's full gates. */
export function isFullGateCheck(config, check) {
  if (typeof check !== 'string' || !check.trim()) return false
  if (literalGateCheck(config, check)) return true
  const gates = fullGates(config)
  const steps = gateSteps(check)
  return (
    gates.length > 0 &&
    Boolean(steps?.length) &&
    steps.every((step) =>
      gates.some((gate) => gate.command === step.command && (!gate.dir || gate.dir.test(step.dir))),
    )
  )
}

/** Recover a leading inline gate assertion, using only commands the repo declares as full gates. */
export function inlineFullGateCheck(config, text) {
  const match = String(text).match(/^`([^`]+)`\s+pass(?:es)?(?=[.!]|$)/)
  return match && isFullGateCheck(config, match[1]) ? normalise(match[1]) : null
}
