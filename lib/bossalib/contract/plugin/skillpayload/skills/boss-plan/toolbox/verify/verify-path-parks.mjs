import { reviewAreas } from './verify-areas.mjs'
import { levelConfidence, legacyCodes } from './verify-level.mjs'
import { CONFIG_FILENAME, extensionRootsFor } from '../skill-config.mjs'

const text = (value) => typeof value === 'string' && value.trim().length > 0

// A policy a PR could weaken: review globs, or `verifyDefaults.merge` rules that could otherwise
// cover an edit to the policy itself. The schema rejects a malformed rule before this reads it.
const policyNonEmpty = (p) =>
  p.review.alwaysReview.some((area) => area.globs.length > 0) ||
  (Array.isArray(p.merge?.auto) && p.merge.auto.length > 0)

const normalizeRoot = (root) => String(root).replace(/^\.\//, '').replace(/\/+$/, '')

/**
 * The changed paths that touch the verify policy surface: the config file at the repo root, any
 * path under an extension root whose directory name starts `boss-verify-` (matched by name, so a
 * deleted extension that discovery can no longer see still counts), and any declared applicability
 * script (`appliesScripts`, repo-relative), so a PR cannot skip a judge by editing its check.
 */
export function policySurfaceHits(
  paths,
  { extensionRoots = extensionRootsFor(null), appliesScripts = [] } = {},
) {
  const prefixes = extensionRoots.map((root) => `${normalizeRoot(root)}/boss-verify-`)
  const scripts = new Set(appliesScripts.filter(text).map(normalizeRoot))
  return (Array.isArray(paths) ? paths : []).filter(
    (path) =>
      path === CONFIG_FILENAME ||
      scripts.has(normalizeRoot(path)) ||
      prefixes.some((prefix) => path.startsWith(prefix) && path.indexOf('/', prefix.length) > 0),
  )
}

export function levelAreasFor(files, policy, options = {}) {
  return reviewAreas(
    files.map((file) =>
      typeof file === 'string'
        ? file
        : {
            path: file.filename,
            previousPath: file.previous_filename,
            status: file.status,
            changes: file.changes,
            patch: file.patch,
          },
    ),
    policy.review,
    options,
  )
}

export function pathParkFacts(files, policy, options = {}) {
  const paths = files.flatMap((file) =>
    typeof file === 'string' ? [file] : [file.filename, file.previous_filename].filter(Boolean),
  )
  return {
    areas: levelAreasFor(files, policy, options).areas,
    policySurfaceHit: policyNonEmpty(policy) && policySurfaceHits(paths, options).length > 0,
  }
}

/** Project only path-driven asks: review, collectors and size are neutral, not predictions. */
export function predictPathParks(paths, policy, options = {}) {
  const facts = pathParkFacts(paths, policy, options)
  const result = levelConfidence({
    ...facts,
    changedPaths: paths,
    changedLines: 0,
    sizeBaseline: { ok: true, thresholds: null },
  })
  return {
    ...facts,
    codes: legacyCodes(result, policy.review),
    labels: [...new Set(result.asks.map((ask) => ask.label))],
  }
}
