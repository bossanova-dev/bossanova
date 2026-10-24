import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
/** @param {import('node:test').TestContext} t @param {Record<string, string>} files */
export function fixture(t, files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'docs-guard-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  for (const [file, text] of Object.entries({
    'scripts/layout/docs-allowlist.json': '["docs/guides — guides"]\n',
    'scripts/layout/docs-moves.json': '[]\n',
    'scripts/guards/docs-layout-baseline.json': '[]\n',
    'scripts/guards/docs-paths-baseline.json': '[]\n',
    ...files,
  })) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['config', 'core.excludesFile', '/dev/null'], { cwd: root })
  execFileSync('git', ['add', '.'], { cwd: root })
  return root
}
/** @param {string} guard @param {string} root @param {string[]} args */
export function cli(guard, root, args = []) {
  return spawnSync(process.execPath, [`scripts/guards/${guard}.mjs`, '--root', root, ...args], {
    encoding: 'utf8',
  })
}
