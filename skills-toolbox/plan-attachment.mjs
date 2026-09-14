// Shared, dependency-free helpers for tracker-hosted implementation plans.
import { createHash } from 'node:crypto'
import { readFileSync, renameSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { isMainModule } from './main-module.mjs'

// A usage error means NO request was sent. That is a different fact from a request the server
// rejected, and the two have opposite remedies: correct the call versus re-prepare and retry.
// Exit 2 alone cannot carry it — a caller that pipes stderr sees only text — so the text says it.
export const USAGE_ERROR_TOKEN = 'plan-attachment: usage-error (no request sent)'

// How much of a rejected upload's response body to quote. Signed-storage errors are small XML or
// JSON documents; the cap exists so a server that answers with a page cannot flood a run's log.
const MAX_ERROR_BODY_BYTES = 2048

function createdAt(attachment) {
  const value = Date.parse(attachment?.createdAt || '')
  return Number.isNaN(value) ? 0 : value
}

function newest(attachments) {
  return attachments.reduce((selected, attachment) => {
    if (!selected || createdAt(attachment) > createdAt(selected)) return attachment
    return selected
  }, null)
}

function isMarkdown(attachment) {
  const type = attachment?.contentType || attachment?.mimeType || ''
  const filename = attachment?.filename || attachment?.url || ''
  return type === 'text/markdown' || /\.md(?:$|[?#])/i.test(filename)
}

function planAttachmentTitle(issueID) {
  return `Implementation plan (${issueID})`
}

function matchesImplementationPlanAttachment(attachment, issueID, { mode }) {
  const title = planAttachmentTitle(issueID)
  if (mode === 'exact') return attachment?.title === title
  return (
    isMarkdown(attachment) &&
    typeof attachment?.title === 'string' &&
    attachment.title.includes(issueID)
  )
}

/**
 * Select a canonical plan attachment. Exact title wins; the Markdown fallback
 * keeps older tracker payloads usable while never treating arbitrary files as plans.
 */
export function selectImplementationPlanAttachment(attachments, issueID) {
  const list = Array.isArray(attachments) ? attachments.filter(Boolean) : []
  const exact = list.filter((attachment) =>
    matchesImplementationPlanAttachment(attachment, issueID, { mode: 'exact' }),
  )
  if (exact.length > 0) return newest(exact)
  return newest(
    list.filter((attachment) =>
      matchesImplementationPlanAttachment(attachment, issueID, { mode: 'permissive' }),
    ),
  )
}

/**
 * Select duplicate canonical implementation plan attachments that are safe to
 * delete after a newer attachment for the same issue has been read back.
 *
 * ORDERING, AND WHY IT CANNOT BE SILENT. The comparison below is `createdAt(candidate) <
 * keepCreatedAt`, and `createdAt` reports `0` for an attachment that carries no parsable
 * timestamp. The tracker's own issue read returns attachments as `{id, title, subtitle, url}` with
 * no `createdAt` at all, so every comparison became `0 < 0` and the function returned `[]` — a
 * stale duplicate survived while the run reported a clean supersede. An empty array is the same
 * value as "nothing is stale", which is why the miss was invisible for as long as it was.
 *
 * So an unorderable candidate set now THROWS. The escape is `keepJustFinalized`: the one real
 * caller finalized `keepAttachmentId` moments earlier in the same run, which is knowledge no
 * timestamp on the payload can supply, and declaring it supersedes the exact-title attachments
 * this call cannot order. It is NOT a licence to discard ordering the payload does carry. A
 * candidate that reports a parsable `createdAt` is still compared, and one NEWER than the kept
 * attachment — a concurrent run's plan, landed between this run's finalize and this call — is
 * never selected for deletion. Where the keep itself carries no timestamp, a timestamped candidate
 * has nothing to be compared against and is kept: the two errors are not symmetric, because a
 * surviving duplicate costs one follow-up sweep while a deleted concurrent plan is unrecoverable.
 * An undeclared call still fails loudly instead of doing nothing.
 *
 * @param {object[]} attachments the tracker's attachment list for the issue
 * @param {{issueID: string, keepAttachmentId: string, keepJustFinalized?: boolean}} options
 * @returns {string[]} attachment ids safe to delete
 */
export function selectSupersededPlanAttachments(
  attachments,
  { issueID, keepAttachmentId, keepJustFinalized = false },
) {
  const list = Array.isArray(attachments) ? attachments.filter(Boolean) : []
  const keep = list.find((attachment) => attachment.id === keepAttachmentId)
  if (!keep) return []
  const candidates = list.filter(
    (attachment) =>
      attachment.id !== keepAttachmentId &&
      matchesImplementationPlanAttachment(attachment, issueID, { mode: 'exact' }),
  )
  if (candidates.length === 0) return []
  if (keepJustFinalized) {
    const declaredKeepCreatedAt = createdAt(keep)
    return candidates
      .filter((attachment) => {
        const candidateCreatedAt = createdAt(attachment)
        // Unorderable candidate: the declaration is the only ordering information in existence.
        if (candidateCreatedAt === 0) return true
        // Orderable candidate, unorderable keep: nothing here proves this one is stale.
        if (declaredKeepCreatedAt === 0) return false
        // Both orderable: the same comparison the well-ordered path makes, declaration or not.
        return candidateCreatedAt < declaredKeepCreatedAt
      })
      .map((attachment) => attachment.id)
  }
  const keepCreatedAt = createdAt(keep)
  const unorderable = [keep, ...candidates].filter((attachment) => createdAt(attachment) === 0)
  if (unorderable.length > 0) {
    throw new Error(
      `plan-attachment: unorderable supersede candidates for ${issueID}: ` +
        `${unorderable.length} exact-title attachment(s) carry no parsable createdAt ` +
        `(${unorderable.map((attachment) => attachment.id).join(', ')}). ` +
        'Pass `keepJustFinalized: true` when this run just finalized keepAttachmentId, or supply ' +
        'an attachment listing that carries timestamps. Returning an empty set here would read as ' +
        '"nothing is stale".',
    )
  }
  return candidates
    .filter((attachment) => createdAt(attachment) < keepCreatedAt)
    .map((attachment) => attachment.id)
}

/**
 * Truncate to at most MAX_ERROR_BODY_BYTES *bytes*, never UTF-16 code units.
 *
 * `String.length` and `String.slice` count code units, so a cap applied through them is a
 * CHARACTER cap: a multi-byte body passes a 2048 "byte" test at up to 8192 bytes. That is the same
 * byte-versus-character confusion this module's upload contract exists to prevent, so the quoting
 * path must not commit it. A slice can land mid-character, and decoding that non-fatally yields a
 * trailing U+FFFD, which is dropped rather than quoted back as if the server had sent it.
 */
function truncateToBytes(text) {
  const bytes = Buffer.from(text, 'utf8')
  if (bytes.length <= MAX_ERROR_BODY_BYTES) return text
  const decoded = new TextDecoder('utf-8').decode(bytes.subarray(0, MAX_ERROR_BODY_BYTES))
  return `${decoded.replace(/\uFFFD+$/, '')}… (truncated)`
}

/**
 * Read at most the cap from a streaming body, then STOP the transfer.
 *
 * Returns null when the response exposes no readable stream, which is the signal to fall back to
 * `.text()`. Reading the stream is what keeps the bound a real one: `.text()` buffers the whole
 * document first, so a server answering with a page is already in memory by the time any cap is
 * applied to it.
 */
async function readBoundedStream(body) {
  if (!body || typeof body.getReader !== 'function') return null
  const reader = body.getReader()
  const chunks = []
  let total = 0
  try {
    while (total <= MAX_ERROR_BODY_BYTES) {
      const { done, value } = await reader.read()
      if (done) break
      if (!value) continue
      const chunk = Buffer.from(value)
      chunks.push(chunk)
      total += chunk.length
    }
  } finally {
    // Bounded by the cap plus at most one chunk, and the rest is never transferred.
    try {
      await reader.cancel()
    } catch {
      /* a body that cannot be cancelled still must not replace the upload failure */
    }
  }
  return Buffer.concat(chunks)
}

/**
 * Read a rejected response's body as text, bounded, never throwing.
 *
 * A signed-storage rejection carries its reason in the BODY, not the status: an expired URL and a
 * payload/header mismatch both answer 403, and their remedies are opposite (re-prepare versus fix
 * the declared size or headers). A body-less or unreadable response must still produce the status
 * error, so every failure mode here degrades to an empty string rather than replacing the upload
 * failure with a failure to read it.
 */
async function readErrorBody(response) {
  try {
    const streamed = await readBoundedStream(response?.body)
    if (streamed !== null) return truncateToBytes(streamed.toString('utf8').trim())
    if (typeof response?.text !== 'function') return ''
    return truncateToBytes(String((await response.text()) ?? '').trim())
  } catch {
    return ''
  }
}

/** Put one raw plan file to a tracker-provided signed URL. */
export async function putPlanAttachment({ file, uploadURL, headers, fetchImpl = fetch }) {
  const response = await fetchImpl(uploadURL, {
    method: 'PUT',
    headers,
    body: readFileSync(file),
  })
  const status = Number(response?.status)
  if (!Number.isInteger(status) || status < 200 || status >= 300) {
    const body = await readErrorBody(response)
    throw new Error(
      `signed attachment upload returned ${Number.isInteger(status) ? status : 'unknown'}` +
        (body ? `: ${body}` : ''),
    )
  }
  return status
}

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex')
}

/**
 * Compare the bytes stored behind a signed attachment URL against the local plan file.
 *
 * This is the read-back check, and it is a DIGEST comparison rather than a "non-empty content"
 * assertion on purpose. Both observed read-back failures scored as healthy under a presence test:
 * an unsigned GraphQL attachment URL answers `{"error":"unauthorized"}`, which is small but
 * present, and an off-by-one upload stores bytes that are neither empty nor the plan. A digest
 * decides both, and it decides them without the stored body entering an agent's context.
 *
 * @param {{file: string, url: string, fetchImpl?: typeof fetch}} options
 * @returns {Promise<{match: boolean, local: {sha256: string, bytes: number},
 *   fetched: {sha256: string, bytes: number}}>}
 */
export async function verifyPlanAttachment({ file, url, fetchImpl = fetch }) {
  const localBytes = readFileSync(file)
  const response = await fetchImpl(url, { method: 'GET' })
  const status = Number(response?.status)
  if (!Number.isInteger(status) || status < 200 || status >= 300) {
    const body = await readErrorBody(response)
    throw new Error(
      `plan-attachment: attachment read-back fetch returned ` +
        `${Number.isInteger(status) ? status : 'unknown'}${body ? `: ${body}` : ''}`,
    )
  }
  if (typeof response?.arrayBuffer !== 'function') {
    throw new Error('plan-attachment: attachment read-back response carried no readable body')
  }
  const fetchedBytes = Buffer.from(await response.arrayBuffer())
  const local = { sha256: sha256(localBytes), bytes: localBytes.length }
  const fetched = { sha256: sha256(fetchedBytes), bytes: fetchedBytes.length }
  return { match: local.sha256 === fetched.sha256, local, fetched }
}

function asUtf8String(body) {
  if (Buffer.isBuffer(body)) return body.toString('utf8')
  return String(body ?? '')
}

/**
 * Decode a tracker-returned epic spec attachment body into plain JSON text.
 *
 * Some tracker attachment reads return the JSON file body base64-encoded, while newer paths may
 * hand back the plain JSON bytes directly. Accept both and validate by feeding the resulting text to
 * JSON.parse so an invalid/transcribed body fails as a named attachment error instead of being
 * written as corrupted spec input for parseEpicSpec().
 */
export function decodeSpecAttachmentBody(body) {
  const raw = asUtf8String(body)
  const trimmed = raw.trim()
  if (!trimmed) throw new Error('plan-attachment: empty spec attachment body')
  if (trimmed.startsWith('{') || trimmed.startsWith('[')) {
    try {
      JSON.parse(raw)
      return raw
    } catch (error) {
      throw new Error(`plan-attachment: invalid plain JSON spec attachment body: ${error.message}`)
    }
  }
  let decoded
  const compact = trimmed.replace(/\s+/g, '')
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(compact) || compact.length % 4 === 1) {
    throw new Error('plan-attachment: invalid base64 spec attachment body')
  }
  try {
    decoded = Buffer.from(compact, 'base64').toString('utf8')
  } catch (error) {
    throw new Error(`plan-attachment: invalid base64 spec attachment body: ${error.message}`)
  }
  const encoded = Buffer.from(decoded, 'utf8').toString('base64')
  if (compact.replace(/=+$/, '') !== encoded.replace(/=+$/, '')) {
    throw new Error('plan-attachment: invalid base64 spec attachment body')
  }
  try {
    JSON.parse(decoded)
  } catch (error) {
    throw new Error(
      `plan-attachment: spec attachment body is neither plain JSON nor base64 JSON: ${error.message}`,
    )
  }
  return decoded
}

function writeFileAtomic(file, data) {
  const temporary = join(dirname(file), `.${process.pid}.${Date.now()}.tmp`)
  writeFileSync(temporary, data)
  renameSync(temporary, file)
}

// Detect direct CLI entry with isMainModule(), never the runtime's own entry-point flag: that
// flag reads `undefined` on runtimes older than the 22.x backport, which made this whole block
// dead code — the CLI exited 0 having uploaded nothing. isMainModule() compares process.argv[1]
// against this module's path instead, so entry detection is runtime-independent.
if (isMainModule(import.meta.url)) {
  const [, , command, ...args] = process.argv
  const usage =
    'usage: plan-attachment.mjs put <file> <url> <headers-json-file>\n' +
    '       plan-attachment.mjs verify <local-file> <signed-url>\n' +
    '       plan-attachment.mjs decode <in-file> <out-file>\n'
  // Every usage exit prints the token FIRST, so `head -1` separates "no request was sent" from a
  // rejected one without parsing the rest. The retry branch documented in plan-storage.md is
  // scoped to the latter; re-preparing a signed URL cannot fix a call that never made a request.
  const usageExit = () => {
    process.stderr.write(`${USAGE_ERROR_TOKEN}\n${usage}`)
    process.exitCode = 2
  }
  if (command === 'put') {
    const [file, uploadURL, headersFile] = args
    if (!file || !uploadURL || !headersFile) {
      usageExit()
    } else {
      const headers = JSON.parse(readFileSync(headersFile, 'utf8'))
      putPlanAttachment({ file, uploadURL, headers })
        .then((status) => process.stdout.write(`${status}\n`))
        .catch((error) => {
          process.stderr.write(`${error.message}\n`)
          process.exitCode = 1
        })
    }
  } else if (command === 'verify') {
    const [file, url] = args
    if (!file || !url) {
      usageExit()
    } else {
      verifyPlanAttachment({ file, url })
        .then(({ match, local, fetched }) => {
          if (match) {
            process.stdout.write(`verify: match sha256=${local.sha256} bytes=${local.bytes}\n`)
            return
          }
          process.stderr.write(
            `plan-attachment: verify MISMATCH - local sha256=${local.sha256} bytes=${local.bytes}` +
              ` differs from fetched sha256=${fetched.sha256} bytes=${fetched.bytes}\n`,
          )
          process.exitCode = 1
        })
        .catch((error) => {
          process.stderr.write(`${error.message}\n`)
          process.exitCode = 1
        })
    }
  } else if (command === 'decode') {
    const [inFile, outFile] = args
    if (!inFile || !outFile) {
      usageExit()
    } else {
      try {
        writeFileAtomic(outFile, decodeSpecAttachmentBody(readFileSync(inFile, 'utf8')))
      } catch (error) {
        process.stderr.write(`${error.message}\n`)
        process.exitCode = 1
      }
    }
  } else {
    usageExit()
  }
}
