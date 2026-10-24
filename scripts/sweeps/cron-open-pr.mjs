#!/usr/bin/env node
// Shared cron branch-prefix helpers for pre-session gates.
// Mirrors services/bossd/internal/cron/scheduler.go cronBranchName slugging.

const MAX_CRON_SLUG = 40

export function cronBranchSlug(name) {
  let slug = String(name ?? '')
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
  if (slug === '') slug = 'job'
  if (slug.length > MAX_CRON_SLUG) slug = slug.slice(0, MAX_CRON_SLUG).replace(/-+$/g, '')
  return slug || 'job'
}

export function hasOpenCronPR(ghPrListJson, slugOrSlugs) {
  const prs = typeof ghPrListJson === 'string' ? JSON.parse(ghPrListJson) : ghPrListJson
  if (!Array.isArray(prs)) return false
  const slugs = Array.isArray(slugOrSlugs) ? slugOrSlugs : [slugOrSlugs]
  const prefixes = slugs.map((slug) => `cron-${cronBranchSlug(slug)}-`)
  return prs.some((pr) => {
    const headRef = String(pr?.headRefName ?? '')
    return prefixes.some((prefix) => headRef.startsWith(prefix))
  })
}
