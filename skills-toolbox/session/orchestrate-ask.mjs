#!/usr/bin/env node

// Asks the operator without pausing the orchestrator. The question dialog is what raises boss's
// `? question` status and its notification, and it pauses the chat that opens it. So the
// orchestrator opens a short-lived question chat in its own session, which asks through the
// dialog, sends the answers back to the orchestrator chat as one `ANSWER <id>: <json>` message,
// and stops. The orchestrator keeps running rounds and reads the message when it arrives.
//
//   node orchestrate-ask.mjs ask --id <question-id> --questions <file.json>
//        [--session <own-session>] [--reply-to <orchestrator-chat-id>] [--topic <title topic>]
//        [--agent claude]
// `--session` and `--reply-to` default to the BOSS_SESSION_ID and BOSS_AGENT_SESSION_ID bossd sets
// in every chat pane, so an orchestrator running in boss names neither.
//   node orchestrate-ask.mjs parse-answer [--message <text>]   (stdin when --message is absent)
//   node orchestrate-ask.mjs cancel --chat <question-chat-id>
// `ask` and `cancel` take [--host <ssh-host>] [--boss <boss>].

import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'

import { isMainModule } from '../main-module.mjs'

/**
 * @typedef {{ label: string, description?: string }} QuestionOption
 * @typedef {{ question: string, header: string, options: QuestionOption[], multiSelect?: boolean }} Question
 * @typedef {(args: string[]) => string} BossRunner
 * @typedef {Record<string, string | string[]>} Answers
 */

// What the question dialog shows: one to four questions, each with a short header and two to
// four labelled options.
const MAX_QUESTIONS = 4
const MAX_HEADER = 12
const MIN_OPTIONS = 2
const MAX_OPTIONS = 4
const ID = /^[A-Za-z0-9][A-Za-z0-9._-]*$/
const ANSWER = /\bANSWER ([A-Za-z0-9][A-Za-z0-9._-]*):/
const SUBMITTED = 'DELIVERY_STATE_SUBMITTED'
// The question dialog that raises boss's question status is Claude Code's.
export const DEFAULT_AGENT = 'claude'
export const CANCELLED_TITLE = 'question (cancelled)'
const CANCEL_INSTRUCTION =
  'The orchestrator cancelled this question because it no longer needs an answer. Close the question dialog if it is open, send no ANSWER, and stop.'

const isText = (/** @type {unknown} */ value) => typeof value === 'string' && value.trim() !== ''

/**
 * Checks `questions` against what the question dialog can show and returns it unchanged.
 * @param {unknown} questions
 * @returns {Question[]}
 */
export function validateQuestions(questions) {
  if (!Array.isArray(questions)) throw new Error('questions must be a JSON array')
  if (questions.length < 1 || questions.length > MAX_QUESTIONS)
    throw new Error(`questions: ask 1 to ${MAX_QUESTIONS}, not ${questions.length}`)
  const headers = new Set()
  questions.forEach((item, i) => {
    const where = `question ${i + 1}`
    if (!item || typeof item !== 'object') throw new Error(`${where} must be an object`)
    if (!isText(item.question)) throw new Error(`${where} needs question text`)
    if (!isText(item.header) || item.header.length > MAX_HEADER)
      throw new Error(`${where} needs a header of 1 to ${MAX_HEADER} characters`)
    if (headers.has(item.header)) throw new Error(`${where} repeats the header "${item.header}"`)
    headers.add(item.header)
    if (item.multiSelect !== undefined && typeof item.multiSelect !== 'boolean')
      throw new Error(`${where} multiSelect must be true or false`)
    const options = item.options
    if (!Array.isArray(options) || options.length < MIN_OPTIONS || options.length > MAX_OPTIONS)
      throw new Error(`${where} needs ${MIN_OPTIONS} to ${MAX_OPTIONS} options`)
    options.forEach((option, j) => {
      if (!option || !isText(option.label))
        throw new Error(`${where} option ${j + 1} needs a label`)
    })
  })
  return /** @type {Question[]} */ (questions)
}

function validateId(/** @type {unknown} */ id) {
  if (typeof id !== 'string' || !ID.test(id))
    throw new Error(
      `question id must be letters, digits, '.', '_' or '-', not ${JSON.stringify(id)}`,
    )
  return id
}

/**
 * The prompt a question chat runs: ask exactly `questions` in one dialog, send the answers back
 * to the `replyTo` chat as `ANSWER <id>: <json>`, then stop without acting on them.
 * @param {{ replyTo: string, id: string, questions: Question[] }} options
 */
