import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import crypto from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const installSh = path.join(repoRoot, 'infra/install.sh')
// infra/ is not published to the public mirror.
const skip = !fs.existsSync(installSh) && 'infra/install.sh is not in this tree'

const ASSET = 'boss-linux-amd64'
const BINARY = '#!/bin/sh\necho boss\n'

// A curl stand-in that serves files from $STUB_ASSETS by URL basename and fails like
// `curl -f` on a 404 when the asset is absent.
const STUB_CURL = `#!/bin/sh
out=''
url=''
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
src="$STUB_ASSETS/\${url##*/}"
[ -f "$src" ] || exit 22
cp "$src" "$out"
`

function sha256(text) {
  return crypto.createHash('sha256').update(text).digest('hex')
}

function setup(assets) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'install-sh-'))
  for (const sub of ['bin', 'assets', 'out']) fs.mkdirSync(path.join(dir, sub))
  fs.writeFileSync(path.join(dir, 'bin/curl'), STUB_CURL, { mode: 0o755 })
  for (const [name, contents] of Object.entries(assets)) {
    fs.writeFileSync(path.join(dir, 'assets', name), contents)
  }
  return dir
}

function downloadAndVerify(dir) {
  const dest = path.join(dir, 'out/boss')
  const result = spawnSync(
    'sh',
    ['-c', '. "$1" && download_and_verify "$2" "$3"', 'sh', installSh, ASSET, dest],
    {
      encoding: 'utf8',
      env: {
        ...process.env,
        PATH: `${path.join(dir, 'bin')}${path.delimiter}${process.env.PATH}`,
        STUB_ASSETS: path.join(dir, 'assets'),
        BOSS_INSTALL_SH_SOURCE_ONLY: '1',
      },
    },
  )
  return { ...result, dest }
}

function withTree(assets, fn) {
  const dir = setup(assets)
  try {
    fn(dir)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
}

test(
  'sourcing with BOSS_INSTALL_SH_SOURCE_ONLY=1 defines functions without running main',
  { skip },
  () => {
    const result = spawnSync(
      'sh',
      [
        '-c',
        '. "$1" && command -v download_and_verify >/dev/null && echo sourced',
        'sh',
        installSh,
      ],
      {
        encoding: 'utf8',
        env: { ...process.env, BOSS_INSTALL_SH_SOURCE_ONLY: '1', PATH: '/usr/bin:/bin' },
      },
    )
    assert.equal(result.status, 0, result.stderr)
    assert.equal(result.stdout, 'sourced\n')
    assert.equal(result.stderr, '')
  },
)

test('a missing .sha256 aborts and leaves nothing to install', { skip }, () => {
  withTree({ [ASSET]: BINARY }, (dir) => {
    const result = downloadAndVerify(dir)
    assert.notEqual(result.status, 0)
    assert.match(result.stderr, new RegExp(`${ASSET}\\.sha256`))
    assert.equal(fs.existsSync(result.dest), false)
  })
})

test('a matching checksum installs the verified binary', { skip }, () => {
  withTree({ [ASSET]: BINARY, [`${ASSET}.sha256`]: `${sha256(BINARY)}  ${ASSET}\n` }, (dir) => {
    const result = downloadAndVerify(dir)
    assert.equal(result.status, 0, result.stderr)
    assert.equal(fs.readFileSync(result.dest, 'utf8'), BINARY)
    assert.equal(fs.existsSync(`${result.dest}.sha256`), false)
  })
})

test('a wrong checksum aborts and removes the download', { skip }, () => {
  withTree({ [ASSET]: BINARY, [`${ASSET}.sha256`]: `${sha256('tampered')}  ${ASSET}\n` }, (dir) => {
    const result = downloadAndVerify(dir)
    assert.notEqual(result.status, 0)
    assert.match(result.stderr, /SHA256 mismatch/)
    assert.equal(fs.existsSync(result.dest), false)
  })
})

test('an empty or malformed .sha256 aborts', { skip }, () => {
  for (const checksum of ['', 'not-a-checksum\n']) {
    withTree({ [ASSET]: BINARY, [`${ASSET}.sha256`]: checksum }, (dir) => {
      const result = downloadAndVerify(dir)
      assert.notEqual(result.status, 0, JSON.stringify(checksum))
      assert.match(result.stderr, new RegExp(ASSET))
      assert.equal(fs.existsSync(result.dest), false)
    })
  }
})

test('a hex checksum of the wrong length aborts', { skip }, () => {
  const digest = sha256(BINARY)
  for (const checksum of [digest.slice(0, 63), `${digest}a`]) {
    withTree({ [ASSET]: BINARY, [`${ASSET}.sha256`]: `${checksum}  ${ASSET}\n` }, (dir) => {
      const result = downloadAndVerify(dir)
      assert.notEqual(result.status, 0, checksum)
      assert.match(result.stderr, /not a SHA256 digest/, checksum)
      assert.equal(fs.existsSync(result.dest), false, checksum)
      assert.equal(fs.existsSync(`${result.dest}.sha256`), false, checksum)
    })
  }
})
