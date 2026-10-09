import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { evaluateBossReleaseGate, main } from './boss-release.mjs'

function fixture(t, extension) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'release-cron-'))
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }))
  const root = path.join(tmp, 'repo')
  const gitAt = (cwd, ...args) =>
    execFileSync('/usr/bin/git', args, {
      cwd,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    }).trim()
  const git = (...args) => gitAt(root, ...args)
  gitAt(tmp, 'init', '--bare', '--initial-branch=main', path.join(tmp, 'origin'))
  gitAt(tmp, 'clone', path.join(tmp, 'origin'), root)
  git('config', 'user.name', 'Test')
  git('config', 'commit.gpgsign', 'false')
  git('config', 'user.email', 'test@example.com')
  git('commit', '--allow-empty', '-m', 'baseline')
  git('tag', 'v1')
  git('commit', '--allow-empty', '-m', 'change')
  git('push', 'origin', 'main', '--tags')
  if (extension) {
    const dir = path.join(root, '.claude/skills/boss-release-test')
    fs.mkdirSync(dir, { recursive: true })
    fs.writeFileSync(
      path.join(dir, 'SKILL.md'),
      '---\nx-boss-extension:\n  extends: boss-release\n  role: release\n---\n',
    )
  }
  let green = true
  const run = (command, args, { cwd }) => {
    if (command === 'git') return gitAt(cwd, ...args)
    const endpoint = args[1]
    if (endpoint.includes('check-runs'))
      return JSON.stringify([
        {
          check_runs: [
            { name: 'test', status: 'completed', conclusion: green ? 'success' : 'failure' },
          ],
        },
      ])
    return JSON.stringify([endpoint.includes('/status') ? { statuses: [] } : { workflow_runs: [] }])
  }
  return {
    root,
    run,
    red: () => {
      green = false
    },
  }
}

test('cron exits zero only when an installed extension has a ready candidate', (t) => {
  const f = fixture(t, true)
  assert.equal(evaluateBossReleaseGate(f).hasWork, true)
  assert.equal(main(f, { stderr: () => {} }), 0)
  f.red()
  assert.equal(evaluateBossReleaseGate(f).hasWork, false)
  assert.equal(main(f, { stderr: () => {} }), 1)
})

test('no extension never wakes the agent and prints the report', (t) => {
  const f = fixture(t, false)
  const verdict = evaluateBossReleaseGate(f)
  assert.equal(verdict.hasWork, false)
  assert.match(
    verdict.reason,
    /1 merged changes unreleased since v1; no release extension installed/,
  )
  let output = ''
  assert.equal(
    main(f, {
      stderr: (line) => {
        output += line
      },
    }),
    1,
  )
  assert.match(output, /no release extension installed/)
})

test('cron fails closed with a reason on a read failure', () => {
  let output = ''
  assert.equal(
    main(
      {
        root: process.cwd(),
        run: () => {
          throw new Error('fetch refused')
        },
      },
      {
        stderr: (line) => {
          output += line
        },
      },
    ),
    1,
  )
  assert.match(output, /fetch refused/)
})