export function relayPrompt({ replyTo, id, questions }) {
  return [
    `You are a question relay for the orchestrator chat ${replyTo}. Do these three steps, then stop.`,
    '',
    '1. Ask the operator exactly the questions below in one question dialog (the AskUserQuestion tool), unchanged: the same question text, headers, options and multiSelect. Ask nothing else.',
    '',
    '```json',
    JSON.stringify(questions, null, 2),
    '```',
    '',
    '2. When the operator answers, send the answers back with this one command:',
    '',
    `    boss chat send --json ${replyTo} 'ANSWER ${id}: <answers>'`,
    '',
    "   <answers> is one compact JSON object on one line. Its keys are the question headers. Each value is the chosen option's label, a list of labels for a multiSelect question, or the operator's own words when they wrote their own answer. Inside the single quotes, write each ' as '\\''. If the reply's delivery.delivery_state is neither DELIVERY_STATE_SUBMITTED nor DELIVERY_STATE_QUEUED, send it once more.",
    '',
    '3. Then stop. Never act on the answers yourself: run no other command, edit no file and message no other chat. The orchestrator carries the answers out.',
    '',
    'If a message says this question is cancelled, close the dialog and stop without replying.',
  ].join('\n')
}

/**
 * Reads an `ANSWER <id>: <json>` message back into its question id and answers. Text before the
 * message, such as a delivery prefix, is ignored.
 * @param {string} message
 * @returns {{ id: string, answers: Answers }}
 */
export function parseAnswer(message) {
  const match = ANSWER.exec(String(message))
  if (!match) throw new Error('not an answer: expected "ANSWER <question-id>: <json>"')
  const rest = message.slice(match.index + match[0].length).trim()
  /** @type {unknown} */
  let answers
  try {
    answers = JSON.parse(rest)
  } catch {
    const end = rest.lastIndexOf('}')
    try {
      answers = JSON.parse(rest.slice(0, end + 1))
    } catch {
      throw new Error(`answer ${match[1]} does not carry a JSON object`)
    }
  }
  if (!answers || typeof answers !== 'object' || Array.isArray(answers))
    throw new Error(`answer ${match[1]} must carry a JSON object keyed by question header`)
  const entries = Object.entries(answers)
  const valid = ([, value]) =>
    typeof value === 'string' ||
    (Array.isArray(value) && value.every((label) => typeof label === 'string'))
  if (!entries.length || !entries.every(valid))
    throw new Error(`answer ${match[1]} must map each header to a label or a list of labels`)
  return { id: match[1], answers: /** @type {Answers} */ (answers) }
}

const onHost = (/** @type {string | undefined} */ host) => (host ? ['--host', host] : [])

/**
 * Opens a question chat in `session`, sends it the relay prompt and confirms the prompt was
 * submitted. A relay that was not submitted is closed, so asking again cannot raise two dialogs.
 * @param {{ session: string, replyTo: string, id: string, questions: unknown, topic?: string,
 *   agent?: string, host?: string, runBoss: BossRunner }} options
 * @returns {{ chat: string, id: string }}
 */
export function ask({
  session,
  replyTo,
  id,
  questions,
  topic,
  agent = DEFAULT_AGENT,
  host,
  runBoss,
}) {
  validateId(id)
  const asked = validateQuestions(questions)
  if (!isText(session)) throw new Error('ask needs --session <the orchestrator’s own session>')
  if (!isText(replyTo)) throw new Error('ask needs --reply-to <the orchestrator chat id>')
  const boss = (/** @type {string[]} */ args) => runBoss([...onHost(host), 'chat', ...args])
  // The id leads the title, so `ls` and `resolve` can find this chat again from an ANSWER.
  const title = `${QUESTION_TITLE_PREFIX}${id}${topic ? ` · ${topic}` : ''}`
  const created = JSON.parse(
    boss(['new', session, '--role', 'operator', '--title', title, '--agent', agent, '--json']),
  )
  const chat = created?.chat?.agent_session_id
  if (!isText(chat)) throw new Error('boss chat new --json returned no chat id')
  const close = () => {
    try {
      boss(['delete', chat, '--json'])
    } catch {
      // Best effort: the error below already tells the caller the relay did not start.
    }
  }
  let state
  try {
    const sent = JSON.parse(
      boss(['send', '--json', chat, relayPrompt({ replyTo, id, questions: asked })]),
    )
    state = sent?.delivery?.delivery_state
  } catch (error) {
    close()
    throw error
  }
  if (state !== SUBMITTED) {
    close()
    throw new Error(`relay prompt to question chat ${chat} was ${state}; it is closed, ask again`)
  }
  return { chat, id }
}

export const QUESTION_TITLE_PREFIX = 'question: '

/**
 * The question chats open in `session`: the chats whose title `ask` gave them, with the question id
 * read back from that title.
 * @param {{ session: string, host?: string, runBoss: BossRunner }} options
 * @returns {{ chat: string, id: string, status: string }[]}
 */
export function listQuestions({ session, host, runBoss }) {
  if (!isText(session)) throw new Error('ls needs --session <the orchestrator’s own session>')
  const listed = JSON.parse(runBoss([...onHost(host), 'chats', '--json', session]))
  return (listed?.chats ?? [])
    .filter((c) => typeof c?.title === 'string' && c.title.startsWith(QUESTION_TITLE_PREFIX))
    .map((c) => ({
      chat: c.agent_session_id,
      id: c.title.slice(QUESTION_TITLE_PREFIX.length).split(' · ')[0].trim(),
      status: c.status ?? '',
    }))
}

