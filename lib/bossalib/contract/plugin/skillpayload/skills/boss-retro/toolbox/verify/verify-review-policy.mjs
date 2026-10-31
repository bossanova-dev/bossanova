/**
 * @typedef {{name: string, globs: string[], parkCode: string, label?: string}} ReviewArea
 * @typedef {{alwaysReview: ReviewArea[], neverReview: string[], deprecations: string[]}} ReviewPolicy
 */

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
/** @param {unknown} value @returns {value is string} */
const glob = (value) => typeof value === 'string' && value.length > 0
/** @param {unknown} value @returns {value is string} */
const label = (value) =>
  typeof value === 'string' &&
  value.trim().length > 0 &&
  !/[\r\n]/.test(value) &&
  Array.from(value).length <= 60
/** @param {unknown} value @returns {string[]} */
const globs = (value) => (Array.isArray(value) ? value.filter(glob) : [])

/** Normalize new and legacy review keys without mutating or trusting unvalidated input.
 * @param {unknown} block @returns {ReviewPolicy}
 */
export function reviewPolicyFromBlock(block) {
  /** @type {ReviewPolicy} */
  const policy = { alwaysReview: [], neverReview: [], deprecations: [] }
  if (!object(block)) return policy
  if (object(block.alwaysReview)) {
    for (const [name, entries] of Object.entries(block.alwaysReview)) {
      if (label(name) && Array.isArray(entries))
        policy.alwaysReview.push({
          name,
          globs: globs(entries),
          parkCode: 'always-human-path',
          label: name,
        })
    }
  }
  policy.neverReview = globs(block.neverReview)
  /** @param {unknown} entries @param {string} name @param {string} parkCode */
  const legacy = (entries, name, parkCode) => {
    /** @type {Map<string | undefined, ReviewArea>} */
    const groups = new Map()
    for (const entry of Array.isArray(entries) ? entries : []) {
      const pattern = typeof entry === 'string' ? entry : object(entry) ? entry.glob : null
      if (!glob(pattern)) continue
      if (
        object(entry) &&
        (Object.keys(entry).some((k) => k !== 'glob' && k !== 'label') ||
          (entry.label !== undefined && !label(entry.label)))
      )
        continue
      const caption = object(entry) && label(entry.label) ? entry.label.trim() : undefined
      let area = groups.get(caption)
      if (!area) {
        area = {
          name: caption ?? name,
          globs: [],
          parkCode,
          ...(caption === undefined ? {} : { label: caption }),
        }
        groups.set(caption, area)
        policy.alwaysReview.push(area)
      }
      area.globs.push(pattern)
    }
  }
  if (Object.hasOwn(block, 'alwaysHumanPaths')) {
    policy.deprecations.push(
      'verifyDefaults.alwaysHumanPaths is deprecated: move its globs to verifyDefaults.alwaysReview',
    )
    legacy(block.alwaysHumanPaths, 'Always review', 'always-human-path')
  }
  if (Object.hasOwn(block, 'humanReview')) {
    policy.deprecations.push(
      'verifyDefaults.humanReview is deprecated: move each category to a named verifyDefaults.alwaysReview area',
    )
    for (const [category, name] of [
      ['database', 'Database'],
      ['security', 'Security'],
      ['ui', 'UI'],
    ])
      legacy(
        object(block.humanReview) ? block.humanReview[category] : null,
        name,
        `human-review:${category}`,
      )
  }
  return policy
}
