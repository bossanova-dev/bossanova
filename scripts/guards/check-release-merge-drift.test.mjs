import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { after, test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  findReleaseDrift,
  releaseOwnership,
  renderVerdict,
  stripImagePins,
} from './check-release-merge-drift.mjs'

const script = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  'check-release-merge-drift.mjs',
)

const gitEnv = {
  ...process.env,
  GIT_AUTHOR_NAME: 'Fixture',
  GIT_AUTHOR_EMAIL: 'fixture@example.com',
  GIT_COMMITTER_NAME: 'Fixture',
  GIT_COMMITTER_EMAIL: 'fixture@example.com',
  GIT_CONFIG_GLOBAL: '/dev/null',
  GIT_CONFIG_NOSYSTEM: '1',
}

const tempRoots = []
after(() => {
  for (const dir of tempRoots) fs.rmSync(dir, { recursive: true, force: true })
})

function makeRepo() {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'release-drift-')))
  tempRoots.push(dir)
  const git = (...args) =>
    execFileSync('git', args, { cwd: dir, encoding: 'utf8', env: gitEnv }).trim()
  const write = (files) => {
    for (const [file, text] of Object.entries(files)) {
      fs.mkdirSync(path.dirname(path.join(dir, file)), { recursive: true })
      fs.writeFileSync(path.join(dir, file), text)
    }
  }
  const commit = (message, files) => {
    write(files)
    git('add', '-A')
    git('commit', '-q', '--no-verify', '-m', message)
    return git('rev-parse', 'HEAD')
  }
  git('-c', 'init.defaultBranch=main', 'init', '-q')
  return { dir, git, commit }
}

const STAGING_OVERLAY = 'infra/kustomize/overlays/staging/kustomization.yml'

// The shape of the real overlay: two sed-pinned image tags plus config the release never writes.
function kustomization(tag, { patches = ['patch-configmap-bosso.yml'] } = {}) {
  return [
    'images:',
    '  - name: example/bosso',
    `    newTag: '${tag}'  # bosso`,
    '  - name: example/mcp-gateway',
    `    newTag: '${tag}'  # mcp-gateway`,
    'patches:',
    ...patches.map((patch) => `  - path: ${patch}`),
    '',
  ].join('\n')
}

// What main carries in the files a release later touches.
const MAIN_TREE = {
  [STAGING_OVERLAY]: kustomization('1.2.2'),
  'infra/kustomize/overlays/staging/patch-configmap-bosso.yml': 'origins: strict\n',
}

// What perform-staging-release.yml / perform-production-release.yml commit onto a release branch.
const RELEASE_COMMIT = {
  'CHANGELOG.md': '# v1.2.3\n',
  '.VERSION': '1.2.3\n',
  [STAGING_OVERLAY]: kustomization('1.2.3'),
  'services/marketing/src/content/changelog/1.2.3.md': 'release notes\n',
}

// Merge `head` into `branch` the way a release PR's refs/pull/N/merge does, returning the merge.
function releaseMerge(repo, branch, head) {
  repo.git('checkout', '-q', branch)
  repo.git('merge', '-q', '--no-ff', '--no-edit', head)
  return repo.git('rev-parse', 'HEAD')
}

test('a plain promotion of main passes, with only release-owned files differing', () => {
  const repo = makeRepo()
  repo.commit('base', { 'a.go': 'a1\n', ...MAIN_TREE })
  repo.git('checkout', '-q', '-b', 'staging')
  repo.commit('release v1.2.3', RELEASE_COMMIT)
  repo.git('checkout', '-q', 'main')
  const head = repo.commit('feature', { 'a.go': 'a2\n', 'b.go': 'b\n' })
  const merge = releaseMerge(repo, 'staging', head)

  const raw = repo.git('diff', '--name-only', head, merge).split('\n').sort()
  assert.deepEqual(raw, Object.keys(RELEASE_COMMIT).sort())
  assert.deepEqual(findReleaseDrift({ head, merge, cwd: repo.dir }), [])
})

test('a release branch that kept a hunk main reverted is flagged (the #3095 route)', () => {
  const repo = makeRepo()
  const base = repo.commit('base', { 'host_service_test.go': 'poll\n', 'other.go': 'x\n' })
  repo.git('checkout', '-q', '-b', 'staging')

  // Release PR head is the unsquashed feature commit, never a main commit.
  repo.git('checkout', '-q', '-b', 'release/staging-1', base)
  const unsquashed = repo.commit('wait on the finalizer', { 'host_service_test.go': 'finalizer\n' })
  releaseMerge(repo, 'staging', unsquashed)

  // Main gets the same change as a squash, then a later PR drops the hunk on main only.
  repo.git('checkout', '-q', 'main')
  repo.commit('squash: wait on the finalizer', { 'host_service_test.go': 'finalizer\n' })
  repo.commit('unrelated PR reverts the hunk', { 'host_service_test.go': 'poll\n' })
  const head = repo.commit('next feature', { 'other.go': 'y\n' })
  const merge = releaseMerge(repo, 'staging', head)

  assert.deepEqual(findReleaseDrift({ head, merge, cwd: repo.dir }), ['host_service_test.go'])
})

