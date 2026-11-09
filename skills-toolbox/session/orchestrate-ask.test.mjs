#!/usr/bin/env node
import assert from 'node:assert/strict'
import { writeFileSync } from 'node:fs'
import path from 'node:path'
import test from 'node:test'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  CANCELLED_TITLE,
  ask,
  cancel,
  listQuestions,
  parseAnswer,
  resolve,
  relayPrompt,
  run,
  validateQuestions,
} from './orchestrate-ask.mjs'

const question = (header, over = {}) => ({
  question: `Merge the ${header} PR?`,
  header,
  multiSelect: false,
  options: [
    { label: 'A: merge (recommended)', description: 'Its park is policy only.' },
    { label: 'B: hold', description: 'Wait for the release.' },
  ],
  ...over,
})
const ORCHESTRATOR = '11111111-2222-3333-4444-555555555555'
const QUESTION_CHAT = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee'

// A daemon behind the `boss` argv the helper sends. `delivery` is the state `chat send` reports;
// `deleteError` is what `chat delete` throws, if anything.
function fakeBoss({ delivery = 'DELIVERY_STATE_SUBMITTED', deleteError = null } = {}) {
  const calls = []
  const runBoss = (/** @type {string[]} */ args) => {
    calls.push(args)
    const verbs = args.filter((arg, i) => arg !== '--host' && args[i - 1] !== '--host')
    const [group, verb, target] = verbs
    assert.equal(group, 'chat')
    if (verb === 'new')
      return JSON.stringify({
        chat: { agent_session_id: QUESTION_CHAT, session_id: target, role: 'operator' },
      })
    if (verb === 'send')
      return JSON.stringify({
        agent_session_id: verbs[3],
        delivery: { delivered: true, delivery_state: delivery },
      })
    if (verb === 'delete') {
      if (deleteError) throw deleteError
      return JSON.stringify({ agent_session_id: target, deleted: true })
    }
    if (verb === 'rename') return `Chat ${target} renamed.`
    throw new Error(`unexpected boss call ${args.join(' ')}`)
  }
  return { runBoss, calls }
}

test('validateQuestions accepts one to four questions with options', () => {
  const four = ['Alpha', 'Beta', 'Gamma', 'Delta'].map((h) => question(h))
  assert.deepEqual(validateQuestions(four), four)
  assert.deepEqual(validateQuestions([question('Alpha')]), [question('Alpha')])
})

test('validateQuestions rejects what the question dialog cannot show', () => {
  const bad = {
    'not a list': { questions: 'x' },
    'no questions': [],
    'five questions': ['A', 'B', 'C', 'D', 'E'].map((h) => question(h)),
    'one option': [question('Alpha', { options: [question('Alpha').options[0]] })],
    'five options': [
      question('Alpha', {
        options: ['1', '2', '3', '4', '5'].map((label) => ({ label, description: 'd' })),
      }),
    ],
    'an unlabelled option': [
      question('Alpha', { options: [{ description: 'd' }, { label: 'B' }] }),
    ],
    'a long header': [question('Much too long a header')],
    'no question text': [question('Alpha', { question: ' ' })],
    'two questions under one header': [question('Alpha'), question('Alpha')],
    'a non-boolean multiSelect': [question('Alpha', { multiSelect: 'yes' })],
  }
  for (const [name, questions] of Object.entries(bad))
    assert.throws(() => validateQuestions(questions), /question/i, name)
})

const promptFor = (over = {}) =>
  relayPrompt({ replyTo: ORCHESTRATOR, id: 'merge-4012', questions: [question('Alpha')], ...over })

