import { readFileSync, statSync } from 'node:fs'
import { resolveOperation, emitOperation } from './operation-runner.mjs'
import { computeDescriptionPatch } from './description-patch.mjs'

const oneLine = (text) => String(text).replace(/[\r\n]+/g, ' ')

export function runWriteDescription(
  { id, 'body-file': bodyFile, 'base-file': baseFile },
  { write, errWrite, env, resolveAdapter },
) {
  if (!id) {
    errWrite('write-description: --id <issueId> is required\n')
    return 2
  }
  if (!bodyFile) {
    errWrite('write-description: --body-file <path> is required\n')
    return 2
  }
  let body
  let bytes
  try {
    body = readFileSync(bodyFile, 'utf8')
    // Measured with stat(2), not Buffer.byteLength over the decoded string: the
    // contract is "the bytes on disk", and the two disagree the moment a body is
    // re-encoded or carries a lone surrogate. A caller that re-derived this number
    // itself would be reporting its own belief about a file it never opened.
    bytes = statSync(bodyFile).size
  } catch (err) {
    errWrite(`write-description: could not read --body-file ${bodyFile}: ${err.message}\n`)
    return 2
  }
  // The most destructive input this verb can receive. update-comment refuses a blank
  // body because it would erase a comment's marker anchor; a blank DESCRIPTION erases
  // the only surviving copy of the reporter's original notes, because the tracker
  // exposes no description history to recover them from. Fail closed.
  if (body.trim() === '') {
    errWrite(
      `write-description: --body-file ${bodyFile} is empty; refusing to blank the description\n`,
    )
    return 2
  }
  // The other way the bytes on disk can stop being the bytes that reach the tracker.
  // readFileSync(..., 'utf8') does NOT throw on malformed input — it substitutes U+FFFD
  // — so without this check `description` would carry the corruption while `bytes` still
  // attests to the intact on-disk size and `outcome` still reads `descriptor-emitted`.
  // The write replaces the whole description and the tracker keeps no history, so that
  // corruption is unrecoverable. Re-encoding a cleanly decoded body always reproduces
  // the file, so a mismatch means the decode was lossy (or the file changed under us
  // between read and stat). Fail closed either way.
  const decodedBytes = Buffer.byteLength(body, 'utf8')
  if (decodedBytes !== bytes) {
    errWrite(
      `write-description: --body-file ${bodyFile} is not valid UTF-8 (${bytes} bytes on disk, ` +
        `${decodedBytes} after decoding); refusing to write a corrupted description\n`,
    )
    return 2
  }
  let base
  if (baseFile !== undefined) {
    try {
      base = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(
        readFileSync(baseFile),
      )
    } catch (error) {
      errWrite(
        `write-description: could not read valid UTF-8 --base-file ${baseFile}: ${oneLine(error.message)}\n`,
      )
      return 2
    }
  }
  const adapter = resolveAdapter({ env })
  let patchFallback
  if (base !== undefined) {
    const patchOp = resolveOperation(adapter.operationMap, 'patchDescription')
    if (patchOp) {
      const result = computeDescriptionPatch(base, body)
      if (result.ops) {
        try {
          emitOperation(
            patchOp,
            { id, patch: result.ops },
            { write },
            {
              bytes,
              patchBytes: Buffer.byteLength(JSON.stringify(result.ops)),
              mode: 'patch',
            },
          )
        } catch (error) {
          errWrite(`write-description: ${oneLine(error.message)}\n`)
          return 2
        }
        return 0
      }
      patchFallback = result.reason
    } else patchFallback = 'no-patchDescription-op'
  }
  const op = resolveOperation(adapter.operationMap, 'writeDescription')
  if (!op) {
    errWrite(
      'write-description: resolved tracker adapter has no writeDescription operation or has no tool or command; send the description inline instead\n',
    )
    return 2
  }
  try {
    emitOperation(
      op,
      { id, description: body },
      { write },
      { bytes, mode: 'replace', ...(patchFallback ? { patchFallback } : {}) },
    )
  } catch (error) {
    errWrite(`write-description: ${oneLine(error.message)}\n`)
    return 2
  }
  return 0
}
