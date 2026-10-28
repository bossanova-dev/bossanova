const RELATION_FIELDS = ['subIssues', 'parent', 'blockedBy', 'blocking']

/**
 * Check schema support even on empty repositories, then repository-level REST availability.
 * @param {{api: Function, issue: Function, identity: {host: string, owner: string, repo: string, repository: string}}} ctx
 * @param {(status: string, remedy: string) => never} fail
 */
export function checkRelationsAvailable(ctx, fail) {
  /** @param {string} location */
  const unavailable = (location) =>
    fail(
      'relations-unavailable',
      `sub-issues and issue dependencies are not available on ${location}; the github tracker needs them for epics and blocked-by scheduling`,
    )
  /** @param {Function} action */
  const read = (action) => {
    try {
      return action()
    } catch (error) {
      fail(
        'repository-unavailable',
        `check access to ${ctx.identity.repository} with gh repo view; ${String(error)}`,
      )
    }
  }
  const schema = read(() =>
    ctx.api('graphql', ['-f', 'query={__type(name: "Issue") { fields { name } }}']),
  )
  if (schema?.errors?.length)
    fail('repository-unavailable', `cannot read Issue schema; ${JSON.stringify(schema.errors)}`)
  const fields = schema?.data?.__type?.fields
  if (
    !Array.isArray(fields) ||
    RELATION_FIELDS.some((name) => !fields.some((f) => f?.name === name))
  )
    unavailable(ctx.identity.host)

  // gh issue list excludes pull requests, unlike the REST repository issues endpoint.
  const issues = read(() =>
    ctx.issue('list', ['--state', 'all', '--limit', '1', '--json', 'number']),
  )
  if (!Array.isArray(issues) || issues.length > 1)
    fail('repository-unavailable', 'cannot read newest issue for relation availability check')
  if (!issues.length) return
  const number = issues[0]?.number
  if (!Number.isSafeInteger(number) || number < 1)
    fail(
      'repository-unavailable',
      'cannot read newest issue number for relation availability check',
    )
  const base = `repos/${ctx.identity.owner}/${ctx.identity.repo}/issues/${number}`
  for (const endpoint of ['sub_issues', 'dependencies/blocked_by']) {
    try {
      ctx.api(`${base}/${endpoint}`)
    } catch (error) {
      if (/\bHTTP\s+(404|410)\b/i.test(String(error))) unavailable(ctx.identity.repository)
      fail(
        'repository-unavailable',
        `check access to ${ctx.identity.repository} with gh repo view; ${String(error)}`,
      )
    }
  }
}
