// golangci-lint caps its output by default (max-issues-per-linter: 50, max-same-issues: 3), so a
// baseline built from its report silently misses violations past the cap. A prototype found the
// caps hid 31 of 34. Require both caps to be 0 and same-line deduplication to be off.
//
// No YAML parser: `yaml` is not installed (the constraint scripts/guards/check-vacuous-job-gates.mjs also
// documents), so this reads the top-level block by indentation. A missing key fails, because
// missing means golangci's default cap applies.

export const CAP_KEYS = ['max-issues-per-linter', 'max-same-issues']
const REQUIRED_VALUES = {
  ...Object.fromEntries(CAP_KEYS.map((key) => [key, '0'])),
  'uniq-by-line': 'false',
}

const indentOf = (line) => line.length - line.trimStart().length
const isSkippable = (line) => line.trim() === '' || line.trimStart().startsWith('#')
const stripComment = (value) => value.replace(/\s+#.*$/, '').trim()

/**
 * @param {string} text Contents of .golangci.yml.
 * @returns {{failures: string[]}}
 */
export function checkGolangciCaps(text) {
  const lines = text.split(/\r?\n/)
  const start = lines.findIndex((line) => /^issues:\s*(?:#.*)?$/.test(line))
  if (start === -1) {
    return {
      failures: [
        `.golangci.yml has no top-level issues: block — add issues: { ${Object.entries(
          REQUIRED_VALUES,
        )
          .map(([key, value]) => `${key}: ${value}`)
          .join(', ')} }`,
      ],
    }
  }
  const values = new Map()
  let childIndent = null
  for (let i = start + 1; i < lines.length; i++) {
    const line = lines[i]
    if (isSkippable(line)) continue
    const indent = indentOf(line)
    if (indent === 0) break
    childIndent ??= indent
    if (indent !== childIndent) continue
    const match = /^\s*([\w-]+):(.*)$/.exec(line)
    if (match) values.set(match[1], stripComment(match[2]))
  }
  const failures = []
  for (const [key, expected] of Object.entries(REQUIRED_VALUES)) {
    if (!values.has(key)) {
      failures.push(
        `.golangci.yml issues.${key} is not set, so golangci's default filtering applies — set it to ${expected}`,
      )
    } else if (values.get(key) !== expected) {
      failures.push(`.golangci.yml issues.${key} is ${values.get(key)} — set it to ${expected}`)
    }
  }
  return { failures }
}
