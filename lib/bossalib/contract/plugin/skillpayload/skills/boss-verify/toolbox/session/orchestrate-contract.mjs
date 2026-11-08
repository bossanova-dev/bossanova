// The orchestrator's extension roles, spread into the single role table in skill-extensions.mjs.
// `orchestrate-rules` is prose the orchestrator applies every round. `orchestrate-classify` runs a
// script over the round's snapshot rows and returns overrides for some of them; `orchestrate-round`
// runs a script once per round and returns items for the operator list. Both scripts are named by
// the marker's `script` key. The snapshot helper (orchestrate-snapshot.mjs) runs them.

const nonEmptyString = (value) => typeof value === 'string' && value.trim() !== ''
const singleLine = (value) => nonEmptyString(value) && !/[\r\n]/.test(value)
const plainObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

// Each override names one snapshot row by `host` and `session` and replaces its `action` and `why`
// (and optionally `next`, the command that takes the action).
const OVERRIDE_KEYS = new Set(['host', 'session', 'action', 'why', 'next'])
const KEBAB_ACTION = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/

function refineClassify(result) {
  if (!Array.isArray(result.overrides)) return ['overrides must be an array']
  const errors = []
  result.overrides.forEach((override, idx) => {
    if (!plainObject(override)) {
      errors.push(`override ${idx} is not an object`)
      return
    }
    if (Object.keys(override).some((key) => !OVERRIDE_KEYS.has(key)))
      errors.push(`override ${idx} has an unknown key`)
    for (const key of ['host', 'session', 'why'])
      if (!singleLine(override[key]))
        errors.push(`override ${idx} "${key}" must be a non-empty single-line string`)
    if (typeof override.action !== 'string' || !KEBAB_ACTION.test(override.action))
      errors.push(`override ${idx} "action" must be a kebab-case name`)
    if ('next' in override && typeof override.next !== 'string')
      errors.push(`override ${idx} "next" must be a string`)
  })
  return errors
}

export const ORCHESTRATE_ROLES = {
  'orchestrate-rules': { kind: 'instructions', keys: [] },
  'orchestrate-classify': { kind: 'fields', keys: ['overrides'], refine: refineClassify },
  'orchestrate-round': { kind: 'items', keys: ['title', 'detail'], nonEmpty: true },
}

// The roles whose marker names a `script` the core runs directly, with no agent dispatch.
export const SCRIPT_ROLES = new Set(['orchestrate-classify', 'orchestrate-round'])

export const INVALID_SCRIPT_REASON =
  'invalid "script" (expected a path inside the extension directory with no ".." segment)'

/**
 * `{ script }` from a script role's marker: a relative path with no `..` segment, normalised to
 * forward slashes without a leading `./`. Null when it is missing, empty or absolute.
 */
export function parseScriptMarker(marker) {
  if (typeof marker?.script !== 'string') return null
  const raw = marker.script.trim().replace(/\\/g, '/')
  if (!raw || raw.startsWith('/') || /^[A-Za-z]:/.test(raw)) return null
  const segments = raw.split('/').filter((segment) => segment && segment !== '.')
  if (!segments.length || segments.includes('..')) return null
  return { script: segments.join('/') }
}
