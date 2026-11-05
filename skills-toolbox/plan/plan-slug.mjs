// plan-slug — pure, dependency-free slug helper for boss-plan's plan-file path.
//
// A plan file is named `<ISSUE-ID>-<hyphenated-title>.md`. The id is upcased and
// the title is lowercased, with every run of non-alphanumeric characters folded
// to a single hyphen and leading/trailing hyphens trimmed. Keeping this pure and
// import-free lets it ship in the published skill toolbox and be shared by the
// repo-local publish path without pulling in any storage dependency.

/**
 * Return the whole plan-file stem `<ID>-<title-slug>`, already prefixed with the id.
 * @param {string} id Tracker issue identifier.
 * @param {string} title
 * @returns {string}
 */
export function issueSlug(id, title) {
  if (typeof id !== 'string' || !id.trim()) {
    const hint =
      id !== null && typeof id === 'object'
        ? '; pass id and title positionally rather than an issue object'
        : ''
    throw new TypeError(
      `issueSlug(id, title): id must be a non-blank string; received ${typeof id}${hint}`,
    )
  }
  if (typeof title !== 'string' || !title.trim()) {
    throw new TypeError(
      `issueSlug(id, title): title must be a non-blank string; received ${typeof title}`,
    )
  }
  const titleSlug = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
  return `${id.toUpperCase()}-${titleSlug}`
}
