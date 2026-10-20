import fs from 'node:fs'
import path from 'node:path'

/** Read the root Makefile and its explicitly included fragments in include order. */
export function rootMakefileSources(repoRoot) {
  const text = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
  const sources = [{ path: 'Makefile', text }]
  const anchored = '$(dir $(lastword $(MAKEFILE_LIST)))'
  for (const match of text.replace(/\\\n/g, ' ').matchAll(/^(-?include)\s+(.+)$/gm)) {
    for (const item of match[2]
      .split(anchored)
      .join('')
      .replace(/\s+#.*$/, '')
      .split(/\s+/)
      .filter(Boolean)) {
      try {
        sources.push({ path: item, text: fs.readFileSync(path.resolve(repoRoot, item), 'utf8') })
      } catch (error) {
        if (match[1] !== '-include' || error.code !== 'ENOENT') throw error
      }
    }
  }
  return sources
}

export function readRootMakefile(repoRoot) {
  return rootMakefileSources(repoRoot)
    .map(({ text }) => text)
    .join('\n')
}
