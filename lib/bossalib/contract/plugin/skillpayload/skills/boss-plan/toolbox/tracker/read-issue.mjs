import { statSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { ISSUE_SNAPSHOT_FIELDS, issueSnapshotProblems } from './issue-snapshot.mjs'
import { writeFileAtomically } from './read-description.mjs'
export const READ_ISSUE_USAGE = `  read-issue --id <issueId> --out-file <path>
      Write a complete bare issue snapshot atomically; exits 2 on unreadable input, 64 on usage.
`
const fallback = `execute the tracker adapter's getIssue operation and save the bare payload with ${ISSUE_SNAPSHOT_FIELDS.map(({ field, rule }) => `${field} (${rule})`).join(', ')}`
export async function runReadIssue(rest, { write, errWrite, env, resolveAdapter }) {
  const fail = (message, code = 2) => {
    errWrite(`read-issue: ${String(message).replace(/[\r\n]+/g, ' ')}\n`)
    return code
  }
  let flags
  try {
    const parsed = parseArgs({
      args: rest,
      strict: true,
      tokens: true,
      options: { id: { type: 'string' }, 'out-file': { type: 'string' } },
    })
    const seen = new Set()
    for (const token of parsed.tokens) {
      if (token.kind !== 'option') continue
      if (seen.has(token.name) || !token.value?.trim())
        throw new Error(`invalid or repeated --${token.name}`)
      seen.add(token.name)
    }
    flags = parsed.values
    if (!flags.id || !flags['out-file']) throw new Error('--id and --out-file are required')
  } catch (error) {
    return fail(`usage: ${error.message}`, 64)
  }
  try {
    const target = path.resolve(flags['out-file'])
    if (!statSync(path.dirname(target)).isDirectory())
      throw new Error('--out-file parent must be a directory')
    const adapter = resolveAdapter({ env })
    if (typeof adapter?.readIssueSnapshot !== 'function')
      return fail(`adapter has no readIssueSnapshot capability; ${fallback}`)
    const snapshot = await adapter.readIssueSnapshot(flags.id)
    const problems = issueSnapshotProblems(snapshot)
    if (problems.length)
      return fail(`incomplete issue snapshot (missing: ${problems.join(', ')}); ${fallback}`)
    const bytes = writeFileAtomically(target, JSON.stringify(snapshot) + '\n')
    write(
      JSON.stringify({
        outcome: 'issue-snapshot-written',
        bytes,
        id: snapshot.id,
        identifier: snapshot.identifier,
        uuid: snapshot.uuid,
        state:
          snapshot.status ??
          snapshot.stateName ??
          (typeof snapshot.state === 'string' ? snapshot.state : snapshot.state?.name),
        attachments: snapshot.attachments.length,
      }) + '\n',
    )
    return 0
  } catch (error) {
    return fail(
      `${error.message}${error.code === TRACKER_CREDENTIALS_MISSING ? `; ${fallback}` : ''}`,
    )
  }
}
