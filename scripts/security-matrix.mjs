// scripts/security-matrix.mjs
// Emits the Go module list from go.work as a JSON array, for the GitHub Actions
// security matrix. Pure Node builtins only (no deps) so it runs in any worktree.
import { readFileSync } from 'node:fs'

const text = readFileSync(new URL('../go.work', import.meta.url), 'utf8')
// Capture the paths inside the `use ( ... )` block.
const block = text.match(/use\s*\(([^)]*)\)/s)
if (!block) {
  console.error('security-matrix: no `use (...)` block found in go.work')
  process.exit(1)
}
const modules = block[1]
  .split('\n')
  .map((l) => l.trim())
  .filter((l) => l.startsWith('./'))
  .map((l) => l.replace(/^\.\//, ''))
if (modules.length === 0) {
  console.error('security-matrix: no modules parsed from go.work')
  process.exit(1)
}
process.stdout.write(JSON.stringify(modules))