test('the relay prompt replies to the orchestrator chat under the question id', () => {
  const prompt = promptFor()
  // The only boss command it may run is the reply to the orchestrator chat.
  const commands = prompt.match(/\bboss\b[^\n`]*/g) ?? []
  assert.ok(commands.length > 0)
  for (const command of commands)
    assert.ok(
      command.startsWith(`boss chat send --json ${ORCHESTRATOR} 'ANSWER merge-4012: `),
      command,
    )
  // The answer the reply carries reads back under the same id.
  const message = `ANSWER merge-4012: ${JSON.stringify({ Alpha: 'A: merge (recommended)' })}`
  assert.deepEqual(parseAnswer(message), {
    id: 'merge-4012',
    answers: { Alpha: 'A: merge (recommended)' },
  })
})

test('the relay prompt asks exactly the given questions', () => {
  const questions = [question('Alpha'), question('Beta', { multiSelect: true })]
  const prompt = promptFor({ questions })
  const block = prompt.match(/```json\n([\s\S]*?)\n```/)
  assert.ok(block, 'the questions travel as one JSON block')
  assert.deepEqual(JSON.parse(block[1]), questions)
})

test('the relay prompt forbids acting on the answers and ends the chat’s work', () => {
  const prompt = promptFor()
  assert.match(prompt, /never act on the answers/i)
  assert.match(prompt, /then stop/i)
})

test('parseAnswer reads an ANSWER message back into its id and answers', () => {
  const answers = { Alpha: 'A: merge (recommended)', Beta: ['B: hold', 'Other: ask Sam'] }
  const message = `\n  ANSWER q-7: ${JSON.stringify(answers)}\n`
  assert.deepEqual(parseAnswer(message), { id: 'q-7', answers })
  // Text around the message, such as a delivery prefix, is ignored.
  assert.deepEqual(parseAnswer(`from chat x:\nANSWER q-7: {"Alpha":"B"}`), {
    id: 'q-7',
    answers: { Alpha: 'B' },
  })
})

test('parseAnswer rejects a message that is not an answer', () => {
  for (const message of [
    'please merge it',
    'ANSWER : {}',
    'ANSWER q-7 {"Alpha":"B"}',
    'ANSWER q-7: not json',
    'ANSWER q-7: ["B"]',
    'ANSWER q-7: {"Alpha": 3}',
  ])
    assert.throws(() => parseAnswer(message), /answer/i, message)
})

test('ask opens a question chat in the orchestrator’s session and relays the questions', () => {
  const boss = fakeBoss()
  const questions = [question('Alpha')]
  const result = ask({
    session: 'sess-1',
    replyTo: ORCHESTRATOR,
    id: 'merge-4012',
    questions,
    runBoss: boss.runBoss,
  })
  assert.deepEqual(result, { chat: QUESTION_CHAT, id: 'merge-4012' })
  const [create, send] = boss.calls
  assert.deepEqual(create, [
    'chat',
    'new',
    'sess-1',
    '--role',
    'operator',
    '--title',
    'question: merge-4012',
    '--agent',
    'claude',
    '--json',
  ])
  assert.deepEqual(send.slice(0, 4), ['chat', 'send', '--json', QUESTION_CHAT])
  assert.equal(send[4], relayPrompt({ replyTo: ORCHESTRATOR, id: 'merge-4012', questions }))
  assert.equal(boss.calls.length, 2)
})

test('ask drives the daemon on another host', () => {
  const boss = fakeBoss()
  ask({
    session: 'sess-1',
    replyTo: ORCHESTRATOR,
    id: 'q1',
    questions: [question('Alpha')],
    host: 'build-host',
    topic: 'merges',
    runBoss: boss.runBoss,
  })
  for (const call of boss.calls) assert.deepEqual(call.slice(0, 2), ['--host', 'build-host'])
  assert.ok(boss.calls[0].includes('question: q1 · merges'))
})

test('ask closes the question chat when the relay prompt was not submitted', () => {
  const boss = fakeBoss({ delivery: 'DELIVERY_STATE_UNCONFIRMED' })
  assert.throws(
    () =>
      ask({
        session: 'sess-1',
        replyTo: ORCHESTRATOR,
        id: 'q1',
        questions: [question('Alpha')],
        runBoss: boss.runBoss,
      }),
    /DELIVERY_STATE_UNCONFIRMED/,
  )
  // A relay that might still raise the dialog later is closed, so a retry cannot ask twice.
  assert.deepEqual(boss.calls.at(-1), ['chat', 'delete', QUESTION_CHAT, '--json'])
})

test('ask refuses invalid questions before it opens a chat', () => {
  const boss = fakeBoss()
  assert.throws(() =>
    ask({ session: 's', replyTo: ORCHESTRATOR, id: 'q1', questions: [], runBoss: boss.runBoss }),
  )
  assert.throws(() =>
    ask({
      session: 's',
      replyTo: ORCHESTRATOR,
      id: 'two words',
      questions: [question('Alpha')],
      runBoss: boss.runBoss,
    }),
  )
  assert.deepEqual(boss.calls, [])
})

test('cancel closes the question chat', () => {
  const boss = fakeBoss()
  assert.deepEqual(cancel({ chat: QUESTION_CHAT, host: 'build-host', runBoss: boss.runBoss }), {
    chat: QUESTION_CHAT,
    closed: true,
  })
  assert.deepEqual(boss.calls, [
    ['--host', 'build-host', 'chat', 'delete', QUESTION_CHAT, '--json'],
  ])
})

test('cancel falls back to a cancel instruction on a boss without chat delete', () => {
  const old = Object.assign(new Error('Command failed'), {
    stderr: 'Error: unknown command "delete" for "boss chat"\n',
  })
  const boss = fakeBoss({ deleteError: old })
  assert.deepEqual(cancel({ chat: QUESTION_CHAT, runBoss: boss.runBoss }), {
    chat: QUESTION_CHAT,
    closed: false,
    fallback: 'cancel instruction sent and chat renamed',
  })
  const [, send, rename] = boss.calls
  assert.deepEqual(send.slice(0, 4), ['chat', 'send', '--json', QUESTION_CHAT])
  assert.deepEqual(rename, ['chat', 'rename', QUESTION_CHAT, CANCELLED_TITLE])
})

test('cancel reports any other delete failure', () => {
  const boss = fakeBoss({ deleteError: new Error('daemon unreachable') })
  assert.throws(() => cancel({ chat: QUESTION_CHAT, runBoss: boss.runBoss }), /unreachable/)
  assert.equal(boss.calls.length, 1)
})

test('the CLI asks from a questions file and prints the question chat', (t) => {
  const dir = tempDir('orchestrate-ask-', t)
  const file = path.join(dir, 'questions.json')
  writeFileSync(file, JSON.stringify([question('Alpha')]))
  const boss = fakeBoss()
  /** @type {string[]} */
  const out = []
  const argv = ['ask', '--session', 's', '--reply-to', ORCHESTRATOR, '--id', 'q1']
  const exit = run([...argv, '--questions', file], {
    runBoss: boss.runBoss,
    write: (text) => out.push(text),
  })
  assert.equal(exit, 0)
  assert.deepEqual(JSON.parse(out.join('')), { chat: QUESTION_CHAT, id: 'q1' })
  // An unknown flag is refused before any boss call.
  const refused = run([...argv, '--questions', file, '--bogus'], {
    runBoss: boss.runBoss,
    write: () => {},
  })
  assert.notEqual(refused, 0)
  assert.equal(boss.calls.length, 2)
})

test('the CLI replies to the chat it runs in by default', (t) => {
  const dir = tempDir('orchestrate-ask-', t)
  const file = path.join(dir, 'questions.json')
  writeFileSync(file, JSON.stringify([question('Alpha')]))
  const boss = fakeBoss()
  const exit = run(['ask', '--id', 'q1', '--questions', file], {
    runBoss: boss.runBoss,
    write: () => {},
    env: { BOSS_SESSION_ID: 'sess-own', BOSS_AGENT_SESSION_ID: ORCHESTRATOR },
  })
  assert.equal(exit, 0)
  const [create, send] = boss.calls
  assert.equal(create[2], 'sess-own')
  assert.ok(send[4].includes(`boss chat send --json ${ORCHESTRATOR} 'ANSWER q1: `))
  // Outside a boss chat there is nothing to default to, so ask refuses before any call.
  const outside = fakeBoss()
  const refused = run(['ask', '--id', 'q1', '--questions', file], {
    runBoss: outside.runBoss,
    write: () => {},
    env: {},
  })
  assert.notEqual(refused, 0)
  assert.deepEqual(outside.calls, [])
})

test('the CLI parses an answer from its argument', () => {
  /** @type {string[]} */
  const out = []
  const exit = run(['parse-answer', '--message', 'ANSWER q1: {"Alpha":"B"}'], {
    runBoss: () => assert.fail('parse-answer needs no daemon'),
    write: (text) => out.push(text),
  })
  assert.equal(exit, 0)
  assert.deepEqual(JSON.parse(out.join('')), { id: 'q1', answers: { Alpha: 'B' } })
})

// A boss whose `chats --json` lists the orchestrator chat, two question chats and a repair chat.
function sessionBoss() {
  const calls = []
  const runBoss = (/** @type {string[]} */ args) => {
    calls.push(args)
    if (args[0] === 'chats')
      return JSON.stringify({
        chats: [
          { agent_session_id: ORCHESTRATOR, title: 'conductor', status: 'WORKING' },
          { agent_session_id: 'qchat-a', title: 'question: q-4158 · merges', status: 'WAITING' },
          { agent_session_id: 'qchat-b', title: 'question: q-12', status: 'IDLE' },
          { agent_session_id: 'other', title: 'repair: question: not mine', status: 'IDLE' },
        ],
      })
    if (args[0] === 'chat' && args[1] === 'delete')
      return JSON.stringify({ agent_session_id: args[2], deleted: true })
    throw new Error(`unexpected boss call ${args.join(' ')}`)
  }
  return { runBoss, calls }
}

test('ls lists only the question chats ask opened, with their question ids', () => {
  const boss = sessionBoss()
  assert.deepEqual(listQuestions({ session: 'sess-1', runBoss: boss.runBoss }), [
    { chat: 'qchat-a', id: 'q-4158', status: 'WAITING' },
    { chat: 'qchat-b', id: 'q-12', status: 'IDLE' },
  ])
})

test('resolve reads the answer and closes exactly the question chat that asked it', () => {
  const boss = sessionBoss()
  const result = resolve({
    message: 'ANSWER q-4158: {"bs #4158":"Merge it (Recommended)"}',
    session: 'sess-1',
    runBoss: boss.runBoss,
  })
  assert.deepEqual(result, {
    id: 'q-4158',
    answers: { 'bs #4158': 'Merge it (Recommended)' },
    closed: ['qchat-a'],
  })
  const deletes = boss.calls.filter((c) => c[0] === 'chat' && c[1] === 'delete').map((c) => c[2])
  assert.deepEqual(deletes, ['qchat-a'])
})

test('resolve still returns the answers when the question chat is already closed', () => {
  const boss = sessionBoss()
  const result = resolve({
    message: 'ANSWER q-99: {"x":"A"}',
    session: 'sess-1',
    runBoss: boss.runBoss,
  })
  assert.deepEqual(result.closed, [])
  assert.deepEqual(result.answers, { x: 'A' })
})
