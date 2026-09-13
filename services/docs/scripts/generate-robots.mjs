#!/usr/bin/env node
// Build-time robots.txt generator. Docusaurus copies static/ into build/.
// Keep this staging rule local: docs is an independent pnpm workspace.

import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const staticDir = resolve(__dirname, '..', 'static')
const target = resolve(staticDir, 'robots.txt')

export function isStagingHost(hostname) {
  const firstLabel = hostname.toLowerCase().split('.')[0]
  return firstLabel === 'staging' || firstLabel.endsWith('-staging')
}

export function buildRobotsBody(siteUrl) {
  let isStaging = false
  try {
    isStaging = isStagingHost(new URL(siteUrl).hostname)
  } catch {
    // Fail open for malformed URLs so a production misconfiguration does not
    // silently de-index the site.
  }

  if (isStaging) {
    return '# Staging — keep out of search indexes.\nUser-agent: *\nDisallow: /\n'
  }

  return `User-agent: *\nAllow: /\n\nSitemap: ${siteUrl.replace(/\/$/, '')}/sitemap.xml\n`
}

async function main() {
  const siteUrl = process.env.PUBLIC_DOCS_URL?.trim() || 'https://docs.bossanova.dev'
  const body = buildRobotsBody(siteUrl)
  const isStaging = body.includes('Disallow: /')

  await mkdir(staticDir, { recursive: true })
  await writeFile(target, body, 'utf8')
  console.log(
    `[generate-robots] wrote ${target} for ${isStaging ? 'staging' : 'production'} (PUBLIC_DOCS_URL=${siteUrl})`,
  )
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error)
    process.exitCode = 1
  })
}
