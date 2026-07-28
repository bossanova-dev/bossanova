// Shared, dependency-free helpers for tracker-hosted implementation plans.
import { readFileSync } from 'node:fs'

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

/**
 * Select a canonical plan attachment. Exact title wins; the Markdown fallback
 * keeps older tracker payloads usable while never treating arbitrary files as plans.
 */
export function selectImplementationPlanAttachment(attachments, issueID) {
  const list = Array.isArray(attachments) ? attachments.filter(Boolean) : []
  const title = `Implementation plan (${issueID})`
  const exact = list.filter((attachment) => attachment.title === title)
  if (exact.length > 0) return newest(exact)
  return newest(
    list.filter(
      (attachment) =>
        isMarkdown(attachment) &&
        typeof attachment.title === 'string' &&
        attachment.title.includes(issueID),
    ),
  )
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

if (import.meta.main) {
  const [, , command, file, uploadURL, headersFile] = process.argv
  if (command !== 'put' || !file || !uploadURL || !headersFile) {
    process.stderr.write('usage: plan-attachment.mjs put <file> <url> <headers-json-file>\n')
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
}
