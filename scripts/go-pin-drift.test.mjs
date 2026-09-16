import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

// Repo root is the parent of scripts/.
const repoRoot = fileURLToPath(new URL('..', import.meta.url))

// Single source of truth: the workspace `go` directive in go.work. It is the floor the
// toolchain is checked against — `go mod download` refuses to run when the toolchain is
// older than it, which is exactly how a stale Dockerfile fails.
function readWorkspaceGoVersion() {
  const goWork = fs.readFileSync(path.join(repoRoot, 'go.work'), 'utf8')
  const match = goWork.match(/^go\s+(\d+\.\d+(?:\.\d+)?)\s*$/m)
  assert.ok(match, 'go.work must declare a `go <version>` directive')
  return match[1]
}

// The release images are the only Go-version surface that PR CI never builds: they are
// built solely by perform-staging-release.yml / perform-production-release.yml, on push
// to staging and production. A workspace bump that misses them stays green on every PR
// and main gate, then fails the release itself — which is what happened when the
// workspace moved to 1.26 and these two files stayed on 1.25.
function listReleaseDockerfiles() {
  const servicesDir = path.join(repoRoot, 'services')
  return fs
    .readdirSync(servicesDir, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => path.join(servicesDir, entry.name, 'Dockerfile.k8s'))
    .filter((file) => fs.existsSync(file))
}

const workspaceGoVersion = readWorkspaceGoVersion()
const workspaceGoMinor = workspaceGoVersion.split('.').slice(0, 2).join('.')
const dockerfiles = listReleaseDockerfiles()

test('(a) the release Dockerfiles exist, so this gate cannot pass vacuously', () => {
  assert.ok(
    dockerfiles.length > 0,
    'expected at least one services/*/Dockerfile.k8s; a rename would make every assertion below vacuous',
  )
})

test('(b) every release Dockerfile builder pins the go.work Go minor', () => {
  for (const file of dockerfiles) {
    const relative = path.relative(repoRoot, file)
    const source = fs.readFileSync(file, 'utf8')
    const bases = [...source.matchAll(/^FROM\s+golang:(\d+\.\d+)(?:\.\d+)?[-\s]/gm)]
    assert.ok(bases.length > 0, `${relative} must build FROM a pinned golang:<major.minor> image`)
    for (const base of bases) {
      assert.equal(
        base[1],
        workspaceGoMinor,
        `${relative} builds on golang:${base[1]} but go.work requires go >= ${workspaceGoVersion}. ` +
          `The golang image sets GOTOOLCHAIN=local, so an older base fails \`go mod download\` at release time.`,
      )
    }
  }
})

test('(c) every stub go.mod written by a release Dockerfile declares the go.work version', () => {
  for (const file of dockerfiles) {
    const relative = path.relative(repoRoot, file)
    const source = fs.readFileSync(file, 'utf8')
    // The stubs stand in for workspace modules that are not copied into the build
    // context. They are written as `echo 'module <path>\ngo <version>' > <dir>/go.mod`.
    const stubs = [...source.matchAll(/\\ngo (\d+\.\d+(?:\.\d+)?)'/g)]
    assert.ok(
      stubs.length > 0,
      `${relative} must write stub go.mod files for the uncopied workspace modules`,
    )
    for (const stub of stubs) {
      assert.equal(
        stub[1],
        workspaceGoVersion,
        `${relative} writes a stub go.mod declaring go ${stub[1]}, but go.work declares go ${workspaceGoVersion}`,
      )
    }
  }
})
