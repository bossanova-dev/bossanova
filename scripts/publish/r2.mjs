// scripts/publish/r2.mjs
// Cloudflare R2 reference implementation of the publish adapter. Delegates to the
// existing plan-upload.mjs helpers (+ proof-lib.mjs r2UploadCommand via uploadPlanFile)
// for byte-identical R2 behaviour. The adapter owns key/URL derivation so a non-R2
// adapter can choose its own scheme (see the plan's Open Question 1).
import { generateToken, planObjectKey, uploadPlanFile } from '../plan-upload.mjs'

/** @returns {import('./adapter.mjs').PublishAdapter} */
export function createR2PublishAdapter({ env = process.env, uploadImpl = uploadPlanFile } = {}) {
  return {
    name: 'r2',
    async publish({ issueId, file, dryRun = false }) {
      const key = planObjectKey(issueId, generateToken())
      const url = uploadImpl({ file, key, env, dryRun })
      return { url, key }
    },
  }
}
