import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'
import { scannedFiles } from './registry.mjs'

export const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const SCANNER_DIR = path.join(REPO_ROOT, 'tools/testscan')

/** @typedef {{name: string, dir: string, file: string, line: number}} Fake */
/** @typedef {{file: string, line: number, inSynctest: boolean}} Sleep */
/**
 * @param {string} root
 * @param {{scans: string[]}} record
 * @returns {{fakes: Fake[], sleeps: Sleep[], errors: unknown[]}}
 */
export function scanGoTests(root, record) {
  const files = scannedFiles(root, record.scans).filter((file) => file.endsWith('_test.go'))
  const result = spawnSync('go', ['run', '-C', SCANNER_DIR, '.', root], {
    cwd: root,
    env: { ...process.env, GOWORK: 'off' },
    input: files.length ? `${files.join('\n')}\n` : '',
    encoding: 'utf8',
    timeout: 120_000,
    maxBuffer: 16 * 1024 * 1024,
  })
  if (result.error || result.status !== 0) {
    throw new Error(
      `Go test scanner failed: ${result.error?.message ?? result.stderr ?? result.status}`,
    )
  }
  const data = JSON.parse(result.stdout)
  if (
    !data ||
    !Array.isArray(data.fakes) ||
    !Array.isArray(data.sleeps) ||
    !Array.isArray(data.errors)
  ) {
    throw new Error('Go test scanner returned invalid output')
  }
  if (data.errors.length)
    throw new Error(`Go test scanner parse errors: ${JSON.stringify(data.errors)}`)
  return data
}
