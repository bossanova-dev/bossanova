#!/usr/bin/env node
// Records a finished run's improvement notes in the boss note store, for the built-in `notes`
// extensions every core ships. The extension's agent turns the run's observations into note blocks;
// this helper shapes, filters and writes them, writes one result envelope to the dispatch's
// `outPath`, and always exits 0: a notes failure never changes the run.
//
//   node bs-record-notes.mjs --envelope <path> --notes <path> [--extension <name>]
import { spawnSync } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import fs from 'node:fs'

export const MAX_NOTES_BYTES = 8 * 1024
export const MAX_NOTES = 5
const NOTE_TAG = 'improvement'

export const RUN_TOKEN_PREFIX = 'run:'
let adhocRunId

export function sanitizeRunId(value) {
  return (
    String(value ?? '')
      .replace(/[^A-Za-z0-9._:#@-]/g, '')
      .slice(0, 80) || null
  )
}

export function resetAdhocRunId() {
  adhocRunId = undefined
}

export function resolveRunId({ runId, env = process.env } = {}) {
  for (const value of [runId, env.BOSS_AGENT_SESSION_ID, env.BOSS_SESSION_ID]) {
    const id = sanitizeRunId(value)
    if (id) return id
  }
  return (adhocRunId ??= `adhoc-${randomBytes(4).toString('hex')}`)
}

export function formatRunLine(parts, runId) {
  return `Run: ${parts.join(' / ')} / ${RUN_TOKEN_PREFIX}${sanitizeRunId(runId) || resolveRunId()}`
}

export function parseRunId(body) {
  const lines = String(body ?? '')
    .split(/\r?\n/)
    .filter((line) => line.trim().startsWith('Run:'))
  const segments = lines
    .at(-1)
    ?.trim()
    .slice(4)
    .split(/\s*[\/·]\s*/)
  const token = segments?.findLast((part) => part.startsWith(RUN_TOKEN_PREFIX))
  return token ? sanitizeRunId(token.slice(RUN_TOKEN_PREFIX.length)) : null
}

// Credential shapes a note must never carry. A note body is readable by everyone with repo access,
// so a note matching one is dropped rather than redacted.
const SECRET_PATTERNS = [
  /-----BEGIN [A-Z ]*PRIVATE KEY-----/,
  /\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}/,
  /\bgithub_pat_[A-Za-z0-9_]{20,}/,
  /\bsk-[A-Za-z0-9_-]{20,}/,
  /\bxox[abprs]-[A-Za-z0-9-]{10,}/,
  /\bAKIA[0-9A-Z]{16}\b/,
  /\blin_(?:api|oauth)_[A-Za-z0-9]{20,}/,
  /\b(?:password|passwd|secret|api[_-]?key|token)\s*[:=]\s*\S{6,}/i,
]

export function isSecretBearing(text) {
  return SECRET_PATTERNS.some((pattern) => pattern.test(text))
}

export const NOTE_LABELS = ['Where:', 'Why it matters:', 'Suggested fix:']

// Splits the agent-written notes file into note blocks (separated by blank lines) and shapes each
// into the five-line body sweeps parse:
//
//   <one-line problem statement>
//   Where: <file, skill, command or gate>
//   Why it matters: <the cost>
//   Suggested fix: <the next action, or "unknown">
//   Run: <core> / <outcome> / <mode>
//
// The `Run:` line is always written here from the envelope; one the agent wrote is replaced. A
// block that does not have the other four lines in that order is rejected with a reason.
export function parseNotes(markdown, run) {
  const blocks = markdown
    .replace(/\r\n/g, '\n')
    .split(/\n\s*\n/)
    .map((block) =>
      block
        .split('\n')
        .map((line) => line.trim())
        .filter((line) => line !== '' && !line.startsWith('Run:')),
    )
    .filter((lines) => lines.length > 0)
  const bodies = []
  const rejected = []
  for (const lines of blocks) {
    const shaped =
      lines.length === 4 &&
      !/^(?:Where|Why it matters|Suggested fix):/.test(lines[0]) &&
      NOTE_LABELS.every((label, i) => lines[i + 1].startsWith(`${label} `))
    if (shaped) {
      bodies.push([...lines, `Run: ${run}`].join('\n'))
    } else {
      rejected.push(lines[0])
    }
  }
  return { bodies, rejected }
}

export function idempotencyKey(body) {
  return `bs-record-notes:${createHash('sha256').update(body).digest('hex').slice(0, 32)}`
}

function defaultAddNote({ body, repoId }) {
  const argv = [
    'notes',
    'add',
    '--tag',
    NOTE_TAG,
    '--json',
    '--idempotency-key',
    idempotencyKey(body),
  ]
  if (repoId) argv.push('--repo', repoId)
  // `--` so a body starting with a dash is never read as a flag.
  argv.push('--', body)
  const result = spawnSync(process.env.BOSS_BIN || 'boss', argv, { encoding: 'utf8' })
  if (result.error) throw result.error
  if (result.status !== 0) {
    throw new Error(`boss notes add exited ${result.status}: ${(result.stderr || '').trim()}`)
  }
  const noteId = JSON.parse(result.stdout)?.id
  if (typeof noteId !== 'string' || noteId === '') throw new Error('boss notes add returned no id')
  return noteId
}

// Pure core of the helper: envelope and notes file in, result envelope out. `addNote` is injected
// for tests.
export function recordNotes(
  envelope,
  { notesPath, extension, addNote = defaultAddNote, env = process.env } = {},
) {
  const context = envelope?.context ?? {}
  const core = context.core || envelope?.core || 'unknown'
  const name = extension || envelope?.extension || `${core}-notes`
  const result = (fields) => ({
    ok: true,
    extension: name,
    role: 'notes',
    items: [],
    notes: '',
    error: null,
    ...fields,
  })
  if (typeof notesPath !== 'string' || notesPath === '') {
    return result({ ok: false, error: 'no notes file given' })
  }
  let markdown
  try {
    if (fs.statSync(notesPath).size > MAX_NOTES_BYTES) {
      return result({ ok: false, error: `notes file exceeds ${MAX_NOTES_BYTES} bytes` })
    }
    markdown = fs.readFileSync(notesPath, 'utf8')
  } catch (err) {
    return result({ ok: false, error: `cannot read notes file: ${err.message}` })
  }
  const run = formatRunLine(
    [core, context.outcome || 'unknown', context.mode || 'unknown'],
    resolveRunId({ runId: context.runId, env }),
  ).slice(5)
  const { bodies, rejected } = parseNotes(markdown, run)
  const unique = [...new Set(bodies)]
  const kept = unique.filter((body) => !isSecretBearing(body))
  const capped = kept.slice(0, MAX_NOTES)
  const items = []
  const failures = []
  for (const body of capped) {
    try {
      items.push({ tag: NOTE_TAG, body, noteId: addNote({ body, repoId: context.repoId }) })
    } catch (err) {
      failures.push(err.message)
    }
  }
  const notes = [
    rejected.length > 0 ? `${rejected.length} note(s) not in the five-line shape` : '',
    bodies.length > unique.length
      ? `${bodies.length - unique.length} note(s) repeated within this run`
      : '',
    unique.length > kept.length
      ? `${unique.length - kept.length} note(s) dropped (secret shape)`
      : '',
    kept.length > capped.length
      ? `${kept.length - capped.length} note(s) over the cap of ${MAX_NOTES}`
      : '',
    failures.length > 0 ? `${failures.length} write(s) failed: ${failures[0]}` : '',
  ]
    .filter(Boolean)
    .join('; ')
  if (items.length === 0 && failures.length > 0) {
    return result({ ok: false, notes, error: `boss notes add failed: ${failures[0]}` })
  }
  return result({ items, notes })
}

function parseArgs(argv) {
  const args = {}
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i].startsWith('--') && argv[i + 1] !== undefined && !argv[i + 1].startsWith('--')) {
      args[argv[i].slice(2)] = argv[i + 1]
      i += 1
    }
  }
  return args
}

