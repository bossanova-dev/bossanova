import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { scanSites } from './check-ci-request-sites.mjs'

const push = 'git push --force-with-lease origin "$SESSION_BRANCH"'
const request = 'node "$BOSS_BUILD_TOOLBOX/ci/ci-request.mjs" request --pr "$PR_NUMBER"'
const fenced = (body) => `# Example\n\n\x60\x60\x60bash\n${body}\n\x60\x60\x60\n`
const scan = (text) => scanSites('skill.md', text)

test('unaccounted tag-and-push reports its file and line, request after push accounts for it', () => {
  const sites = scan(fenced(push))
  assert.deepEqual(sites, [
    { file: 'skill.md', line: 4, command: push, disposition: 'unaccounted' },
  ])
  assert.equal(scan(fenced(`${push}\n${request}`))[0].disposition, 'request')
  assert.equal(scan(fenced(`${request}\n${push}`))[0].disposition, 'unaccounted')
})

test('skip markers require a non-empty reason within the same scope', () => {
  assert.equal(
    scan(fenced(`# ci-request: skip pre-tag push\n${push}`))[0].disposition,
    'skip pre-tag push',
  )
  assert.equal(scan(fenced(`# ci-request: skip   \n${push}`))[0].disposition, 'unaccounted')
  assert.equal(
    scan(`${fenced('# ci-request: skip no push here')}\n${fenced(push)}`)[0].disposition,
    'unaccounted',
  )
})

test('inline push and helper invocations need a marker in their paragraph', () => {
  assert.equal(scan('- Use `git push` when done.')[0].disposition, 'unaccounted')
  assert.equal(
    scan('- Use `git push` when done. <!-- ci-request: skip rule statement -->')[0].disposition,
    'skip rule statement',
  )
  assert.equal(scan('| Helper | `push-branch.mjs` |').length, 0)
  assert.equal(scan('| Helper | `node push-branch.mjs` |')[0].disposition, 'unaccounted')
  assert.equal(
    scan('- Use `git push`.\n\n<!-- ci-request: skip unrelated paragraph -->')[0].disposition,
    'unaccounted',
  )
})

test('blockquote and list shell fences, git -C, and consecutive push lines are scanned', () => {
  assert.equal(scan('> ```sh\n> git -C "$ROOT" push origin branch\n> ```')[0].line, 2)
  assert.equal(scan('- Item\n\n  ```bash\n  git push\n  ```')[0].line, 4)
  assert.equal(scan(fenced('git push origin a\ngit push origin b')).length, 1)
  assert.equal(scan('```text\ngit push\n```').length, 0)
  assert.equal(scan('- ```bash\n  echo hi\n`git push`')[0].line, 3)
})

test('canonical toolbox git calls and argument arrays require adjacent code markers', () => {
  for (const code of [
    "git('push', 'origin', branch)",
    "repo.git('push', 'origin', tag)",
    "tryGit(['push', remote, branch])",
    "const args = ['push', '-u', remote, branch]",
    "execFileSync('git', ['push', 'origin'])",
  ]) {
    assert.equal(scanSites('helper.mjs', code)[0].disposition, 'unaccounted')
    assert.equal(
      scanSites('helper.mjs', `// ci-request: skip caller requests after verification\n${code}`)[0]
        .disposition,
      'skip caller requests after verification',
    )
    assert.equal(
      scanSites('helper.mjs', `// ci-request: skip\n${code}`)[0].disposition,
      'unaccounted',
    )
  }
  assert.equal(
    scanSites('helper.sh', '# ci-request: skip ready immediately\ngit push')[0].disposition,
    'skip ready immediately',
  )
})

test('CLI explicit file mode fails unaccounted pushes and lists accounted sites', () => {
  const dir = mkdtempSync(join(tmpdir(), 'ci-request-sites-'))
  try {
    const file = join(dir, 'skill.md')
    writeFileSync(file, fenced(push))
    const bad = spawnSync(
      process.execPath,
      ['scripts/guards/check-ci-request-sites.mjs', '--files', file],
      { encoding: 'utf8' },
    )
    assert.equal(bad.status, 1)
    assert.ok(bad.stderr.includes(`${file}:4`))
    writeFileSync(file, fenced(`${push}\n${request}`))
    const good = spawnSync(
      process.execPath,
      ['scripts/guards/check-ci-request-sites.mjs', '--list', '--files', file],
      { encoding: 'utf8' },
    )
    assert.equal(good.status, 0, good.stderr)
    assert.ok(good.stdout.includes('request'))
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})
