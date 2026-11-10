// verify-replay.mjs — replays a repo's open PRs through the verify judge under candidate merge
// policies and reports what each would do, writing nothing.
//
//   node verify-replay.mjs [--repo o/r] [--pr <n>[,<n>…]] [--candidate <name>=<file>]…
//                          [--format json|markdown]
//
// A candidate is a `verifyDefaults.merge` block of structured `auto` rules, read as JSON from its
// file; the default candidate (no merge block, so nothing merges without a person) always runs
// first. Each PR is judged in process with the candidate served in place of the base tip's merge
// block, through a reader that refuses every writing call. Extensions and the LLM judge are never
// run: the head's recorded results are reused, and a head without them reports `unknown`.
// Exit 0, 1 when any writing call was refused (or a read the run needs failed), 2 on a usage error.
import { createHash } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, join } from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { CONFIG_FILENAME, verifyPolicyFromConfigText } from '../skill-config.mjs'
import {
  UsageError,
  classifyReadCall,
  commandReader,
  judge,
  readOperatorMarkers,
  readPr,
  recordedExtensions,
  repoFor,
  writeAwareReader,
} from './verify-gate.mjs'
import { levelLabel, validConfidence } from './verify-level.mjs'
import { readOpenPrs } from './verify-open-prs.mjs'
import { parkNote } from './verify-park-note.mjs'

export const REFUSED = 'replay-write-refused'
export const DEFAULT_CANDIDATE = 'default'

// The `boss` verbs the judge path reads with; any other verb may write and is refused.
const BOSS_READ_VERBS = new Set(['ls', 'show', 'version', '--version'])
// The git verbs that write to a remote.
const GIT_REMOTE_WRITES = new Set(['push', 'send-pack', 'send-email'])
// git's global options that take the next argument as their value.
const GIT_VALUE_OPTIONS = new Set(['-C', '-c', '--git-dir', '--work-tree', '--namespace'])

const plainObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
const canonicalJson = (value) =>
  Array.isArray(value)
    ? `[${value.map(canonicalJson).join(',')}]`
    : plainObject(value)
      ? `{${Object.keys(value)
          .sort()
          .map((key) => `${JSON.stringify(key)}:${canonicalJson(value[key])}`)
          .join(',')}}`
      : JSON.stringify(value ?? null)

function gitVerb(args) {
  for (let i = 0; i < args.length; i++) {
    const arg = String(args[i])
    if (GIT_VALUE_OPTIONS.has(arg)) i++
    else if (!arg.startsWith('-')) return arg
  }
  return ''
}

/**
 * Whether a replay may run one call: `read` for a `gh` call the gate's write-aware reader caches
 * (classifyReadCall), a `boss` read verb, any git verb that does not write to a remote, and every
 * other command (tar, node); `write` for everything else.
 * @param {string} command @param {readonly unknown[]} [args] @returns {'read'|'write'}
 */
export function replayCallKind(command, args = []) {
  const name = basename(String(command))
  const list = args.map(String)
  if (name === 'gh') return classifyReadCall('gh', list) === 'cached' ? 'read' : 'write'
  if (name === 'boss') return BOSS_READ_VERBS.has(list[0] ?? '') ? 'read' : 'write'
  if (name === 'git') return GIT_REMOTE_WRITES.has(gitVerb(list)) ? 'write' : 'read'
  return 'read'
}

const callSummary = (command, args) =>
  [basename(String(command)), ...args.slice(0, 3).map(String)].join(' ').slice(0, 120)

/**
 * A reader over `inner` that runs only replayCallKind reads; a writing call is recorded in
 * `refused` and answered `{ok: false, error: REFUSED}` without reaching `inner`.
 * @param {Function} inner @param {string[]} [refused]
 */
export function guardedReader(inner, refused = []) {
  const guard = (command, args = [], opts) => {
    if (replayCallKind(command, args) === 'write') {
      refused.push(callSummary(command, args))
      return { ok: false, error: REFUSED, exit: null, output: '' }
    }
    return inner(command, args, opts)
  }
  guard.refused = refused
  return guard
}

