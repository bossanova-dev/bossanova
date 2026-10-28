export function assertCommentBody(body) {
  if (typeof body !== 'string' || body.length > 65_536)
    throw new Error('github tracker: comment body must be a string of at most 65536 characters')
}