test('a fix cherry-picked onto the release candidate passes (the release-fix procedure)', () => {
  const repo = makeRepo()
  repo.commit('base', { 'a.go': 'a1\n', 'fix.go': 'broken\n', ...MAIN_TREE })
  repo.git('checkout', '-q', '-b', 'staging')
  repo.commit('release v1.2.3', RELEASE_COMMIT)
  repo.git('checkout', '-q', 'main')
  const cut = repo.commit('feature', { 'a.go': 'a2\n' })
  repo.git('branch', 'release/staging-2', cut)
  const mainFix = repo.commit('fix the release-only check', { 'fix.go': 'fixed\n' })
  repo.git('checkout', '-q', 'release/staging-2')
  repo.git('cherry-pick', '-x', mainFix)
  const head = repo.git('rev-parse', 'HEAD')

  const onMain = spawnSync('git', ['merge-base', '--is-ancestor', head, 'main'], {
    cwd: repo.dir,
    env: gitEnv,
  })
  assert.equal(onMain.status, 1, 'the cherry-picked head is not on main')
  const merge = releaseMerge(repo, 'staging', head)
  assert.deepEqual(findReleaseDrift({ head, merge, cwd: repo.dir }), [])
})

test('the CLI exits 1 naming the drifted file and how to fix it, and 0 when clean', () => {
  const repo = makeRepo()
  repo.commit('base', { 'keep.go': 'main\n', ...MAIN_TREE })
  repo.git('checkout', '-q', '-b', 'staging')
  repo.commit('hand edit on the release branch', { 'keep.go': 'release-only\n', ...RELEASE_COMMIT })
  repo.git('checkout', '-q', 'main')
  const head = repo.commit('feature', { 'new.go': 'n\n' })
  const merge = releaseMerge(repo, 'staging', head)

  const red = spawnSync(process.execPath, [script, '--head', head, '--merge', merge], {
    cwd: repo.dir,
    encoding: 'utf8',
  })
  assert.equal(red.status, 1, red.stderr)
  assert.match(red.stderr, /^ {2}- keep\.go$/m)
  assert.doesNotMatch(red.stderr, /CHANGELOG\.md$/m)
  assert.match(red.stderr, /land it on main/)

  const green = spawnSync(process.execPath, [script, '--head', head, '--merge', head], {
    cwd: repo.dir,
    encoding: 'utf8',
  })
  assert.equal(green.status, 0, green.stderr)
})