/** The same guard over a `spawnSync`-shaped runner, for the tools the judge spawns directly. */
export function guardedSpawn(spawn, refused) {
  return (command, args = [], opts) => {
    if (replayCallKind(command, args) === 'write') {
      refused.push(callSummary(command, args))
      const error = Object.assign(new Error(REFUSED), { code: REFUSED })
      return { status: 1, signal: null, stdout: '', stderr: REFUSED, error }
    }
    return spawn(command, args, opts)
  }
}

const isNotFound = (r) =>
  !r.ok && (r.payload?.status === '404' || r.payload?.message === 'Not Found')

/**
 * The base config read `r` with its `verifyDefaults.merge` replaced by `merge` (removed when
 * `merge` is undefined, the default candidate); every other key is kept. A base without a config
 * file stays absent for the default candidate. A read that failed otherwise, or a file that does
 * not parse, is returned as it is, so the gate judges it as it would.
 */
export function overlayMergeBlock(r, merge) {
  if (!r.ok && !(isNotFound(r) && merge !== undefined)) return r
  let config = {}
  if (r.ok) {
    const p = r.payload
    if (p?.type !== 'file' || p?.encoding !== 'base64' || typeof p?.content !== 'string') return r
    try {
      config = JSON.parse(Buffer.from(p.content, 'base64').toString('utf8'))
    } catch {
      return r
    }
    if (!plainObject(config)) return r
  }
  const verifyDefaults = plainObject(config.verifyDefaults) ? { ...config.verifyDefaults } : {}
  if (merge === undefined) delete verifyDefaults.merge
  else verifyDefaults.merge = merge
  const content = Buffer.from(JSON.stringify({ ...config, verifyDefaults })).toString('base64')
  return { ok: true, payload: { type: 'file', encoding: 'base64', content }, output: '', exit: 0 }
}

const CONFIG_READ = new RegExp(
  `^repos/[^?]+/contents/${CONFIG_FILENAME.replaceAll('.', '\\.')}\\?ref=[0-9a-fA-F]{40}$`,
)
const isConfigRead = (command, args) =>
  command === 'gh' && args.length === 2 && args[0] === 'api' && CONFIG_READ.test(String(args[1]))

/** A write-aware reader over `shared` that serves the candidate's merge block at the base tip. */
function candidateReader(shared, merge) {
  const reader = (command, args = [], opts) =>
    isConfigRead(command, args)
      ? overlayMergeBlock(shared(command, args, opts), merge)
      : shared(command, args, opts)
  reader.writeAware = true
  reader.uncached = shared.uncached
  return reader
}

/**
 * The extension-results file a recorded run stands for, in the results contract the gate's
 * extension aggregation reads (`{extension, optional, timedOut, result}`). `record` is the gate's
 * `[{extension, outcome}]`.
 */
export function recordedResults(record, headSha) {
  const evidence = (kind) => [{ kind, ref: `extension record on ${headSha}` }]
  return (Array.isArray(record) ? record : []).map(({ extension, outcome }) => ({
    extension,
    optional: false,
    timedOut: false,
    result: {
      ok: true,
      extension,
      role: 'verify',
      ...(outcome === 'abstain'
        ? { verdict: 'abstain', evidence: [] }
        : outcome === 'pass' || outcome === 'recorded'
          ? { verdict: 'pass', evidence: evidence(outcome === 'recorded' ? 'recording' : 'record') }
          : { verdict: 'human', reason: outcome, evidence: evidence('record') }),
      findings: [],
    },
  }))
}

/**
 * A candidate's `verifyDefaults.merge` block, checked with the validator the gate reads the base
 * tip with. A prose `policy` is refused: replaying it needs the LLM judge run per candidate.
 */
export function validateCandidate(name, merge) {
  if (!plainObject(merge))
    throw new UsageError(`verify-replay: candidate ${name} must hold a JSON object`)
  if (Object.hasOwn(merge, 'policy'))
    throw new UsageError(
      `verify-replay: candidate ${name} sets merge.policy; prose candidates are not replayed`,
    )
  try {
    verifyPolicyFromConfigText(
      JSON.stringify({ verifyDefaults: { merge } }),
      `verify-replay candidate ${name}`,
    )
  } catch (error) {
    throw new UsageError(`verify-replay: ${error.message}`)
  }
  return merge
}

const OUTCOMES = { pass: 'merge', human: 'park', blocked: 'blocked', defect: 'defect' }

const fromOf = (ask) =>
  ask.code !== 'always-human-path'
    ? null
    : ask.source === 'config' || ask.source === 'cap'
      ? 'config'
      : ask.area.startsWith('extension:')
        ? 'extension'
        : null

