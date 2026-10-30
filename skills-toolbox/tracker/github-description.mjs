import { createHash } from 'node:crypto'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { assertCommentBody } from './github-comment.mjs'
import { indeterminateWriteError } from './outcome.mjs'

const digest = (text) => createHash('sha256').update(text).digest('hex')

/** Replace an issue body and attest only to the description read back from GitHub. */
export function writeGithubDescription(args, ctx) {
  assertCommentBody(args.description)
  if (!args.description.trim()) throw new Error('github tracker: empty description not written')
  const number = ctx.relations.localNumber(args.id)
  const path = `repos/${ctx.identity.owner}/${ctx.identity.repo}/issues/${number}`
  const temp = mkdtempSync(join(tmpdir(), 'github-description-write-'))
  try {
    const file = join(temp, 'body.json')
    writeFileSync(file, JSON.stringify({ body: args.description }), { mode: 0o600 })
    try {
      ctx.api(path, ['--method', 'PATCH', '--input', file])
      const readback = ctx.api(path)
      if (typeof readback?.body !== 'string' || digest(readback.body) !== digest(args.description))
        throw new Error('description read-back disagrees')
    } catch (error) {
      throw indeterminateWriteError('github tracker: write-description may have applied', error)
    }
  } finally {
    rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
  }
  return {
    id: `GH-${number}`,
    bytes: Buffer.byteLength(args.description),
    sha256: digest(args.description),
  }
}
