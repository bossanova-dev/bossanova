import { existsSync, rmSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { resolve } from 'node:path'
import assert from 'node:assert/strict'

import { describe, expect, it } from 'vitest'
import { buildRobotsBody as buildMarketingRobotsBody } from '../../marketing/scripts/generate-robots.mjs'

const robotsPath = resolve(process.cwd(), 'static/robots.txt')

describe('docs robots generator', () => {
  it('classifies only staging first labels', async () => {
    const { isStagingHost } = await import('./generate-robots.mjs')

    assert.equal(isStagingHost('docs-staging.bossanova.dev'), true)
    assert.equal(isStagingHost('staging.bossanova.dev'), true)
    assert.equal(isStagingHost('docs.bossanova.dev'), false)
    assert.equal(isStagingHost('bossanova.dev'), false)
    assert.equal(isStagingHost('mystaging.bossanova.dev'), false)
    assert.equal(isStagingHost('staging-docs.example.com'), false)
  })

  it('uses sitemap.xml and fails open for malformed URLs', async () => {
    const { buildRobotsBody } = await import('./generate-robots.mjs')

    assert.equal(
      buildRobotsBody('https://docs.bossanova.dev/'),
      'User-agent: *\nAllow: /\n\nSitemap: https://docs.bossanova.dev/sitemap.xml\n',
    )
    assert.equal(
      buildRobotsBody('https://docs-staging.bossanova.dev'),
      '# Staging — keep out of search indexes.\nUser-agent: *\nDisallow: /\n',
    )
    assert.match(buildRobotsBody('not a URL'), /Allow: \//)
    assert.match(buildRobotsBody('not a URL'), /Sitemap: not a URL\/sitemap\.xml/)
    assert.doesNotMatch(buildRobotsBody('https://docs.bossanova.dev'), /sitemap-index\.xml/)
    assert.match(
      buildMarketingRobotsBody('https://bossanova.dev'),
      /Sitemap: https:\/\/bossanova\.dev\/sitemap-index\.xml/,
    )
  })

  it('does not write robots.txt when imported', () => {
    rmSync(robotsPath, { force: true })
    execFileSync(
      process.execPath,
      ['--input-type=module', '--eval', "import('./scripts/generate-robots.mjs')"],
      {
        cwd: process.cwd(),
      },
    )
    expect(existsSync(robotsPath)).toBe(false)
  })
})
