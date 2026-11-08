#!/usr/bin/env node

// The orchestrator's durable operator queue: one boss note per decision only the operator can make,
// under one tag, keyed by a stable first line. An unattended worker round syncs the queue from its
// snapshot; the attended conduit reads it, asks the operator and records each answer.
//
// A note body is
//   key: <stable key>
//   title: <one line>
//   <detail ...>
//   answer: <the operator's answer, once given>
//
// A key names the condition that raised the item: a session row in one action (`host= session= pr=
// action=` plus `reason=` for a park's codes), an `orchestrate-round` extension item (`extension=
// title=`), or a free `topic=` the worker raised. Sync adds what is missing, edits an unanswered
// item whose detail changed, removes duplicates, and removes an item once its condition is known
// to have cleared. An item on a host or session the round could not read is kept: unknown is not
// cleared. An answered item is never re-asked; it leaves the queue when its condition clears.
//
//   node orchestrate-queue.mjs sync --snapshot <collect --json file> [--extra <items.json>] [--dry-run]
//   node orchestrate-queue.mjs ls [--all] [--json]
//   node orchestrate-queue.mjs answer --id <note-id> --answer <text>
//   node orchestrate-queue.mjs resolve --id <note-id>
// Every verb takes [--tag <tag>] [--repo <repo-id>] [--queue-host <ssh-host>] [--boss <boss>].

import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'

import { isMainModule } from '../main-module.mjs'

export const QUEUE_TAG = 'orchestrate-question'
const ANSWERED_SUFFIX = '-answered'

// Rows the worker moves itself. Every other core action waits, is in flight, or is the operator's.
export const AGENT_MOVABLE = new Set([
  'waive-cap',
  'unblock',
  'upstream-red',
  'repair',
  'verify',
  'refresh-park',
  'mark-ready',
  'stalled',
])
// Rows whose decision is always the operator's, so the snapshot itself queues them.
const OPERATOR_ACTIONS = new Set(['awaiting-operator', 'ask-operator'])

const oneLine = (value) =>
  String(value ?? '')
    .replace(/\s+/g, ' ')
    .trim()
const token = (value) => oneLine(value).replace(/ /g, '-') || '-'

/** The park codes of an `awaiting-operator` row: its `why` before the ` · ` label. */
function parkReason(row) {
  return row.action === 'awaiting-operator' ? token(String(row.why ?? '').split(' · ')[0]) : ''
}

function rowKey({ host, session, pr, action, reason }) {
  const parts = [`host=${token(host)}`, `session=${token(session)}`, `pr=${pr ?? '-'}`]
  parts.push(`action=${token(action)}`)
  if (reason) parts.push(`reason=${reason}`)
  return parts.join(' ')
}

/** The stable key of a queue item. */
export function itemKey(item) {
  if (item.extension) return `extension=${token(item.extension)} title=${oneLine(item.title)}`
  if (item.topic) return `topic=${token(item.topic)}`
  return rowKey(item)
}

function render(item, answer = '') {
  const lines = [`key: ${item.key}`, `title: ${oneLine(item.title)}`]
  const detail = String(item.detail ?? '').trim()
  if (detail) lines.push(detail)
  if (answer) lines.push(`answer: ${oneLine(answer)}`)
  return lines.join('\n')
}

/** A queue item from a note, or null when the note under the tag is not one. */
export function parseNote(note, tag = QUEUE_TAG) {
  const lines = String(note.body ?? '').split('\n')
  const key = /^key: (.+)$/.exec(lines[0] ?? '')?.[1]?.trim()
  if (!key) return null
  const title = /^title: (.*)$/.exec(lines[1] ?? '')?.[1] ?? ''
  const rest = lines.slice(title || /^title:/.test(lines[1] ?? '') ? 2 : 1)
  const answerAt = rest.findIndex((line) => line.startsWith('answer: '))
  const answer = answerAt === -1 ? '' : rest[answerAt].slice('answer: '.length)
  const detail = (answerAt === -1 ? rest : rest.slice(0, answerAt)).join('\n').trim()
  const fields = Object.fromEntries(
    [...key.matchAll(/(\w+)=(\S+)/g)].map(([, name, value]) => [name, value]),
  )
  return {
    id: note.id,
    key,
    title,
    detail,
    answer,
    answered: (note.tags ?? []).includes(`${tag}${ANSWERED_SUFFIX}`) || Boolean(answer),
    created_at: note.created_at ?? '',
    fields,
    body: String(note.body ?? ''),
  }
}

function rowLabel(row) {
  const name = String(row.repo ?? '')
    .split('/')
    .pop()
  return row.pr ? `${name}#${row.pr}` : `${name} session ${row.session}`
}

