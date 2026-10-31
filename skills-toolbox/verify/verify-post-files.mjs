// Materializes the judge's verdict inputs. Blockers are required; notes are cosmetic.
import { join } from 'node:path'
import { validateBlockerFile } from './verify-blockers.mjs'
import { writeAtomicJson } from './verify-admission.mjs'

export function verdictFileArgs({ fs, logDir, pr, head, judged }) {
  const args = []
  if (judged.verdict === 'blocked') {
    const file = join(logDir, `boss-verify-blockers-${pr}-${head}.json`)
    const payload = { version: 1, pr: judged.pr ?? pr, head, blockers: judged.blockers }
    if (!validateBlockerFile(payload).ok || !writeAtomicJson(fs, file, payload)) return null
    args.push('--blockers', file)
  }
  if (Array.isArray(judged.findingNotes) && judged.findingNotes.length) {
    const file = join(logDir, `boss-verify-notes-${pr}-${head}.json`)
    if (writeAtomicJson(fs, file, judged.findingNotes)) args.push('--notes', file)
  }
  return args
}
