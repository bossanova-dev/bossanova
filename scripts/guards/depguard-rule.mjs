import fs from 'node:fs'
import path from 'node:path'
import { parse } from 'yaml'

export function imports(source) {
  const tokens = [
    ...source.matchAll(
      /\/\/[^\n]*|\/\*[\s\S]*?\*\/|`[^`]*`|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[A-Za-z_][A-Za-z0-9_]*|[^\s]/g,
    ),
  ]
    .map((match) => match[0])
    .filter((token) => !token.startsWith('//') && !token.startsWith('/*'))
  const packages = []
  for (let i = 0; i < tokens.length; i++) {
    if (tokens[i] !== 'import') continue
    const grouped = tokens[i + 1] === '('
    let j = i + 1
    for (; j < tokens.length; j++) {
      const token = tokens[j]
      if (grouped && token === ')') break
      if (token.startsWith('"') || token.startsWith('`')) {
        packages.push(token.slice(1, -1))
        if (!grouped) break
      }
    }
    i = j
  }
  return packages
}
function markers(id) {
  return {
    begin: `# BEGIN generated: ${id} baseline (node scripts/guards/${id}.mjs --write)`,
    endMarker: '# END generated',
  }
}
function generatedBlock(id, entries) {
  const { begin, endMarker } = markers(id)
  return `${begin}\n${entries.map((file) => `            - "!**/${file}"\n`).join('')}            ${endMarker}`
}
export function checkDepguardRule({
  repoRoot,
  id,
  denied,
  files,
  entries = null,
  write = false,
  allowed = null,
  listMode = null,
}) {
  const failures = []
  const { begin, endMarker } = markers(id)
  const configPath = path.join(repoRoot, '.golangci.yml')
  try {
    let config = fs.readFileSync(configPath, 'utf8')
    if (entries !== null) {
      const start = config.indexOf(begin),
        end = config.indexOf(endMarker, start)
      if (start < 0 || end < 0)
        failures.push(`.golangci.yml: missing ${id} generated baseline block`)
      else {
        const expected = generatedBlock(id, entries)
        if (write) {
          config = config.slice(0, start) + expected + config.slice(end + endMarker.length)
          fs.writeFileSync(configPath, config)
        } else if (config.slice(start, end + endMarker.length) !== expected)
          failures.push(
            `.golangci.yml: generated ${id} baseline block is stale; run make ${id}-update`,
          )
      }
    }
    const rule = parse(config)?.linters?.settings?.depguard?.rules?.[id]
    if (!rule) failures.push(`.golangci.yml: missing ${id} depguard rule`)
    for (const pkg of denied)
      if (!rule?.deny?.some((entry) => entry.pkg === pkg))
        failures.push(`.golangci.yml: ${id} missing deny ${pkg}`)
    if (listMode !== null && rule?.['list-mode'] !== listMode)
      failures.push(`.golangci.yml: ${id} list-mode must be ${listMode}`)
    if (allowed !== null && JSON.stringify(rule?.allow) !== JSON.stringify(allowed))
      failures.push(`.golangci.yml: ${id} allow must match configured domain packages`)
    const expectedFiles = [...files, ...(entries ?? []).map((file) => `!**/${file}`)]
    if (JSON.stringify(rule?.files) !== JSON.stringify(expectedFiles))
      failures.push(
        `.golangci.yml: ${id} files must match configured scopes, exemptions and baseline`,
      )
  } catch (error) {
    failures.push(`.golangci.yml: ${error.message}`)
  }
  return failures
}