export function main(argv, deps = {}) {
  const args = parseArgs(argv)
  if (!args.envelope && !args.core) {
    process.stderr.write(
      'usage: bs-record-notes.mjs --envelope <path> --notes <path> [--extension <name>] | --notes <path> --core <skill> [--outcome <id|none>] [--mode headless|interactive] [--run-id <id>]\n',
    )
    return 0
  }
  let envelope
  try {
    envelope = args.envelope
      ? JSON.parse(fs.readFileSync(args.envelope, 'utf8'))
      : {
          core: args.core,
          context: {
            core: args.core,
            outcome: args.outcome || 'none',
            mode: args.mode || 'headless',
            runId: args['run-id'],
          },
        }
  } catch (err) {
    process.stderr.write(`bs-record-notes: cannot read envelope: ${err.message}\n`)
    return 0
  }
  const out = recordNotes(envelope, { notesPath: args.notes, extension: args.extension, ...deps })
  const json = `${JSON.stringify(out)}\n`
  if (typeof envelope.outPath === 'string' && envelope.outPath !== '') {
    try {
      fs.writeFileSync(envelope.outPath, json)
    } catch (err) {
      process.stderr.write(`bs-record-notes: cannot write ${envelope.outPath}: ${err.message}\n`)
    }
  }
  ;(deps.stdout ?? ((text) => process.stdout.write(text)))(json)
  return 0
}

import { isMainModule } from './main-module.mjs'

if (isMainModule(import.meta.url, { warn: () => {} })) {
  process.exit(main(process.argv.slice(2)))
}
