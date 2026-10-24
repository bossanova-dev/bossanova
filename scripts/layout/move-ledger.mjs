import fs from 'node:fs'
import path from 'node:path'

/** @param {string} repoRoot @param {string} ledger */
export function readMoves(repoRoot, ledger) {
  try {
    const pairs = JSON.parse(fs.readFileSync(path.join(repoRoot, ledger), 'utf8'))
    if (
      !Array.isArray(pairs) ||
      pairs.some(
        (pair) =>
          !Array.isArray(pair) ||
          pair.length !== 2 ||
          pair.some((value) => typeof value !== 'string' || !value),
      )
    )
      return { pairs: [], errors: [`${ledger}: expected [from, to] path pairs`] }
    return { pairs, errors: [] }
  } catch (error) {
    return { pairs: [], errors: [`${ledger}: ${error.message}`] }
  }
}

/** @param {string} text @param {string} from @param {string} to */
export function replacePathReferences(text, from, to) {
  let result = ''
  let at = 0
  let start = text.indexOf(from)
  while (start !== -1) {
    if (
      !/[A-Za-z0-9_./-]/.test(text[start - 1] ?? '') &&
      !/[A-Za-z0-9_.-]/.test(text[start + from.length] ?? '')
    ) {
      result += text.slice(at, start) + to
      at = start + from.length
    }
    start = text.indexOf(from, start + from.length)
  }
  return result + text.slice(at)
}

/** @param {string} file @param {string[]} lines @param {string[][]} moves */
export function staleReferences(file, lines, moves) {
  const failures = []
  for (const [from, to] of moves) {
    for (const [index, line] of lines.entries()) {
      if (replacePathReferences(line, from, to) !== line)
        failures.push(`${file}:${index + 1}: ${from} moved to ${to}; update this reference`)
    }
  }
  return failures
}