/** What decided a merge: an approval, the level-only rule, or the rules covering every ask. */
function mergeDecidedBy(judged, merge, asks) {
  if (judged.reason === 'approved') return [{ by: 'approval' }]
  if (merge?.levelRule) return [{ by: 'level-rule', rule: merge.levelRule }]
  const rules = asks.map((ask) => ask.rule).filter(Boolean)
  const unique = [...new Map(rules.map((rule) => [rule.index, rule])).values()]
  return unique.length ? [{ by: 'rules', rules: unique }] : [{ by: 'no-asks' }]
}

/** What decided each park code: no level rule, a floor or uncovered ask, or the gate itself. */
function parkDecidedBy(codes, level, merge, asks) {
  return codes.map((code) => {
    if (code === 'human-merge' && level === 'high' && !merge?.levelRule)
      return { code, by: 'no-level-rule' }
    const raised = asks.filter((ask) => ask.code === code && !ask.covered)
    if (!raised.length) return { code, by: 'gate' }
    return {
      code,
      by: raised.every((ask) => ask.floor) ? 'floor' : 'uncovered',
      asks: raised.map((ask) => ask.label),
    }
  })
}

/**
 * One report row: the judge output `judged` for PR `pr` under `candidate`, or `unknown` when the
 * replay could not judge it. A park carries the note `post` would leave for it.
 */
export function replayRow({ candidate, pr, headSha, prView = {}, judged = {}, unknown, refused }) {
  const confidence = validConfidence(judged.confidence) ? judged.confidence : null
  const merge = confidence?.merge ?? null
  const decided = (merge?.decisions ?? []).map((ask) => ({ ...ask, from: fromOf(ask) }))
  const asks = decided.map(({ area, label, source, code, floor, rule, prose, from }) => ({
    area,
    label,
    source,
    code,
    floor,
    rule,
    ...(prose ? { prose } : {}),
    ...(from ? { from } : {}),
  }))
  const outcome = unknown
    ? unknown
    : (OUTCOMES[judged.verdict] ??
      (judged.verdict === 'wait'
        ? `wait: ${judged.reason}`
        : `unknown: ${judged.reason ?? judged.verdict}`))
  const codes = outcome === 'park' && Array.isArray(judged.reasons) ? judged.reasons : []
  const row = {
    candidate,
    pr,
    headSha,
    outcome,
    codes,
    asks,
    decidedBy:
      outcome === 'merge'
        ? mergeDecidedBy(judged, merge, decided)
        : outcome === 'park'
          ? parkDecidedBy(codes, confidence?.level, merge, decided)
          : [
              {
                by: 'gate',
                reason: unknown?.split(': ')[1] ?? judged.reason ?? judged.verdict ?? '',
              },
            ],
  }
  if (outcome === 'park')
    row.note = parkNote({
      codes,
      headSha,
      facts: {
        title: prView.title,
        body: prView.body,
        label: confidence ? levelLabel(confidence) : '',
        confidence: judged.confidence,
      },
    }).body
  if (
    codes.includes('always-human-path') &&
    !asks.some((ask) => ask.code === 'always-human-path' && ask.from)
  )
    row.unattributedAlwaysHumanPath = true
  if (refused?.length) row.refused = refused
  return row
}

/** Judges one open PR under one candidate, reusing the head's recorded extension run. */
async function replayOne({ repo, pr, headSha, record, read, deps, dir }) {
  const recorded = new Set((record ?? []).map((entry) => entry.extension))
  const run = async (results, n) => {
    const file = join(dir, `results-${pr}-${n}.json`)
    writeFileSync(file, JSON.stringify(results))
    return judge({ pr, repo, extensionResults: file }, { ...deps, read })
  }
  const results = recordedResults(record, headSha)
  let judged = await run(results, 0)
  const aggregated = Array.isArray(judged.extensions?.outcomes)
  const applicable = judged.facts?.extensionsApplicable ?? []
  if (aggregated && applicable.some((name) => !recorded.has(name)))
    return { judged, unknown: 'unknown: extensions-unrecorded' }
  // A recorded extension the base no longer installs returns no result on a real run.
  const kept = results.filter((entry) => applicable.includes(entry.extension))
  if (aggregated && kept.length !== results.length) judged = await run(kept, 1)
  return { judged, unknown: undefined }
}

