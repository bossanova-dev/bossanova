import { spawnSync } from 'node:child_process'

export const MAX_BUFFER = 1024 * 1024 * 1024

// Variables that would point git at a repository other than the one named by cwd. A caller
// running inside a git hook inherits them, and they must never redirect the scratch clone.
const GIT_LOCATION_ENV = [
  'GIT_DIR',
  'GIT_WORK_TREE',
  'GIT_INDEX_FILE',
  'GIT_OBJECT_DIRECTORY',
  'GIT_ALTERNATE_OBJECT_DIRECTORIES',
  'GIT_COMMON_DIR',
  'GIT_NAMESPACE',
  'GIT_PREFIX',
]

export function gitEnv() {
  const env = { ...process.env }
  for (const name of GIT_LOCATION_ENV) delete env[name]
  return env
}

export function git(cwd, args, { allowFailure = false, input } = {}) {
  const result = spawnSync('git', args, {
    cwd,
    env: gitEnv(),
    input,
    maxBuffer: MAX_BUFFER,
  })
  if (result.error) throw result.error
  if (result.status !== 0 && !allowFailure) {
    const stderr = result.stderr.toString('utf8').trim()
    throw new Error(`git ${args.join(' ')} failed (exit ${result.status}): ${stderr}`)
  }
  return { status: result.status, stdout: result.stdout, text: result.stdout.toString('utf8') }
}
