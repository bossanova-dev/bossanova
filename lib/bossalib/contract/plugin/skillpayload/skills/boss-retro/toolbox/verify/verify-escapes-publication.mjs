/**
 * A create-only Git ref is the shared publication fence. It never expires: a
 * paused owner must not regain comment access after another owner takes over.
 * @param {any} record
 * @param {(cmd:string, args:string[]) => any} run
 */
export async function claimPublication(record, run) {
  const ref = `refs/boss-escape-claims/v1/${record.original.number}/${record.fix.number}`
  const endpoint = `repos/${record.repo}/git/refs`
  const sha = record.original.mergeHeadSha
  const valid = (response) => {
    try {
      const value = JSON.parse(response.stdout)
      return (
        response.status === 0 && !response.error && value.ref === ref && value.object?.sha === sha
      )
    } catch {
      return false
    }
  }
  let created
  try {
    created = await run('gh', [
      'api',
      '--method',
      'POST',
      endpoint,
      '-f',
      `ref=${ref}`,
      '-f',
      `sha=${sha}`,
    ])
  } catch {
    // An unknown outcome may already have created the ref. Never retry ownership.
  }
  if (created && valid(created)) return { ref, owned: true }
  try {
    const current = await run('gh', ['api', `${endpoint.slice(0, -1)}/${ref.slice(5)}`])
    if (valid(current)) return { ref, owned: false }
  } catch {
    // Unavailable or malformed reads cannot establish ownership.
  }
  throw Object.assign(new Error('cannot establish publication claim ownership'), {
    reason: 'claim-unavailable',
    ref,
  })
}