/** The items the snapshot itself puts on the queue: operator rows and extension items. */
export function snapshotItems(snapshot) {
  const items = []
  for (const row of snapshot.rows ?? []) {
    if (!OPERATOR_ACTIONS.has(row.action) || row.unread) continue
    const item = {
      host: row.host,
      session: row.session,
      pr: row.pr ?? null,
      action: row.action,
      reason: parkReason(row),
      title: `${rowLabel(row)} ${oneLine(row.title)}`.trim(),
      detail: `${row.action === 'awaiting-operator' ? 'Waiting on review: ' : 'Draft hold: '}${row.why}`,
    }
    items.push({ ...item, key: itemKey(item) })
  }
  for (const extra of snapshot.operatorItems ?? []) {
    const item = { extension: extra.extension, title: extra.title, detail: extra.detail }
    items.push({ ...item, key: itemKey(item) })
  }
  return items
}

const rowAt = (snapshot, host, session) =>
  (snapshot.rows ?? []).find((row) => row.host === host && row.session === session)

/**
 * Whether a queued item's condition still holds in this snapshot. Unknown counts as holding: a
 * host or session the round could not read, or an extension that did not run.
 */
function stillHolds(item, snapshot) {
  const { fields } = item
  if (fields.topic) return true
  if (fields.extension) {
    if (!(snapshot.roundExtensions ?? []).includes(fields.extension)) return true
    return (snapshot.operatorItems ?? []).some((extra) => itemKey(extra) === item.key)
  }
  if (!fields.host || !fields.session) return true
  if ((snapshot.unread ?? []).includes(fields.host)) return true
  const row = rowAt(snapshot, fields.host, fields.session)
  if (!row) return false
  if (row.unread) return true
  if (row.action !== fields.action) return false
  return !fields.reason || parkReason(row) === fields.reason
}

/** Validates the worker's extra items against the snapshot; a row item must match a current row. */
function acceptExtras(extras, snapshot) {
  const accepted = []
  const rejected = []
  for (const extra of extras) {
    const item = { ...extra, key: itemKey(extra) }
    if (!oneLine(extra.title)) rejected.push({ item, why: 'missing title' })
    else if (extra.topic || extra.extension) accepted.push(item)
    else if (!extra.host || !extra.session || !extra.action)
      rejected.push({ item, why: 'a row item needs host, session and action (or a topic)' })
    else if (rowAt(snapshot, extra.host, extra.session)?.action !== extra.action)
      rejected.push({ item, why: `no ${extra.action} row for ${extra.host} ${extra.session}` })
    else accepted.push(item)
  }
  return { accepted, rejected }
}

/**
 * What a sync would change. `items` are the parsed queue notes, oldest first; `extras` are the
 * worker's own decisions for the operator this round.
 */
export function planQueue({ snapshot, items, extras = [] }) {
  const { accepted, rejected } = acceptExtras(extras, snapshot)
  const desired = new Map()
  for (const item of [...snapshotItems(snapshot), ...accepted])
    if (!desired.has(item.key)) desired.set(item.key, item)
  const plan = { add: [], update: [], remove: [], keep: [], rejected }
  const present = new Set()
  for (const item of items) {
    if (present.has(item.key)) {
      plan.remove.push({ id: item.id, key: item.key, why: 'duplicate' })
      continue
    }
    present.add(item.key)
    const want = desired.get(item.key)
    if (want) {
      if (!item.answered && render(want) !== item.body)
        plan.update.push({ id: item.id, item: want })
      else plan.keep.push(item)
    } else if (stillHolds(item, snapshot)) plan.keep.push(item)
    else plan.remove.push({ id: item.id, key: item.key, why: 'condition cleared' })
  }
  for (const [key, item] of desired) if (!present.has(key)) plan.add.push(item)
  return plan
}

/**
 * The `boss notes` operations the queue needs. `run(args)` returns boss's stdout; `bossHost`
 * drives the daemon on another machine, and `repo` scopes listing and adding (default: boss's own
 * working-directory repo).
 */
export function notesClient({ run, tag = QUEUE_TAG, repo, bossHost }) {
  const boss = (args) => run([...(bossHost ? ['--host', bossHost] : []), 'notes', ...args])
  const scope = repo === undefined ? [] : ['--repo', repo]
  const answeredTag = `${tag}${ANSWERED_SUFFIX}`
  const list = () =>
    JSON.parse(boss(['ls', '--tag', tag, '--json', ...scope]) || '[]')
      .map((note) => parseNote(note, tag))
      .filter(Boolean)
  return {
    tag,
    list,
    add(item) {
      const key = `${tag}/${item.key}`
      const args = ['add', '--tag', tag, '--idempotency-key', key, '--json', ...scope]
      return JSON.parse(boss([...args, '--', render(item)]))
    },
    update(id, item) {
      return JSON.parse(boss(['edit', id, '--body', render(item), '--json', ...scope]))
    },
    answer(id, text) {
      const item = list().find((entry) => entry.id === id)
      if (!item) throw new Error(`no queue item ${id} under tag ${tag}`)
      const body = render(item, text)
      return JSON.parse(
        boss(['edit', id, '--body', body, '--tag', tag, '--tag', answeredTag, '--json', ...scope]),
      )
    },
    resolve(id) {
      boss(['rm', id, ...scope])
    },
  }
}

