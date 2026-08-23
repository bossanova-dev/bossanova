// Shared, dependency-free helpers for tracker-hosted implementation plans.
import { readFileSync, renameSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { isMainModule } from './main-module.mjs'

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
 */
export function selectSupersededPlanAttachments(attachments, { issueID, keepAttachmentId }) {
  const list = Array.isArray(attachments) ? attachments.filter(Boolean) : []
  const keep = list.find((attachment) => attachment.id === keepAttachmentId)
  if (!keep) return []
  const keepCreatedAt = createdAt(keep)
  return list
    .filter(
      (attachment) =>
        attachment.id !== keepAttachmentId &&
        matchesImplementationPlanAttachment(attachment, issueID, { mode: 'exact' }) &&
        createdAt(attachment) < keepCreatedAt,
    )
    .map((attachment) => attachment.id)
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
    throw new Error(
      `signed attachment upload returned ${Number.isInteger(status) ? status : 'unknown'}`,
    )
  }
  return status
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
    '       plan-attachment.mjs decode <in-file> <out-file>\n'
  if (command === 'put') {
    const [file, uploadURL, headersFile] = args
    if (!file || !uploadURL || !headersFile) {
      process.stderr.write(usage)
      process.exitCode = 2
    } else {
      const headers = JSON.parse(readFileSync(headersFile, 'utf8'))
      putPlanAttachment({ file, uploadURL, headers })
        .then((status) => process.stdout.write(`${status}\n`))
        .catch((error) => {
          process.stderr.write(`${error.message}\n`)
          process.exitCode = 1
        })
    }
  } else if (command === 'decode') {
    const [inFile, outFile] = args
    if (!inFile || !outFile) {
      process.stderr.write(usage)
      process.exitCode = 2
    } else {
      try {
        writeFileAtomic(outFile, decodeSpecAttachmentBody(readFileSync(inFile, 'utf8')))
      } catch (error) {
        process.stderr.write(`${error.message}\n`)
        process.exitCode = 1
      }
    }
  } else {
    process.stderr.write(usage)
    process.exitCode = 2
  }
}
