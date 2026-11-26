// Which paths a repo declares generated, read from one commit's .boss-skills.json.
//
// `verifyDefaults.generatedPaths` names files a rebase may regenerate without anyone re-reading
// them, and `verifyDefaults.generatedChecks` names the CI checks that prove them regenerated from
// source. Globs with no check named prove nothing, so they match nothing here, as in verify's
// approval carry. The config comes from a commit (the base the head sits on), never the working
// tree, so a branch cannot widen the set for its own rebase.

import { CONFIG_FILENAME, globToRegExp } from './skill-config.mjs'

const strings = (value) =>
  Array.isArray(value) ? value.filter((item) => typeof item === 'string' && item.trim()) : []

/**
 * The generated-path matchers `rev` declares, or [] when it declares none, names no proving check,
 * or its config cannot be read. `read(command, args)` answers `{ok, output}`.
 * @returns {RegExp[]}
 */
export function generatedPathMatchers(read, rev) {
  const shown = read('git', ['show', `${rev}:${CONFIG_FILENAME}`])
  if (!shown.ok || typeof shown.output !== 'string') return []
  let config
  try {
    config = JSON.parse(shown.output)
  } catch {
    return []
  }
  const defaults = config?.verifyDefaults
  if (strings(defaults?.generatedChecks).length === 0) return []
  return strings(defaults?.generatedPaths).map(globToRegExp)
}

/** @param {RegExp[]} matchers @param {string} path */
export const isGenerated = (matchers, path) => matchers.some((re) => re.test(path))
