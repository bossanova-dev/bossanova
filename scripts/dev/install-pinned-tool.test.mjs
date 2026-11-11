import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const helper = fileURLToPath(new URL('./install-pinned-tool.sh', import.meta.url))

function fixture(t, { releaseStatus = 0, proxyStatus = 0, go = true } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'install-pinned-tool-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const bin = path.join(dir, 'bin')
  fs.mkdirSync(bin)
  fs.symlinkSync('/bin/sh', path.join(bin, 'sh'))
  const log = path.join(dir, 'calls')
  const write = (name, body) =>
    fs.writeFileSync(path.join(bin, name), `#!/bin/sh\n${body}\n`, { mode: 0o755 })
  // An isolated PATH proves the missing-Go route independently of the host installation.
  for (const name of ['dirname', 'sleep']) {
    if (name === 'sleep') write(name, 'exit 0')
    else fs.symlinkSync('/usr/bin/dirname', path.join(bin, name))
  }
  write('release', `echo release >> "$LOG"; echo release-output; exit ${releaseStatus}`)
  if (go) write('go', `echo "$GOBIN go $*" >> "$LOG"; echo proxy-output; exit ${proxyStatus}`)
  return {
    dir,
    run: () =>
      spawnSync(
        '/bin/sh',
        [helper, dir, 'example.com/tool/cmd/tool', '1.2.3', '--', path.join(bin, 'release')],
        {
          env: { PATH: bin, LOG: log },
          encoding: 'utf8',
        },
      ),
    calls: () => fs.readFileSync(log, 'utf8').trim().split('\n'),
  }
}

test('release success never builds from the module proxy and keeps stdout clean', (t) => {
  const f = fixture(t)
  const result = f.run()
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout, '')
  assert.deepEqual(f.calls(), ['release'])
})

test('exhausted release retries build the pinned module in the supplied GOBIN', (t) => {
  const f = fixture(t, { releaseStatus: 22 })
  const result = f.run()
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout, '')
  assert.deepEqual(f.calls(), [
    'release',
    'release',
    'release',
    `${f.dir} go install example.com/tool/cmd/tool@v1.2.3`,
  ])
})

test('failure of both sources is nonzero and names both sources', (t) => {
  const f = fixture(t, { releaseStatus: 22, proxyStatus: 9 })
  const result = f.run()
  assert.equal(result.status, 9)
  assert.equal(result.stdout, '')
  assert.match(result.stderr, /release.*module proxy/s)
  assert.equal(f.calls().length, 6)
})

test('missing Go preserves the release failure status', (t) => {
  const f = fixture(t, { releaseStatus: 22, go: false })
  const result = f.run()
  assert.equal(result.status, 22)
  assert.equal(result.stdout, '')
  assert.match(result.stderr, /module-proxy fallback unavailable: go not on PATH/)
})