test('a depth-2 clone of the merge commit, as release-drift checks out, holds what the CLI reads', () => {
  const repo = makeRepo()
  repo.commit('base', { 'keep.go': 'main\n', ...MAIN_TREE })
  repo.git('checkout', '-q', '-b', 'staging')
  repo.commit('hand edit on the release branch', { 'keep.go': 'release-only\n' })
  repo.commit('release v1.2.3', RELEASE_COMMIT)
  repo.git('checkout', '-q', 'main')
  repo.commit('feature one', { 'one.go': '1\n' })
  repo.commit('feature two', { 'two.go': '2\n' })
  const head = repo.commit('feature three', { 'three.go': '3\n' })
  releaseMerge(repo, 'staging', head)

  const shallow = path.join(repo.dir, 'shallow')
  execFileSync(
    'git',
    ['clone', '-q', '--depth', '2', '--branch', 'staging', `file://${repo.dir}`, shallow],
    { env: gitEnv },
  )
  const depth = execFileSync('git', ['rev-list', '--count', 'HEAD'], {
    cwd: shallow,
    encoding: 'utf8',
    env: gitEnv,
  }).trim()
  assert.ok(Number(depth) < 8, `clone is shallow (${depth} commits)`)

  const run = spawnSync(process.execPath, [script, '--head', head, '--merge', 'HEAD'], {
    cwd: shallow,
    encoding: 'utf8',
  })
  assert.equal(run.status, 1, run.stderr)
  assert.match(run.stderr, /^ {2}- keep\.go$/m)
  assert.doesNotMatch(run.stderr, /^ {2}- infra\//m, 'pin-only overlay blobs read in the clone')
})

test('the CLI refuses to run without --head', () => {
  const run = spawnSync(process.execPath, [script], { encoding: 'utf8' })
  assert.equal(run.status, 2)
})

test('a release branch that kept an old overlay patch is flagged, though the release writes its overlay', () => {
  const repo = makeRepo()
  repo.commit('base', { 'a.go': 'a1\n', ...MAIN_TREE })
  repo.git('checkout', '-q', '-b', 'staging')
  repo.commit('release v1.2.3', RELEASE_COMMIT)
  repo.commit('hand edit on the release branch', {
    'infra/kustomize/overlays/staging/patch-configmap-bosso.yml': 'origins: any\n',
  })
  repo.git('checkout', '-q', 'main')
  const head = repo.commit('feature', { 'a.go': 'a2\n' })
  const merge = releaseMerge(repo, 'staging', head)

  assert.deepEqual(findReleaseDrift({ head, merge, cwd: repo.dir }), [
    'infra/kustomize/overlays/staging/patch-configmap-bosso.yml',
  ])
})

test('a kustomization.yml edit beyond its pinned tags is flagged', () => {
  const repo = makeRepo()
  repo.commit('base', { 'a.go': 'a1\n', ...MAIN_TREE })
  repo.git('checkout', '-q', '-b', 'staging')
  repo.commit('release v1.2.3', RELEASE_COMMIT)
  repo.commit('hand edit adds a patch on the release branch', {
    [STAGING_OVERLAY]: kustomization('1.2.3', {
      patches: ['patch-configmap-bosso.yml', 'patch-hpa-bosso.yml'],
    }),
  })
  repo.git('checkout', '-q', 'main')
  const head = repo.commit('feature', { 'a.go': 'a2\n' })
  const merge = releaseMerge(repo, 'staging', head)

  assert.deepEqual(findReleaseDrift({ head, merge, cwd: repo.dir }), [STAGING_OVERLAY])
})

test('a kustomization.yml the release branch has and the head lacks is flagged', () => {
  const repo = makeRepo()
  repo.commit('base', { 'a.go': 'a1\n' })
  repo.git('checkout', '-q', '-b', 'staging')
  repo.commit('release v1.2.3', RELEASE_COMMIT)
  repo.git('checkout', '-q', 'main')
  const head = repo.commit('feature', { 'a.go': 'a2\n' })
  const merge = releaseMerge(repo, 'staging', head)

  assert.deepEqual(findReleaseDrift({ head, merge, cwd: repo.dir }), [STAGING_OVERLAY])
})

test('stripImagePins blanks only the sed-pinned newTag lines', () => {
  assert.equal(stripImagePins(kustomization('1.2.2')), stripImagePins(kustomization('9.9.9-rc.1')))
  assert.equal(
    stripImagePins(`    newTag: "1.0"  # bosso\n`),
    stripImagePins(`    newTag: '2.0'  # bosso\n`),
  )
  const unpinned = `    newTag: '1.0'  # redis\n`
  assert.equal(stripImagePins(unpinned), unpinned)
  assert.notEqual(
    stripImagePins(kustomization('1.0')),
    stripImagePins(kustomization('1.0', { patches: [] })),
  )
})

test('stripImagePins blanks exactly the span the release sed rewrites', () => {
  const pin = (line) => stripImagePins(`${line}\n`)
  assert.equal(pin(`    newTag: '1.0' # bosso`), pin(`    newTag: '2.0'  # bosso`))
  assert.equal(pin(`    newTag: '1.0'# mcp-gateway`), pin(`    newTag: '2.0'  # mcp-gateway`))
  for (const line of [
    `    newTag: '1.0'\t# bosso`,
    `    newTag: '1.0'  # bossonova`,
    `    newTag: '1.0'  # bosso, pinned by hand`,
    `    newTag: '1.0'  # mcp-gateway-canary`,
  ]) {
    assert.equal(pin(line), `${line}\n`, `not a sed pin line: ${JSON.stringify(line)}`)
  }
  assert.notEqual(pin(`    newTag: '1.0'  # bosso `), pin(`    newTag: '1.0'  # bosso`))
})

test('release ownership is exact for files and kustomization.yml, directory-bounded for changelogs', () => {
  assert.equal(releaseOwnership('CHANGELOG.md'), 'file')
  assert.equal(releaseOwnership('services/marketing/src/content/changelog/1.2.3.md'), 'dir')
  assert.equal(
    releaseOwnership('infra/kustomize/overlays/production/kustomization.yml'),
    'image-pins',
  )
  assert.equal(releaseOwnership('infra/kustomize/overlays/staging/kustomization.yml'), 'image-pins')
  assert.equal(releaseOwnership('infra/kustomize/overlays/staging/patch-configmap-bosso.yml'), null)
  assert.equal(releaseOwnership('infra/kustomize/overlays/production/patch-hpa-bosso.yml'), null)
  assert.equal(releaseOwnership('docs/CHANGELOG.md'), null)
  assert.equal(releaseOwnership('.VERSION.bak'), null)
  assert.equal(releaseOwnership('infra/kustomize/overlays/staging-canary/kustomization.yml'), null)
  assert.equal(releaseOwnership('infra/kustomize/base/kustomization.yml'), null)
})

test('renderVerdict passes only an empty drift list', () => {
  assert.equal(renderVerdict({ head: 'abc', drift: [] }).ok, true)
  const red = renderVerdict({ head: 'abc', drift: ['x.go'] })
  assert.equal(red.ok, false)
  assert.match(red.text, /^::error::/)
})