const COUNTS = ['merge', 'park', 'blocked', 'defect', 'wait', 'unknown']

/**
 * The replay report for `prs` (numbers; every open PR when empty) of `repo` under the default
 * candidate and then `candidates` (`[{name, merge}]`). `deps.read` is the real reader, which only
 * ever sees calls the guard allows; the other deps reach `judge` as they are.
 */
export async function replay({ repo: repoOption, prs = [], candidates = [] }, deps = {}) {
  const refused = []
  const guard = guardedReader(deps.read ?? commandReader(deps), refused)
  const shared = writeAwareReader(guard)
  const judgeDeps = {
    ...deps,
    spawn: guardedSpawn(deps.spawn ?? spawnSync, refused),
  }
  const repo = repoFor(shared, repoOption)
  if (!repo) throw new Error('verify-replay: the repository could not be resolved; pass --repo')
  let numbers = prs
  let truncated = false
  if (!numbers.length) {
    const open = readOpenPrs(shared, repo)
    numbers = open.rows.map((row) => Number(row.pr.number)).sort((a, b) => a - b)
    truncated = open.truncated
  }
  const all = [{ name: DEFAULT_CANDIDATE, merge: undefined }, ...candidates]
  const heads = []
  for (const pr of numbers) {
    const view = readPr(shared, pr, repo)
    const p = view.ok ? (view.payload ?? {}) : null
    const headSha = String(p?.headRefOid ?? '').toLowerCase()
    const markers = p ? readOperatorMarkers(shared, repo, pr) : { readable: false }
    heads.push({
      pr,
      headSha,
      prView: { title: p?.title ?? '', body: p?.body ?? '' },
      skip: !p ? 'unknown: pr-unreadable' : p.state !== 'OPEN' ? 'not-replayable: pr-not-open' : '',
      record: markers.readable ? recordedExtensions(markers.markers, headSha) : null,
    })
  }
  const dir = mkdtempSync(join(tmpdir(), 'verify-replay-'))
  const rows = []
  try {
    for (const candidate of all) {
      const read = candidateReader(shared, candidate.merge)
      for (const head of heads) {
        const before = refused.length
        const base = { candidate: candidate.name, pr: head.pr, headSha: head.headSha }
        if (head.skip) {
          rows.push(replayRow({ ...base, unknown: head.skip }))
          continue
        }
        const out = await replayOne({ repo, ...head, read, deps: judgeDeps, dir })
        rows.push(
          replayRow({
            ...base,
            prView: head.prView,
            judged: out.judged,
            unknown: out.unknown,
            refused: refused.slice(before),
          }),
        )
      }
    }
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
  const perCandidate = Object.fromEntries(
    all.map((candidate) => {
      const counts = Object.fromEntries(COUNTS.map((key) => [key, 0]))
      for (const row of rows.filter((r) => r.candidate === candidate.name)) {
        const key = row.outcome.split(':')[0]
        counts[COUNTS.includes(key) ? key : 'unknown']++
      }
      return [candidate.name, counts]
    }),
  )
  return {
    repo,
    candidates: all.map((candidate) => ({
      name: candidate.name,
      digest: createHash('sha256')
        .update(canonicalJson(candidate.merge ?? null))
        .digest('hex'),
      rules: Array.isArray(candidate.merge?.auto) ? candidate.merge.auto : [],
    })),
    prs: heads.map((head) => ({ number: head.pr, headSha: head.headSha })),
    rows,
    summary: {
      perCandidate,
      unattributedAlwaysHumanPath: rows.filter((row) => row.unattributedAlwaysHumanPath).length,
      refusedWrites: refused.length,
      truncated,
    },
  }
}

const cell = (value) => String(value).replaceAll('|', '\\|').replaceAll('\n', ' ')
const ruleText = (hit) => `rule ${hit.index} ${canonicalJson(hit.rule)}`

function decidedText(entry) {
  if (entry.by === 'approval') return 'an approval of this head'
  if (entry.by === 'level-rule') return `${ruleText(entry.rule)} allows the head's level`
  if (entry.by === 'rules') return `${entry.rules.map(ruleText).join(', ')} cover every ask`
  if (entry.by === 'no-asks') return 'nothing to cover'
  if (entry.by === 'no-level-rule') return `${entry.code}: no rule allows a high head`
  if (entry.by === 'floor') return `${entry.code}: ${entry.asks.join(', ')} (no rule may cover)`
  if (entry.by === 'uncovered') return `${entry.code}: ${entry.asks.join(', ')} (no rule covers)`
  return entry.code ?? entry.reason ?? ''
}

/** The report as Markdown: per candidate a counts line, a table and each park's note. */
export function renderMarkdown(report) {
  const out = [`# Merge policy replay: ${report.repo}`, '']
  if (report.summary.truncated) out.push('Only the newest open PRs were replayed.', '')
  if (report.summary.refusedWrites)
    out.push(`**${report.summary.refusedWrites} writing call(s) were refused.**`, '')
  for (const candidate of report.candidates) {
    const counts = report.summary.perCandidate[candidate.name]
    out.push(`## ${candidate.name}`, '')
    out.push(
      candidate.rules.length
        ? `Rules: ${candidate.rules.map((rule, i) => `${i} ${canonicalJson(rule)}`).join(', ')}`
        : 'Rules: none',
      '',
    )
    out.push(COUNTS.map((key) => `${key} ${counts[key]}`).join(' · '), '')
    out.push('| PR | head | outcome | park codes | decided by |', '| --- | --- | --- | --- | --- |')
    const rows = report.rows.filter((row) => row.candidate === candidate.name)
    for (const row of rows)
      out.push(
        `| #${row.pr} | \`${row.headSha.slice(0, 12)}\` | ${cell(row.outcome)}${row.unattributedAlwaysHumanPath ? ' (unattributed protected path)' : ''} | ${cell(row.codes.join(', '))} | ${cell(row.decidedBy.map(decidedText).join('; '))} |`,
      )
    out.push('')
    for (const row of rows.filter((r) => r.note))
      out.push(
        `<details><summary>#${row.pr} park note</summary>`,
        '',
        row.note,
        '',
        '</details>',
        '',
      )
  }
  return `${out.join('\n').trimEnd()}\n`
}

function readCandidate(spec) {
  const at = spec.indexOf('=')
  const name = at > 0 ? spec.slice(0, at) : ''
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(name) || name === DEFAULT_CANDIDATE)
    throw new UsageError(`verify-replay: --candidate needs <name>=<file>, got ${spec}`)
  let merge
  try {
    merge = JSON.parse(readFileSync(spec.slice(at + 1), 'utf8'))
  } catch (error) {
    throw new UsageError(`verify-replay: candidate ${name} is unreadable: ${error.message}`)
  }
  return { name, merge: validateCandidate(name, merge) }
}