/**
 * Reads an ANSWER message and closes the question chat that asked it, so an answered question never
 * leaves its chat open. Returns the answers even when no open chat matches (it may be closed already).
 * @param {{ message: string, session: string, host?: string, runBoss: BossRunner }} options
 */
export function resolve({ message, session, host, runBoss }) {
  const answer = parseAnswer(message)
  const closed = listQuestions({ session, host, runBoss })
    .filter((q) => q.id === answer.id)
    .map((q) => cancel({ chat: q.chat, host, runBoss }))
  return { ...answer, closed: closed.map((c) => c.chat) }
}

/** @param {unknown} error */
const missingDeleteVerb = (error) => {
  const failure = /** @type {{ message?: unknown, stderr?: unknown }} */ (error ?? {})
  return /unknown command "?delete"?/.test(`${failure.message ?? ''}\n${failure.stderr ?? ''}`)
}

/**
 * Closes a question chat whose question went moot: `boss chat delete` stops its pane and removes
 * it. A boss too old to delete chats gets the best the chat surface offers instead: a cancel
 * instruction and a title that marks the chat cancelled.
 * @param {{ chat: string, host?: string, runBoss: BossRunner }} options
 */
export function cancel({ chat, host, runBoss }) {
  if (!isText(chat)) throw new Error('cancel needs --chat <question chat id>')
  const boss = (/** @type {string[]} */ args) => runBoss([...onHost(host), 'chat', ...args])
  try {
    boss(['delete', chat, '--json'])
    return { chat, closed: true }
  } catch (error) {
    if (!missingDeleteVerb(error)) throw error
  }
  boss(['send', '--json', chat, CANCEL_INSTRUCTION])
  boss(['rename', chat, CANCELLED_TITLE])
  return { chat, closed: false, fallback: 'cancel instruction sent and chat renamed' }
}

/**
 * Runs `boss` with `args` and returns its stdout.
 * @param {string} bossBin
 * @returns {BossRunner}
 */
function bossRunner(bossBin) {
  return (args) => execFileSync(bossBin, args, { encoding: 'utf8', maxBuffer: 64 << 20 })
}

/**
 * @param {string[]} argv
 * @param {{ write: (text: string) => void, runBoss?: BossRunner, env: Record<string, string | undefined> }} io
 */
function main(argv, io) {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    strict: true,
    options: {
      session: { type: 'string' },
      'reply-to': { type: 'string' },
      id: { type: 'string' },
      questions: { type: 'string' },
      topic: { type: 'string' },
      agent: { type: 'string', default: DEFAULT_AGENT },
      chat: { type: 'string' },
      message: { type: 'string' },
      host: { type: 'string' },
      boss: { type: 'string', default: process.env.BOSS_BIN || 'boss' },
    },
  })
  const runBoss = io.runBoss ?? bossRunner(values.boss)
  const print = (/** @type {unknown} */ value) => io.write(`${JSON.stringify(value)}\n`)
  const [command] = positionals
  if (command === 'ask') {
    if (!values.questions) throw new Error('ask needs --questions <file.json>')
    print(
      ask({
        session: values.session ?? io.env.BOSS_SESSION_ID ?? '',
        replyTo: values['reply-to'] ?? io.env.BOSS_AGENT_SESSION_ID ?? '',
        id: values.id ?? '',
        questions: JSON.parse(readFileSync(values.questions, 'utf8')),
        topic: values.topic,
        agent: values.agent,
        host: values.host,
        runBoss,
      }),
    )
    return 0
  }
  if (command === 'parse-answer') {
    print(parseAnswer(values.message ?? readFileSync(0, 'utf8')))
    return 0
  }
  if (command === 'ls') {
    print(
      listQuestions({
        session: values.session ?? io.env.BOSS_SESSION_ID ?? '',
        host: values.host,
        runBoss,
      }),
    )
    return 0
  }
  if (command === 'resolve') {
    print(
      resolve({
        message: values.message ?? readFileSync(0, 'utf8'),
        session: values.session ?? io.env.BOSS_SESSION_ID ?? '',
        host: values.host,
        runBoss,
      }),
    )
    return 0
  }
  if (command === 'cancel') {
    print(cancel({ chat: values.chat ?? '', host: values.host, runBoss }))
    return 0
  }
  process.stderr.write('usage: orchestrate-ask.mjs ask|parse-answer|resolve|ls|cancel [options]\n')
  return 64
}

/**
 * Runs one verb and returns its exit code: 0 done, 1 failed, 64 usage.
 * @param {string[]} argv
 * @param {{ write?: (text: string) => void, runBoss?: BossRunner,
 *   env?: Record<string, string | undefined> }} [io]
 */
export function run(argv, io = {}) {
  try {
    return main(argv, { write: (text) => process.stdout.write(text), env: process.env, ...io })
  } catch (error) {
    process.stderr.write(
      `orchestrate-ask: ${String(/** @type {Error} */ (error).message).split('\n')[0]}\n`,
    )
    return 1
  }
}

if (isMainModule(import.meta.url)) process.exitCode = run(process.argv.slice(2))
