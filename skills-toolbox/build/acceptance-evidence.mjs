// acceptance-evidence.mjs — evidence certification may use besides running a criterion's own
// plan `check:` command. Both rules come from the repo's `.boss-skills.json`, never from here, so
// the published cores stay project-agnostic.
//
//   derivedCheck     a criterion with no command that names a test file the repo maps to a runner
//                    (`acceptance.derivedChecks: [{glob, command}]`) gets that runner as its check.
//   isFullGateCheck  a check made only of the repo's full gate commands (`acceptance.
//                    fullGateCommands`, default `commands.lint` and `commands.test`) is what CI
//                    runs, so a recorded green CI wait on the head proves it (../ci/ci-wait-record.mjs).

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

/** The repo's full gate commands: `acceptance.fullGateCommands`, else `commands.lint`/`.test`. */
function fullGateCommands(config) {
  const declared = config?.acceptance?.fullGateCommands
  const list = Array.isArray(declared) ? declared : [config?.commands?.lint, config?.commands?.test]
  return new Set(list.filter((c) => typeof c === 'string' && c.trim()).map(normalise))
}

/** Whether every `&&` step of `check` is one of the repo's full gate commands. */
export function isFullGateCheck(config, check) {
  if (typeof check !== 'string' || !check.trim()) return false
  const gate = fullGateCommands(config)
  return gate.size > 0 && check.split('&&').every((step) => gate.has(normalise(step)))
}
