import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const root = fileURLToPath(new URL('../..', import.meta.url))
const helper = path.join(root, 'scripts/dev/ensure-golangci-lint.sh')
const version = 'v2.15.0'

function writeExecutable(file, body) {
  fs.writeFileSync(file, `#!/bin/sh\n${body}\n`, { mode: 0o755 })
}

function fakeLinter(file, linterVersion, builtWith) {
  writeExecutable(
    file,
    `echo "golangci-lint has version ${linterVersion} built with ${builtWith} from abc on 2026-10-16"`,
  )
}

// Keep the fixture PATH isolated from the host Go and golangci-lint.
function fixture(
  t,
  { localGo = 'go1.27.3', releaseGo = 'go1.27.0', toolchain, goToolchain, probeFails = false } = {},
) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ensure-golangci-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const bin = path.join(dir, 'bin')
  const gobin = path.join(dir, 'gobin')
  fs.mkdirSync(bin)
  fs.mkdirSync(gobin)
  const log = path.join(dir, 'log')
  writeExecutable(
    path.join(bin, 'go'),
    `case "$1 $2" in
  "env GOBIN") echo "${gobin}" ;;
  "env GOPATH") echo "${dir}" ;;
  "env GOTOOLCHAIN") echo auto ;;
  "work edit") printf '%s\\n' '${JSON.stringify(toolchain ? { Toolchain: toolchain } : {})}' ;;
  "env GOVERSION")
    if [ "$GOTOOLCHAIN" != "" ] && [ "$GOTOOLCHAIN" != "local" ]; then
      if [ "${probeFails}" = "true" ]; then echo "toolchain download unavailable" >&2; exit 1; fi
      echo "$GOTOOLCHAIN"
    else echo "${localGo}"; fi ;;
  install*) echo "go $*" >> "${log}"
    v=\${2##*@}
    printf '#!/bin/sh\\necho "golangci-lint has version %s built with ${localGo} from (unknown)"\\n' "$v" > "$GOBIN/golangci-lint"
    chmod +x "$GOBIN/golangci-lint" ;;
esac`,
  )
  writeExecutable(
    path.join(bin, 'curl'),
    `echo "curl $*" >> "${log}"
cat > "$3" <<'EOF'
printf '#!/bin/sh\\necho "golangci-lint has version %s built with ${releaseGo} from abc on 2026-10-16"\\n' "\${3#v}" > "$2/golangci-lint"
chmod +x "$2/golangci-lint"
EOF`,
  )
  writeExecutable(path.join(bin, 'sleep'), 'exit 0')
  writeExecutable(
    path.join(bin, 'node'),
    `exec '${process.execPath.replaceAll("'", "'\"'\"'")}' "$@"`,
  )
  const env = {
    HOME: dir,
    PATH: `${gobin}:${bin}:/usr/bin:/bin`,
    ...(goToolchain ? { GOTOOLCHAIN: goToolchain } : {}),
  }
  function run(op) {
    return spawnSync('sh', [helper, op, version], { env, encoding: 'utf8' })
  }
  function calls() {
    return fs.existsSync(log) ? fs.readFileSync(log, 'utf8') : ''
  }
  function installed() {
    return spawnSync(path.join(gobin, 'golangci-lint'), ['--version'], { encoding: 'utf8' }).stdout
  }
  return { gobin, run, calls, installed }
}

test('check reports an absent, mismatched, stale-toolchain and usable binary distinctly', (t) => {
  const f = fixture(t)
  assert.equal(f.run('check').status, 3)

  fakeLinter(path.join(f.gobin, 'golangci-lint'), '2.11.4', 'go1.27.0')
  const mismatch = f.run('check')
  assert.equal(mismatch.status, 1)
  assert.match(mismatch.stderr, /want v2\.15\.0/)

  fakeLinter(path.join(f.gobin, 'golangci-lint'), '2.15.0', 'go1.26.1')
  const stale = f.run('check')
  assert.equal(stale.status, 4)
  assert.match(stale.stderr, /go1\.26\.1/)
  assert.match(stale.stderr, /go1\.27\.3/)

  fakeLinter(path.join(f.gobin, 'golangci-lint'), '2.15.0', 'go1.27.0')
  assert.equal(f.run('check').status, 0, f.run('check').stderr)
})

test('a binary built with a newer Go than the local toolchain is usable', (t) => {
  const f = fixture(t, { localGo: 'go1.26.5' })
  fakeLinter(path.join(f.gobin, 'golangci-lint'), 'v2.15.0', 'go1.27.0')
  assert.equal(f.run('check').status, 0)
})

test('install leaves a usable binary alone', (t) => {
  const f = fixture(t)
  fakeLinter(path.join(f.gobin, 'golangci-lint'), '2.15.0', 'go1.27.0')
  const result = f.run('install')
  assert.equal(result.status, 0, result.stderr)
  assert.equal(f.calls(), '')
})

test('install replaces a stale-toolchain binary with the release when the release is new enough', (t) => {
  const f = fixture(t)
  fakeLinter(path.join(f.gobin, 'golangci-lint'), '2.15.0', 'go1.26.1')
  const result = f.run('install')
  assert.equal(result.status, 0, result.stderr)
  assert.match(f.installed(), /version 2\.15\.0 built with go1\.27\.0/)
  assert.doesNotMatch(f.calls(), /^go install/m)
})

test('install rejects an old release without building from source', (t) => {
  const f = fixture(t, { localGo: 'go1.28.1', toolchain: 'go1.27.5', releaseGo: 'go1.26.1' })
  const result = f.run('install')
  assert.equal(result.status, 4, result.stderr)
  assert.match(result.stderr, /GOLANGCI_LINT_VERSION/)
  assert.match(f.installed(), /built with go1\.26\.1/)
  assert.doesNotMatch(f.calls(), /^go install/m)
})

test('check accepts a release older than the host and newer than the workspace toolchain', (t) => {
  const f = fixture(t, { localGo: 'go1.28.1', toolchain: 'go1.26.5' })
  fakeLinter(path.join(f.gobin, 'golangci-lint'), '2.15.0', 'go1.27.0')
  const result = f.run('check')
  assert.equal(result.status, 0, result.stderr)
})

test('check respects the explicit local toolchain override', (t) => {
  const f = fixture(t, { localGo: 'go1.28.1', toolchain: 'go1.26.5', goToolchain: 'local' })
  fakeLinter(path.join(f.gobin, 'golangci-lint'), '2.15.0', 'go1.27.0')
  const result = f.run('check')
  assert.equal(result.status, 4, result.stderr)
  assert.match(result.stderr, /go1\.28\.1/)
})

test('check reports a failed toolchain probe distinctly', (t) => {
  const f = fixture(t, { localGo: 'go1.28.1', toolchain: 'go1.26.5', probeFails: true })
  fakeLinter(path.join(f.gobin, 'golangci-lint'), '2.15.0', 'go1.27.0')
  const result = f.run('check')
  assert.equal(result.status, 5, result.stderr)
  assert.match(result.stderr, /go1\.26\.5/)
  assert.match(result.stderr, /go1\.28\.1/)
  assert.match(result.stderr, /GOTOOLCHAIN=local/)
  assert.match(result.stderr, /toolchain download unavailable/)
})
