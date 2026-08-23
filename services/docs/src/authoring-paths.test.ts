import { existsSync, readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')

const documentedRepoPaths = [
  {
    doc: 'services/docs/README.md',
    paths: [
      'services/docs/vitest.config.ts',
      'services/docs/src/test/theme/',
      'services/docs/src/test/theme/Tabs.tsx',
      'services/docs/src/test/theme/TabItem.tsx',
      'services/docs/src/test/theme/CodeBlock.tsx',
    ],
  },
  {
    doc: 'services/marketing/DESIGN.md',
    paths: ['services/marketing/src/components/ThemeToggle.astro'],
  },
]

describe('authoring docs path references', () => {
  for (const { doc, paths } of documentedRepoPaths) {
    it(`${doc} points at repository paths that still exist`, () => {
      const body = readFileSync(resolve(repoRoot, doc), 'utf8')

      for (const path of paths) {
        expect(body, `${doc} should mention ${path}`).toContain(path)
        expect(existsSync(resolve(repoRoot, path)), `${path} should exist`).toBe(true)
      }
    })
  }
})
