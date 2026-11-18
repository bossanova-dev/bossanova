import {
  LINE_CONTINUES,
  SHELL_INFO_STRINGS,
  extractFencedBlocks,
  removeQuotes,
  tokenizeShellLine,
} from './check-skill-shell.mjs'

export const RUNNER_FORM_ROOT = 'lib/bossalib/contract/plugin/skillpayload/skills/boss-plan/'

/** @param {string} relPath */
export function inRunnerFormScope(relPath) {
  if (!relPath.startsWith(RUNNER_FORM_ROOT)) return false
  const name = relPath.slice(RUNNER_FORM_ROOT.length)
  return name === 'SKILL.md' || (name.startsWith('references/') && name.endsWith('.md'))
}

/** @param {string} body @returns {string[]} */
export function findRunnerFormViolations(body) {
  const reasons = new Set()
  const state = {
    quote: null,
    substDepth: 0,
    substQuote: null,
    runnerFormReasons: reasons,
    recordComments: true,
    lineOffset: 0,
    comments: new Map(),
  }
  let commandLines = 0
  let firstCommand = true
  for (const line of body.split('\n')) {
    const tokens = tokenizeShellLine(line, state)
    const comment = state.comments.get(state.lineOffset++)
    if (tokens.length === 0) continue
    commandLines += 1
    // A dangling continuation is still a second physical line, even with no argument after it.
    if (LINE_CONTINUES.test(line.slice(0, comment?.index ?? line.length))) reasons.add('multi-line')
    let words = []
    const inspect = () => {
      if (words.length === 0) return
      const args = words.map(removeQuotes)
      if (firstCommand) {
        if (args[0] !== 'bash' || !args[1]?.endsWith('/toolbox/plan/boss-plan-run.sh'))
          reasons.add('not-runner')
        firstCommand = false
      }
      if (['.', 'source'].includes(args[0])) reasons.add('sourcing')
      if (['bash', 'sh', 'zsh'].includes(args[0]) && args[1] === '-c') reasons.add('bash-c')
      words = []
    }
    for (const token of tokens) {
      if (token.operator) inspect()
      else words.push(token.raw)
    }
    inspect()
  }
  if (commandLines > 1) reasons.add('multi-line')
  if (firstCommand) reasons.add('not-runner')
  return [...reasons]
}

/** @param {string} markdown */
export function findLegacyToolboxMentions(markdown) {
  return markdown
    .split('\n')
    .flatMap((line, index) =>
      /BOSS_PLAN_TOOLBOX|boss-plan-env\.sh/.test(line)
        ? [{ line: index + 1, reason: 'legacy-toolbox' }]
        : [],
    )
}

/** @param {string} file @param {string} markdown */
export function runnerFormFindings(file, markdown) {
  if (!inRunnerFormScope(file)) return []
  const violations = findLegacyToolboxMentions(markdown)
  for (const block of extractFencedBlocks(markdown)) {
    if (!SHELL_INFO_STRINGS.has(block.info)) continue
    for (const reason of findRunnerFormViolations(block.body))
      violations.push({ line: block.startLine, reason })
  }
  return violations.map(({ line, reason }) => ({
    file,
    line,
    kind: 'runner-form',
    message: `${reason}; use one line: bash <skill-dir>/toolbox/plan/boss-plan-run.sh <step|node helper.mjs> …`,
  }))
}