/** Plans and applies one sync; returns what changed. */
export function syncQueue({ snapshot, client, extras = [], dryRun = false }) {
  const plan = planQueue({ snapshot, items: client.list(), extras })
  if (!dryRun) {
    for (const item of plan.add) client.add(item)
    for (const { id, item } of plan.update) client.update(id, item)
    for (const { id } of plan.remove) client.resolve(id)
  }
  return {
    added: plan.add.map((item) => item.key),
    updated: plan.update.map(({ item }) => item.key),
    removed: plan.remove.map(({ key, why }) => ({ key, why })),
    kept: plan.keep.length,
    rejected: plan.rejected.map(({ item, why }) => ({ key: item.key, why })),
  }
}

/** Whether a queue item already carries this row's decision. */
function queuedFor(row, items) {
  return items.some(
    (item) =>
      item.fields.host === token(row.host) &&
      item.fields.session === token(row.session) &&
      item.fields.action === row.action,
  )
}

/**
 * The cron-gate verdict for a worker round: exit 0 when some readable row is agent-movable and its
 * decision is not already queued, or the queue is out of sync; 69 when a host could not be read; 1
 * otherwise. `reason` is one line for the gate log.
 */
export function gateVerdict({ snapshot, items }) {
  if ((snapshot.unread ?? []).length) {
    const first = (snapshot.errors ?? [])[0] ?? ''
    return { exit: 69, reason: `could not read ${snapshot.unread.join(', ')}: ${oneLine(first)}` }
  }
  const movable = (snapshot.rows ?? []).filter(
    (row) => AGENT_MOVABLE.has(row.action) && !row.unread && !queuedFor(row, items),
  )
  if (movable.length) {
    const sample = movable
      .slice(0, 3)
      .map((row) => `${row.action} ${rowLabel(row)}`)
      .join(', ')
    return { exit: 0, reason: `${movable.length} agent-movable row(s): ${sample}` }
  }
  const plan = planQueue({ snapshot, items })
  const changes = plan.add.length + plan.update.length + plan.remove.length
  if (changes)
    return {
      exit: 0,
      reason: `operator queue out of sync: +${plan.add.length} ~${plan.update.length} -${plan.remove.length}`,
    }
  return {
    exit: 1,
    reason: `no agent-movable row in ${(snapshot.rows ?? []).length} and the operator queue is in sync`,
  }
}

const readJson = (file) => JSON.parse(readFileSync(file, 'utf8'))

/** @param {string} bossBin */
export function bossRunner(bossBin) {
  /** @param {string[]} args */
  return (args) => execFileSync(bossBin, args, { encoding: 'utf8', maxBuffer: 64 << 20 })
}

function main(argv) {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    strict: true,
    options: {
      snapshot: { type: 'string' },
      extra: { type: 'string' },
      'dry-run': { type: 'boolean', default: false },
      all: { type: 'boolean', default: false },
      json: { type: 'boolean', default: false },
      id: { type: 'string' },
      answer: { type: 'string' },
      tag: { type: 'string', default: QUEUE_TAG },
      repo: { type: 'string' },
      'queue-host': { type: 'string' },
      boss: { type: 'string', default: process.env.BOSS_BIN || 'boss' },
    },
  })
  const [command] = positionals
  const client = notesClient({
    run: bossRunner(values.boss),
    tag: values.tag,
    repo: values.repo,
    bossHost: values['queue-host'],
  })
  const print = (value) => process.stdout.write(`${JSON.stringify(value, null, 2)}\n`)
  if (command === 'sync') {
    if (!values.snapshot) throw new Error('sync needs --snapshot <collect --json output>')
    const extras = values.extra ? readJson(values.extra) : []
    if (!Array.isArray(extras)) throw new Error('--extra must hold a JSON array of items')
    const result = syncQueue({
      snapshot: readJson(values.snapshot),
      client,
      extras,
      dryRun: values['dry-run'],
    })
    print(result)
    return result.rejected.length ? 2 : 0
  }
  if (command === 'ls') {
    const items = client.list().filter((item) => values.all || !item.answered)
    if (values.json) print(items.map(({ body: _body, ...item }) => item))
    else
      for (const item of items)
        process.stdout.write(
          `${item.id} ${item.answered ? '[answered] ' : ''}${item.title}\n    ${item.key}\n`,
        )
    return 0
  }
  if (command === 'answer') {
    if (!values.id || !values.answer) throw new Error('answer needs --id <note-id> --answer <text>')
    print(client.answer(values.id, values.answer))
    return 0
  }
  if (command === 'resolve') {
    if (!values.id) throw new Error('resolve needs --id <note-id>')
    client.resolve(values.id)
    return 0
  }
  process.stderr.write('usage: orchestrate-queue.mjs sync|ls|answer|resolve [options]\n')
  return 64
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
