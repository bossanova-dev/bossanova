import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
const source = new URL('../../infra/kustomize/release.sh', import.meta.url)
const revision = 'abcdef123456789012345678901234567890123456'
const document =
  'apiVersion: v1\nkind: Secret\nmetadata:\n  name: prepared-abc1234567\n  namespace: bs-production\ndata:\n  PASSWORD: c2VjcmV0\n'
function fixture(phase, commit = revision) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'repository-deploy-'))
  const script = path.join(directory, 'infra/kustomize/release.sh')
  const overlayRoot = path.join(directory, 'infra/kustomize')
  fs.mkdirSync(path.dirname(script), { recursive: true })
  fs.mkdirSync(overlayRoot, { recursive: true })
  fs.writeFileSync(script, fs.readFileSync(source))
  const tools = path.join(directory, 'tools')
  fs.mkdirSync(tools)
  const log = path.join(directory, 'calls')
  const output = path.join(directory, 'prepared.yaml')
  const write = (name, body) =>
    fs.writeFileSync(path.join(tools, name), '#!/bin/bash\nset -e\n' + body, { mode: 0o700 })
  write(
    'git',
    'case "$1" in rev-parse) case "$2" in --abbrev-ref) echo production;; *) echo abcdef123456789012345678901234567890123456;; esac;; *) echo "UNEXPECTED git $*" >> "$CALL_LOG"; exit 91;; esac\n',
  )
  write('gcloud', 'echo "UNEXPECTED gcloud" >> "$CALL_LOG"; exit 92\n')
  write(
    'kubectl',
    'echo "kubectl $*" >> "$CALL_LOG"\nif [ "$1" = kustomize ]; then printf "%s" "$RENDERED"; fi\n',
  )
  write('make', 'echo "UNEXPECTED make $*" >> "$CALL_LOG"; exit 93\n')
  fs.writeFileSync(
    path.join(overlayRoot, 'pull-secrets.sh'),
    '#!/bin/bash\necho pull-secrets >> "$CALL_LOG"\n',
    { mode: 0o700 },
  )
  if (phase === 'apply') fs.writeFileSync(output, document, { mode: 0o600 })
  const result = spawnSync('/bin/bash', [script, 'production'], {
    cwd: directory,
    encoding: 'utf8',
    env: {
      PATH: tools + ':/usr/bin:/bin',
      HOME: directory,
      CALL_LOG: log,
      RENDERED: document,
      TARGET_BRANCH: 'production',
      GIGANTIC_DEPLOY_PHASE: phase,
      GIGANTIC_DEPLOY_COMMIT: commit,
      GIGANTIC_DEPLOY_MANIFEST: output,
      KUBECONFIG: path.join(directory, 'isolated-config'),
    },
  })
  const calls = fs.existsSync(log) ? fs.readFileSync(log, 'utf8') : ''
  const rendered = fs.existsSync(output) ? fs.readFileSync(output, 'utf8') : ''
  const mode = fs.existsSync(output) ? fs.statSync(output).mode & 0o777 : null
  fs.rmSync(directory, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  return { ...result, calls, rendered, mode }
}
test('operator prepare reuses secret fetching, renders privately and makes no cluster writes or git updates', () => {
  const result = fixture('prepare')
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.rendered, document)
  assert.equal(result.mode, 0o600)
  assert.match(result.calls, /pull-secrets/)
  assert.match(result.calls, /kubectl kustomize/)
  assert.doesNotMatch(result.calls, /UNEXPECTED|kubectl apply|kubectl delete|kubectl rollout/)
  assert.doesNotMatch(result.stdout + result.stderr, /c2VjcmV0/)
})
test('operator apply consumes prepared manifest without refetching secrets or changing credentials', () => {
  const result = fixture('apply')
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.calls, /kubectl apply .*prepared.yaml/)
  assert.doesNotMatch(result.calls, /UNEXPECTED|pull-secrets|kubectl kustomize/)
  assert.match(result.calls, /rollout restart.*statefulset\/bs-bosso/)
  assert.match(result.calls, /rollout status.*deployment\/bs-deploy-redis/)
})
test('operator mode refuses unknown phase and changed source before secret or cluster calls', () => {
  for (const result of [fixture('unknown'), fixture('prepare', '0'.repeat(40))]) {
    assert.notEqual(result.status, 0)
    assert.equal(result.calls, '')
  }
})

test('Terraform Cloud bearer token travels through stdin rather than process arguments', () => {
  const source = fs.readFileSync(
    new URL('../../infra/kustomize/pull-secrets.sh', import.meta.url),
    'utf8',
  )
  const start = source.indexOf('tfc_get()')
  const end = source.indexOf('\n}', start)
  assert.ok(start >= 0 && end > start)
  const functionSource = source.slice(start, end + 2)
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'terraform-token-'))
  try {
    fs.writeFileSync(
      path.join(directory, 'curl'),
      '#!/bin/bash\nprintf "%s\\n" "$@" > "$ARG_LOG"\ncat > "$STDIN_LOG"\nprintf "{}"\n',
      { mode: 0o700 },
    )
    const result = spawnSync(
      '/bin/bash',
      ['-c', functionSource + '\ntfc_get https://app.terraform.io/api/v2/workspaces/test'],
      {
        encoding: 'utf8',
        env: {
          PATH: directory + ':/usr/bin:/bin',
          TOKEN: 'private-token',
          ARG_LOG: path.join(directory, 'args'),
          STDIN_LOG: path.join(directory, 'stdin'),
        },
      },
    )
    assert.equal(result.status, 0, result.stderr)
    assert.doesNotMatch(fs.readFileSync(path.join(directory, 'args'), 'utf8'), /private-token/)
    assert.match(
      fs.readFileSync(path.join(directory, 'stdin'), 'utf8'),
      /Authorization: Bearer private-token/,
    )
  } finally {
    fs.rmSync(directory, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})
