// plan-slug — pure, dependency-free slug helper for boss-plan's plan-file path.
//
// A plan file is named `<ISSUE-ID>-<hyphenated-title>.md`. The id is upcased and
// the title is lowercased, with every run of non-alphanumeric characters folded
// to a single hyphen and leading/trailing hyphens trimmed. Keeping this pure and
// import-free lets it ship in the published skill toolbox and be shared by the
// repo-local publish path without pulling in any storage dependency.

export function issueSlug(id, title) {
  const titleSlug = String(title)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
  return `${String(id).toUpperCase()}-${titleSlug}`
}
