// Pure content-hash helpers for the cached-lint stamp layer (BOS-340).
//
// `moduleLintKey` produces a deterministic hex sha256 over every lint-relevant
// ingredient for a Go module: the module's own git tree hash, the tree hashes
// of its declared deps (e.g. lib/bossalib, which every other module `replace`s
// in), the root .golangci config hash, and the pinned golangci-lint + go
// versions. Any ingredient change changes the key; nothing else does.
//
// `hashTree` hashes a directory's git-tracked content (mode+oid rows, cheap —
// no file reads) plus a working-tree overlay for any dirty or untracked files,
// so an unstaged edit invalidates the key even though the committed oid is
// stale.

import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'

function sha256Hex(data) {
  return createHash('sha256').update(data).digest('hex')
}

// Canonical JSON: object keys sorted recursively so serialization order can
// never perturb the digest.
function canonicalize(value) {
  if (Array.isArray(value)) {
    return value.map(canonicalize)
  }
  if (value && typeof value === 'object') {
    const out = {}
    for (const key of Object.keys(value).sort()) {
      out[key] = canonicalize(value[key])
    }
    return out
  }
  return value
}

// Deterministic hex sha256 of a module's lint-relevant inputs.
//   { treeHashes: { <dir>: <hash>, ... }, moduleDir, deps: [<dir>...],
//     configHash, versions: { golangci, go, buildEnv, ... } }
// The whole `versions` object is folded in (canonicalized), so the caller can
// add any environment fingerprint that changes findings — the pinned tool
// versions plus the Go build environment (GOOS/GOARCH/build tags/workspace mode),
// which select which build-constrained files golangci-lint analyzes. Omitting
// the build env would let a host-default stamp be reused for a different-GOOS
// `make lint`, reporting `(cached)` with a different real file set.
export function moduleLintKey({ treeHashes, moduleDir, deps, configHash, versions }) {
  const depTrees = {}
  for (const dep of [...deps].sort()) {
    depTrees[dep] = treeHashes[dep]
  }
  const payload = {
    moduleDir,
    moduleTree: treeHashes[moduleDir],
    depTrees,
    configHash,
    versions,
  }
  return sha256Hex(JSON.stringify(canonicalize(payload)))
}

// Hash a directory's git-tracked content plus a dirty/untracked working-tree
// overlay. `dir` is repo-relative (POSIX). Returns a hex sha256.
export function hashTree(repoRoot, dir) {
  const git = (args) =>
    execFileSync('git', args, { cwd: repoRoot, encoding: 'utf8', maxBuffer: 1024 * 1024 * 256 })

  // Tracked files: mode + object id rows. Fast; no content reads.
  const tracked = git(['ls-files', '-s', '--', dir])

  const hash = createHash('sha256')
  hash.update('tracked\n')
  hash.update(tracked)

  // Dirty/untracked overlay: for every path git reports as changed under the
  // dir, fold in its current working-tree content (or a tombstone if deleted).
  // Two flags are load-bearing, both to avoid under-invalidation (a module
  // reporting `(cached)` while its real findings changed):
  //   --untracked-files=all — the default `-unormal` collapses a fully-untracked
  //     subdirectory into a single `?? dir/` entry, whose path is a directory;
  //     readFileSync would throw and tombstone it, folding NONE of the new `.go`
  //     files inside. golangci-lint DOES lint untracked `.go` files. `-uall`
  //     expands untracked dirs to individual files so each file's content hashes.
  //   -z — NUL-delimited, raw (never-quoted) paths. The default output
  //     octal-escapes non-ASCII names under core.quotePath (git's default), which
  //     would resolve to a wrong path and tombstone a real file. `-z` also frees
  //     us from parsing the `orig -> new` rename spelling.
  // Rename/copy records carry a second NUL field (the original path); we hash the
  // destination's working-tree content, so we skip that trailing field.
  const status = git(['status', '--porcelain', '-z', '--untracked-files=all', '--', dir])
  const records = status.split('\0')
  const overlayPaths = []
  for (let i = 0; i < records.length; i++) {
    const rec = records[i]
    if (!rec) continue
    // Porcelain v1: XY<space>path (path starts at index 3).
    overlayPaths.push(rec.slice(3))
    const xy = rec.slice(0, 2)
    if (xy[0] === 'R' || xy[0] === 'C' || xy[1] === 'R' || xy[1] === 'C') i++
  }

  hash.update('\noverlay\n')
  for (const filePath of overlayPaths.sort()) {
    hash.update(filePath)
    hash.update('\0')
    const abs = path.join(repoRoot, filePath)
    try {
      hash.update(fs.readFileSync(abs))
    } catch {
      // Deleted in the working tree (staged deletion / removed): tombstone.
      hash.update('\0deleted\0')
    }
    hash.update('\0')
  }

  return hash.digest('hex')
}