/** Parses argv and runs the replay: `{report, text, exitCode}`. Usage errors throw UsageError. */
export async function main(argv = process.argv.slice(2), deps = {}) {
  let values
  try {
    ;({ values } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: false,
      options: {
        repo: { type: 'string' },
        pr: { type: 'string' },
        candidate: { type: 'string', multiple: true },
        format: { type: 'string', default: 'json' },
      },
    }))
  } catch (error) {
    throw new UsageError(`verify-replay: ${error.message}`)
  }
  if (!['json', 'markdown'].includes(values.format))
    throw new UsageError('verify-replay: --format must be json or markdown')
  const prs = values.pr === undefined ? [] : values.pr.split(',')
  if (values.pr !== undefined && !prs.every((pr) => /^[1-9]\d{0,9}$/.test(pr)))
    throw new UsageError('verify-replay: --pr takes a comma list of PR numbers')
  const candidates = (values.candidate ?? []).map(readCandidate)
  const names = candidates.map((candidate) => candidate.name)
  if (new Set(names).size !== names.length)
    throw new UsageError('verify-replay: candidate names must be unique')
  const report = await replay(
    { repo: values.repo, prs: [...new Set(prs.map(Number))], candidates },
    deps,
  )
  return {
    report,
    text: values.format === 'markdown' ? renderMarkdown(report) : `${JSON.stringify(report)}\n`,
    exitCode: report.summary.refusedWrites > 0 ? 1 : 0,
  }
}

if (isMainModule(import.meta.url)) {
  try {
    const { text, exitCode } = await main()
    process.stdout.write(text)
    process.exitCode = exitCode
  } catch (error) {
    process.stderr.write(`${error.message}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
