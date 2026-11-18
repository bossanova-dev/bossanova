import { realpathSync } from 'node:fs'
import { isAbsolute, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

/** @typedef {{ helper: string, root: string | null, contained: boolean | null }} ToolboxProvenance */

/**
 * @param {string | URL} moduleUrl
 * @param {{ BOSS_PLAN_TOOLBOX?: string }} [env]
 * @returns {ToolboxProvenance}
 */
export function runToolboxProvenance(moduleUrl, env = process.env) {
  const helper = realpathSync(fileURLToPath(moduleUrl))
  if (!env.BOSS_PLAN_TOOLBOX) return { helper, root: null, contained: null }
  let root
  try {
    root = realpathSync(env.BOSS_PLAN_TOOLBOX)
  } catch {
    return { helper, root: env.BOSS_PLAN_TOOLBOX, contained: false }
  }
  const path = relative(root, helper)
  return { helper, root, contained: path !== '' && !isAbsolute(path) && !path.startsWith('..') }
}

/** @param {string} prefix @param {ToolboxProvenance} provenance */
export function formatHelperProvenance(prefix, { helper, root, contained }) {
  const suffix = root === null ? '' : ` (${contained ? '' : 'OUTSIDE '}run toolbox ${root})`
  return `${prefix} helper ${helper}${suffix}`
}
