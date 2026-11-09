// A chat's terminal pane, read and answered from outside the session: the orchestrator's `pane`
// verb. It finds the chat's tmux session on this machine or over SSH, prints the pane's tail, and
// with `--approve` answers a permission or confirmation prompt (Claude Code's "Do you want to
// proceed?", codex's "Would you like to run the following command?") with its one-time "Yes".

import { execFileSync, spawnSync } from 'node:child_process'

// bossd names a chat's tmux session `<prefix>-<first 8 characters of the chat id>`.
const CHAT_ID = /^[A-Za-z0-9][A-Za-z0-9-]*$/
const TMUX_TARGET = /^[A-Za-z0-9._-]+$/

// The rows a prompt draws below its options: blank lines, a box's bottom edge and the key hints
// ("Esc to cancel · Tab to amend", "Press enter to confirm or esc to cancel").
const MAX_FOOTER_ROWS = 4
// How far above its first option a prompt's question may sit (codex prints the command between).
const MAX_QUESTION_GAP = 14
// Box chrome around a row: Claude Code draws some prompts inside a bordered card.
const ROW_CHROME = /^[\s│┃]+|[\s│┃]+$/g
const BOX_EDGE = /^[╭╰─━┌└]+[─━]*[╮╯┐┘]?$/
// One numbered option, with the selection cursor when it sits on it.
const OPTION = /^(?:([❯›>])\s*)?(\d)\.\s+(\S.*)$/
// The question a permission or confirmation prompt asks. An agent's own question to the operator
// ("Which approach?") is not one, even when its options read Yes and No.
const PERMISSION_QUESTION = /^(?:do you want to|would you like to|allow|proceed)\b.*\?$/i
const PERMISSION_REJECT = /\bwhat to do differently\b|\((?:esc|n)\)$/i
const APPROVAL_FOOTER = /\btab to amend\b|\bpress enter to confirm\b|\bpress \d-\d or esc\b/i
// A single-key shortcut an option names, as codex prints "1. Yes, proceed (y)".
const SHORTCUT = /\(([a-z0-9])\)$/i

// CSI sequences (including private modes such as ESC[?25l), OSC sequences and ESC + one char.
const stripAnsi = (text) =>
  String(text).replace(/\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[^[\]])/g, '')

function rows(text) {
  return stripAnsi(text)
    .replace(/\r/g, '')
    .split('\n')
    .map((row) => row.replace(ROW_CHROME, ''))
}

/**
 * The permission or confirmation prompt live at the bottom of a captured pane, or null. A prompt
 * is a question in the agents' permission phrasing, then a numbered run of options from 1 with a
 * "Yes" and a "No", then at most a few rows of footer. The answer is the lowest-numbered "Yes",
 * which both agents render as the one-time approval. `key` is its shortcut when the option names
 * one (codex's `(y)`), else its number, which selects it in both agents' menus.
 */
export function findApprovalPrompt(text) {
  const lines = rows(text)
  let end = lines.length
  while (end > 0 && !lines[end - 1]) end--
  let i = end - 1
  const footer = []
  while (i >= 0 && !OPTION.test(lines[i])) {
    footer.push(lines[i])
    if (footer.length > MAX_FOOTER_ROWS) return null
    i--
  }
  const options = []
  while (i >= 0) {
    const match = OPTION.exec(lines[i])
    if (match) {
      const n = Number(match[2])
      if (options.length && n !== options[0].n - 1) return null
      options.unshift({ n, label: match[3].trim(), selected: Boolean(match[1]) })
      i--
      if (n === 1) break
      continue
    }
    // A long option can wrap onto a row of its own.
    if (!options.length || !lines[i] || BOX_EDGE.test(lines[i])) return null
    i--
  }
  if (!options.length || options[0].n !== 1) return null
  let question = ''
  for (let j = i; j >= 0 && j >= i - MAX_QUESTION_GAP; j--) {
    if (PERMISSION_QUESTION.test(lines[j])) {
      question = lines[j]
      break
    }
  }
  const yes = options.find((option) => /^Yes\b/.test(option.label))
  const no = options.find((option) => /^No\b/.test(option.label))
  if (!question || !yes || !no) return null
  // A permission menu rejects with "No, and tell <agent> what to do differently (esc)" or carries
  // the approval footer; an agent's own question to the operator has neither.
  const approvalShaped =
    PERMISSION_REJECT.test(no.label) || footer.some((row) => APPROVAL_FOOTER.test(row))
  if (!approvalShaped) return null
  const shortcut = SHORTCUT.exec(yes.label)
  return {
    question,
    options,
    option: yes,
    key: shortcut ? shortcut[1] : String(yes.n),
    selected: options.find((option) => option.selected)?.n ?? null,
  }
}

