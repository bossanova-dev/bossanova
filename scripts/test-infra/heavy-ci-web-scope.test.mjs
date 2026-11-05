import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { classifyLines, keyValue } from '../dev/workflow-yaml-lib.mjs'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

for (const workflow of ['test-web.yml', 'test-web-real-stack.yml']) {
  const lines = classifyLines(
    fs.readFileSync(path.join(root, '.github/workflows', workflow), 'utf8'),
  )
  const scope = lines.findIndex(
    (line) => line.kind === 'structural' && keyValue(line.text)?.value === 'paths',
  )
  const start = lines.findIndex(
    (line, i) => i > scope && line.kind === 'structural' && keyValue(line.text)?.key === 'run',
  )
  assert.ok(scope >= 0 && start > scope)
  const body = []
  for (const line of lines.slice(start + 1)) {
    if (line.kind === 'structural') break
    if (line.kind === 'scalar') body.push(line.raw.slice(lines[start].indent + 2))
  }

  for (const [name, event, pr, base, changed, expected, rejectDiff] of [
    [
      'manual bare-head dispatch after a docs-only tip',
      'workflow_dispatch',
      '',
      '',
      'README.md',
      true,
      true,
    ],
    ['manual bare-head dispatch at the first commit', 'workflow_dispatch', '', '', '', true, true],
    [
      'merge dispatch scopes unrelated paths',
      'workflow_dispatch',
      '123',
      '',
      'README.md',
      false,
      false,
    ],
    [
      'merge dispatch runs web changes',
      'workflow_dispatch',
      '123',
      '',
      'services/web/src/App.tsx',
      true,
      false,
    ],
    ['PR scopes unrelated paths', 'pull_request', '', 'main', 'README.md', false, false],
    ['PR runs web changes', 'pull_request', '', 'main', 'services/web/src/App.tsx', true, false],
    ['release always runs', 'pull_request', '', 'production', 'README.md', true, true],
  ]) {
    test(`${workflow}: ${name}`, (t) => {
      const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'heavy-ci-web-scope-'))
      t.after(() => fs.rmSync(temp, { recursive: true, force: true, maxRetries: 3 }))
      fs.writeFileSync(
        path.join(temp, 'git'),
        '#!/bin/bash\n[ "$REJECT_DIFF" != true ] || exit 42\nprintf "%s\\n" "$CHANGED_PATH"\n',
        { mode: 0o755 },
      )
      const output = path.join(temp, 'output')
      const result = spawnSync('/bin/bash', ['-c', body.join('\n')], {
        encoding: 'utf8',
        env: {
          ...process.env,
          PATH: `${temp}:${process.env.PATH}`,
          EVENT_NAME: event,
          PR_NUMBER: pr,
          RELEASE_BASE: base,
          CHANGED_PATH: changed,
          REJECT_DIFF: String(rejectDiff),
          GITHUB_OUTPUT: output,
        },
      })
      assert.equal(result.status, 0, result.stderr)
      assert.equal(fs.readFileSync(output, 'utf8').trim(), `run=${expected}`)
    })
  }
  test(`${workflow}: renaming a file out of the web tree still runs web CI`, (t) => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'heavy-ci-web-rename-'))
    t.after(() => fs.rmSync(temp, { recursive: true, force: true, maxRetries: 3 }))
    const git = (...args) => {
      const result = spawnSync('/usr/bin/git', args, { cwd: temp, encoding: 'utf8' })
      assert.equal(result.status, 0, result.stderr)
    }
    git('init')
    git('config', 'commit.gpgsign', 'false')
    git('config', 'user.name', 'CI fixture')
    git('config', 'user.email', 'ci@example.test')
    fs.mkdirSync(path.join(temp, 'services/web'), { recursive: true })
    fs.writeFileSync(path.join(temp, 'services/web/source.txt'), 'unchanged source\n')
    git('add', 'services/web/source.txt')
    git('commit', '-m', 'add web source')
    fs.mkdirSync(path.join(temp, 'docs'))
    git('mv', 'services/web/source.txt', 'docs/source.txt')
    git('commit', '-m', 'move source out of web')
    const output = path.join(temp, 'output')
    const result = spawnSync('/bin/bash', ['-c', body.join('\n')], {
      cwd: temp,
      encoding: 'utf8',
      env: {
        ...process.env,
        PATH: '/usr/bin:/bin',
        EVENT_NAME: 'pull_request',
        PR_NUMBER: '',
        RELEASE_BASE: 'main',
        GITHUB_OUTPUT: output,
      },
    })
    assert.equal(result.status, 0, result.stderr)
    assert.equal(fs.readFileSync(output, 'utf8').trim(), 'run=true')
  })
}
