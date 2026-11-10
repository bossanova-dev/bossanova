#!/usr/bin/env node

import { spawnSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { resolveBossBinary } from '../boss-binary.mjs'
import { isMainModule } from '../main-module.mjs'
import {
  buildBlockers,
  validateBlockerFile,
  validateOutcomeFile,
  encodeOutcomesTrailer,
} from '../verify/verify-blockers.mjs'
import { commandReader, readOperatorMarkers } from '../verify/verify-gate.mjs'

const line = (value) => typeof value === 'string' && value.trim() !== '' && !/[\r\n]/.test(value)
const jsonFile = (file) => JSON.parse(readFileSync(file, 'utf8'))
const runGit = (args) => {
  const r = spawnSync('git', args, { encoding: 'utf8', timeout: 30000 })
  return { ok: !r.error && r.status === 0, output: String(r.stdout ?? '').trim() }
}

function checkedBlockers(input) {
  const result = validateBlockerFile(input)
  if (!result.ok) throw new Error(result.errors.join('; '))
  return input
}

/** Print metadata as quoted data too; fence length is chosen from all untrusted content. */
export function blockerBrief(input) {
  checkedBlockers(input)
  const order = ['conflict', 'operator-note', 'review-finding', 'check', 'no-review']
  return (
    [...input.blockers]
      .sort((a, b) => order.indexOf(a.kind) - order.indexOf(b.kind))
      .map((b) => {
        const content = [
          `id: ${b.id}`,
          `kind: ${b.kind}`,
          `severity: ${b.severity ?? 'none'}`,
          `location: ${b.file ?? 'none'}${b.line === null ? '' : `:${b.line}`}`,
          '',
          b.summary,
          b.text,
        ].join('\n')
        const longest = Math.max(0, ...Array.from(content.matchAll(/`+/g), (m) => m[0].length))
        const fence = '`'.repeat(Math.max(3, longest + 1))
        return `${fence}\n${content}\n${fence}`
      })
      .join('\n\n') + '\n'
  )
}

export function blockerTitle(input) {
  checkedBlockers(input)
  const b = input.blockers[0]
  let subject
  if (b.kind === 'review-finding') {
    const last = b.summary
      .trim()
      .split(/\s+/)
      .map((word) => word.toLowerCase().replace(/[^\p{L}\p{N}]/gu, ''))
      .filter(Boolean)
      .at(-1)
    subject = `${b.severity} ${last || 'finding'}`
  } else if (b.kind === 'check') {
    const name = Array.from(b.summary.trim().replace(/\s+/g, '-'))
    subject = `check ${name.length > 20 ? name.slice(0, 19).join('') + '…' : name.join('')}`
  } else
    subject =
      b.kind === 'operator-note' ? 'operator note' : b.kind === 'conflict' ? 'conflict' : 'review'
  const suffix = input.blockers.length > 1 ? ` +${input.blockers.length - 1}` : ''
  const budget = 40 - Array.from(`repair: ${suffix}`).length
  const runes = Array.from(subject)
  return `repair: ${runes.length > budget ? runes.slice(0, budget - 1).join('') + '…' : subject}${suffix}`
}

/**
 * Name the repair chat and return the warning, if any. Never the session: a session rename
 * also rewrites the PR title.
 */
function renameRepairChat(title, env = process.env) {
  const chat = String(env.BOSS_AGENT_SESSION_ID ?? '').trim()
  if (!chat) return 'BOSS_AGENT_SESSION_ID is unset, so the repair chat keeps its name'
  const boss = resolveBossBinary(env)
  if (!boss.ok) return `repair chat rename failed: ${boss.reason}`
  const r = spawnSync(boss.path, ['chat', 'rename', chat, title], {
    encoding: 'utf8',
    timeout: 30000,
  })
  if (!r.error && r.status === 0) return null
  return `repair chat rename failed: ${r.error?.message ?? (String(r.stderr ?? '').trim() || `exit ${r.status}`)}`
}

/** Validate the publication against real remote-tracking state and Git ancestry. */
export function validateOutcomes(blockers, input, git = runGit) {
  const errors = [...validateBlockerFile(blockers).errors, ...validateOutcomeFile(input).errors]
  if (errors.length) return { ok: false, errors }
  if (input.blockersHead !== blockers.head) errors.push('blockersHead does not match blocker file')
  const expected = new Set(blockers.blockers.map((b) => b.id))
  for (const o of input.outcomes) {
    if (!expected.delete(o.id)) errors.push(`${o.id}: unknown id`)
    if (
      o.outcome === 'fixed' &&
      !git(['merge-base', '--is-ancestor', o.commit, input.pushedHead]).ok
    )
      errors.push(`${o.id}: commit is not an ancestor of pushedHead`)
  }
  for (const id of expected) errors.push(`${id}: missing outcome`)
  const branch = git(['branch', '--show-current'])
  const remote =
    branch.ok && line(branch.output)
      ? git(['rev-parse', `refs/remotes/origin/${branch.output}`])
      : null
  if (!remote?.ok || remote.output !== input.pushedHead)
    errors.push('pushedHead does not equal origin/<branch>')
  return { ok: errors.length === 0, errors }
}

export function outcomesComment(input) {
  const { errors } = validateOutcomeFile(input)
  if (errors.length) throw new Error(errors.join('; '))
  return (
    [
      '### Repair blocker outcomes',
      '',
      ...input.outcomes.map(
        (o) =>
          `- ${o.id}: ${o.outcome === 'fixed' ? `fixed ${o.commit.slice(0, 12)}` : `not fixable: ${o.reason}`}`,
      ),
      '',
      encodeOutcomesTrailer(input),
    ].join('\n') + '\n'
  )
}

export function carryFindings(
  findings,
  outcomes = [],
  isAncestor = (_commit, _descendant = undefined) => false,
  currentOutcomes = [],
) {
  const fixes = [...outcomes, ...currentOutcomes].filter(
    (o) => o.outcome === 'fixed' && isAncestor(o.commit),
  )
  return findings.filter(
    (f) =>
      !buildBlockers({ reviewFindings: [{ ...f, verified: true, fixed: false }] }).some((b) =>
        fixes.some(
          (o) =>
            o.id === b.id &&
            (currentOutcomes.includes(o) ||
              !f.assertionHead ||
              (o.blockersHead && isAncestor(f.assertionHead, o.blockersHead))),
        ),
      ),
  )
}

export async function main(argv, { read = commandReader(), git = runGit } = {}) {
  const [command, ...args] = argv
  if (!['brief', 'rename', 'outcomes', 'carry'].includes(command))
    throw new TypeError('expected brief, rename, outcomes or carry')
  const allowed =
    command === 'carry'
      ? ['pr', 'repo', 'outcomes', 'out']
      : command === 'outcomes'
        ? ['blockers', 'outcomes']
        : ['blockers']
  const { values } = parseArgs({
    args,
    strict: true,
    allowPositionals: false,
    options: Object.fromEntries(allowed.map((k) => [k, { type: 'string' }])),
  })
  const required =
    command === 'carry'
      ? ['pr', 'out']
      : command === 'outcomes'
        ? ['blockers', 'outcomes']
        : ['blockers']
  for (const key of required) if (!line(values[key])) throw new TypeError(`missing --${key}`)
  if (command === 'carry') {
    const pr = Number(values.pr)
    if (!Number.isSafeInteger(pr) || pr < 1) throw new TypeError('invalid --pr')
    const repoRead = values.repo
      ? null
      : read('gh', ['repo', 'view', '--json', 'nameWithOwner', '-q', '.nameWithOwner'])
    const repo = values.repo ?? (repoRead?.ok ? repoRead.output : null)
    if (!repo) throw new Error('cannot read repository')
    const result = readOperatorMarkers(read, repo, pr)
    if (!result.readable) throw new Error('cannot read standing review report')
    const standing = (result.markers ?? [])
      .filter((m) => m.kind === 'review-findings')
      .sort((a, b) => b.at - a.at || b.id - a.id)[0]
    const outcomes = (result.markers ?? [])
      .filter((m) => m.kind === 'repair-outcomes')
      .flatMap((m) => m.outcomes.map((o) => ({ ...o, blockersHead: m.blockersHead })))
    let local
    if (values.outcomes) {
      local = jsonFile(values.outcomes)
      const { errors } = validateOutcomeFile(local)
      if (errors.length) throw new Error(errors.join('; '))
    }
    const head = git(['rev-parse', 'HEAD'])
    if (!head.ok || !line(head.output)) throw new Error('cannot read HEAD')
    const isAncestor = (commit, descendant = head.output) =>
      git(['merge-base', '--is-ancestor', commit, descendant]).ok
    // Explicit outcomes describe this repair, even when an old assertion was rewritten away.
    // Historical posted outcomes still need generation ancestry; a stale local file is no authority.
    if (local && local.pushedHead !== head.output)
      throw new Error('local outcomes pushedHead does not equal HEAD')
    if (local && !isAncestor(local.blockersHead))
      throw new Error('local outcomes blockersHead is not an ancestor of HEAD')
    writeFileSync(
      values.out,
      JSON.stringify(
        carryFindings(
          (standing?.findings ?? []).map((f) => ({
            ...f,
            assertionHead: f.assertionHead ?? standing.head,
          })),
          outcomes,
          isAncestor,
          local?.outcomes ?? [],
        ),
      ) + '\n',
    )
    return JSON.stringify({ out: values.out }) + '\n'
  }
  const blockers = checkedBlockers(jsonFile(values.blockers))
  if (command === 'brief') return blockerBrief(blockers)
  if (command === 'rename') {
    const title = blockerTitle(blockers)
    const warning = renameRepairChat(title)
    if (warning) process.stderr.write(`warning: ${warning}\n`)
    return title + '\n'
  }
  const outcomes = jsonFile(values.outcomes)
  const result = validateOutcomes(blockers, outcomes)
  if (!result.ok) throw new Error(result.errors.join('; '))
  return outcomesComment(outcomes)
}

if (isMainModule(import.meta.url)) {
  main(process.argv.slice(2))
    .then((body) => process.stdout.write(body))
    .catch((error) => {
      process.stderr.write(`${error.message}\n`)
      process.exitCode = error instanceof TypeError ? 2 : 1
    })
}
