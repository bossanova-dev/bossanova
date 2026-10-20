// BOS-1785: replay the mechanical rename; generated outputs come from make generate.
import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'

parseArgs({ strict: true, allowPositionals: false })
const paths = execFileSync('git', ['ls-files', '-z', 'proto', 'lib', 'services', 'plugins'])
  .toString()
  .split('\0')
  .filter((path) => /\.(go|proto|ts)$/.test(path) && !path.includes('/gen/'))
for (const path of paths) {
  const before = readFileSync(path, 'utf8')
  const after = before.replace(/\bStartRun\b|StartRun(?=[A-Z_])/g, 'StartAgentRun')
  if (after !== before) writeFileSync(path, after)
}
