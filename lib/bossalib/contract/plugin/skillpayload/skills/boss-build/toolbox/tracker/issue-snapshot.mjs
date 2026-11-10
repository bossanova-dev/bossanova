const nonEmpty = (value) => typeof value === 'string' && value.trim().length > 0
const plainObject = (value) =>
  value !== null &&
  typeof value === 'object' &&
  [Object.prototype, null].includes(Object.getPrototypeOf(value))
export const ISSUE_SNAPSHOT_FIELDS = Object.freeze([
  Object.freeze({ field: 'identity', rule: 'non-empty id or identifier' }),
  Object.freeze({
    field: 'state',
    rule: 'non-empty status, stateName, state string or state.name',
  }),
  Object.freeze({ field: 'description', rule: 'own string or null description' }),
  Object.freeze({ field: 'attachments', rule: 'own complete array of attachment objects' }),
])
export function issueSnapshotProblems(value) {
  if (!plainObject(value)) return ISSUE_SNAPSHOT_FIELDS.map(({ field }) => field)
  const checks = [
    nonEmpty(value.id) || nonEmpty(value.identifier),
    nonEmpty(
      value.status ??
        value.stateName ??
        (typeof value.state === 'string' ? value.state : value.state?.name),
    ),
    Object.hasOwn(value, 'description') &&
      (value.description === null || typeof value.description === 'string'),
    Object.hasOwn(value, 'attachments') &&
      Array.isArray(value.attachments) &&
      value.attachments.every(plainObject),
  ]
  return ISSUE_SNAPSHOT_FIELDS.filter((_, index) => !checks[index]).map(({ field }) => field)
}