const sameSignature = (a, b) =>
  Boolean(a && b) &&
  a.question === b.question &&
  a.options.map((o) => o.label).join('\n') === b.options.map((o) => o.label).join('\n')

/**
 * Answers the prompt `read()` shows with its affirmative key, through `press(key)`. When the key
 * only moved the cursor onto "Yes" (the same prompt is still up, now on that option), it confirms
 * with Enter. Never presses anything when no prompt is visible.
 *
 * @param {{ read: () => string, press: (key: string) => void, settle?: () => void }} io
 */
export function approvePrompt({ read, press, settle = () => {} }) {
  const prompt = findApprovalPrompt(read())
  if (!prompt)
    return { approved: false, keys: [], reason: 'no permission or confirmation prompt is visible' }
  const keys = [prompt.key]
  press(prompt.key)
  settle()
  let after = findApprovalPrompt(read())
  if (sameSignature(prompt, after) && after.selected === prompt.option.n) {
    keys.push('Enter')
    press('Enter')
    settle()
    after = findApprovalPrompt(read())
  }
  const stuck = sameSignature(prompt, after)
  return {
    approved: !stuck,
    keys,
    question: prompt.question,
    option: `${prompt.option.n}. ${prompt.option.label}`,
    ...(stuck ? { reason: 'the prompt is still showing after the keys were sent' } : {}),
    ...(after && !stuck ? { nextPrompt: after.question } : {}),
  }
}

function hostShell(host, script) {
  /** @type {import('node:child_process').ExecFileSyncOptionsWithStringEncoding} */
  const opts = { input: script, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] }
  return host
    ? execFileSync('ssh', ['-o', 'BatchMode=yes', host, 'bash', '-s'], opts)
    : execFileSync('bash', ['-s'], opts)
}

/**
 * Runs shell scripts against one chat's tmux pane on `host` ('' for this machine). `shell(host,
 * script)` returns the script's stdout; the default runs bash locally or over SSH.
 */
export function chatPane({ host = '', chat, remotePath, shell = hostShell }) {
  if (!CHAT_ID.test(String(chat ?? ''))) throw new Error(`invalid chat id ${JSON.stringify(chat)}`)
  const run = (body) => shell(host, `export PATH=${remotePath}\n${body}`)
  const target = run(
    `tmux ls -F '#S' 2>/dev/null | grep -- "-${chat.slice(0, 8)}$" | head -1`,
  ).trim()
  if (!target) throw new Error(`no tmux pane for chat ${chat}`)
  if (!TMUX_TARGET.test(target)) throw new Error(`unexpected tmux session name ${target}`)
  return {
    target,
    cwd: () => run(`tmux display -p -t '${target}' '#{pane_current_path}'`).trim(),
    read: (lines = 60) => run(`tmux capture-pane -p -t '${target}' -S -${Number(lines) * 3}`),
    press: (key) => {
      if (key !== 'Enter' && !/^[a-z0-9]$/i.test(key)) throw new Error(`refusing key ${key}`)
      run(
        key === 'Enter'
          ? `tmux send-keys -t '${target}' Enter`
          : `tmux send-keys -t '${target}' -l -- '${key}'`,
      )
    },
  }
}

/** The last `lines` non-blank rows of a captured pane. */
export function paneTail(text, lines) {
  const kept = String(text)
    .split('\n')
    .filter((row) => row.trim())
  return kept.slice(-lines).join('\n')
}

const settleBriefly = () => {
  spawnSync('sleep', ['1.5'])
}

/** The `pane` verb: prints a chat's pane tail, or with `approve` answers its prompt. */
export function paneVerb({ host, chat, lines, remotePath, approve, out = process.stdout }) {
  const pane = chatPane({ host, chat, remotePath })
  if (!approve) {
    out.write(`== ${pane.target} ${pane.cwd()}\n${paneTail(pane.read(lines), lines)}\n`)
    return 0
  }
  const result = approvePrompt({
    read: () => pane.read(),
    press: pane.press,
    settle: settleBriefly,
  })
  out.write(`${JSON.stringify({ chat, target: pane.target, ...result })}\n`)
  return result.approved ? 0 : 3
}
