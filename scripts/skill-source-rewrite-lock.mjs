// Synchronize repository-owned writes to the canonical embedded skill payload.
// The lock lives under .git so checkout snapshots can observe it. Its contents
// make an abandoned legacy/expired sentinel recoverable after a hard stop.
import {
  closeSync,
  linkSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { execFileSync, spawnSync } from 'node:child_process'
import { resolve } from 'node:path'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

export const SKILL_SOURCE_REWRITE_LOCK = 'boss-skill-snapshot.lock'
export const SKILL_SOURCE_REWRITE_GENERATION = 'boss-skill-snapshot.generation'
export const SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX = 'dirty:'
export const SKILL_SOURCE_REWRITE_LOCK_MAX_AGE_MS = 5 * 60 * 1000

const LOCK_RETRY_WAIT_MS = 25
const RECLAIM_OWNER_FILE = 'owner.json'
const RECLAIM_RECOVERY_FILE = '.recovering'
const lockRetryWaiter = new Int32Array(new SharedArrayBuffer(Int32Array.BYTES_PER_ELEMENT))

function gitPath(repoRoot, name) {
  const gitPath = execFileSync('git', ['rev-parse', '--git-path', name], {
    cwd: repoRoot,
    encoding: 'utf8',
  }).trim()
  return resolve(repoRoot, gitPath)
}

function lockPath(repoRoot) {
  return gitPath(repoRoot, SKILL_SOURCE_REWRITE_LOCK)
}

function rewriteGenerationPath(repoRoot) {
  return gitPath(repoRoot, SKILL_SOURCE_REWRITE_GENERATION)
}

function replaceRewriteGeneration(repoRoot, generation) {
  const path = rewriteGenerationPath(repoRoot)
  const pendingPath = `${path}.${process.pid}-${Date.now()}-${Math.random()}.pending`
  try {
    writeFileSync(pendingPath, `${generation}\n`, { mode: 0o600, flag: 'wx' })
    renameSync(pendingPath, path)
  } finally {
    rmSync(pendingPath, { force: true })
  }
}

function rewriteGenerationIsDirty(repoRoot) {
  try {
    return readFileSync(rewriteGenerationPath(repoRoot), 'utf8')
      .trim()
      .startsWith(SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX)
  } catch (err) {
    if (err?.code === 'ENOENT') return false
    throw err
  }
}

function markRewriteGenerationDirty(repoRoot) {
  replaceRewriteGeneration(repoRoot, `${SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX}${Date.now()}`)
}

function recordRewriteGeneration(repoRoot, token, repairsPayload) {
  // A dirty marker means an earlier rewrite may have stopped midway through a
  // multi-file payload. A subset writer cannot prove that payload coherent;
  // only an explicitly declared whole-payload repair may clear the marker.
  if (!repairsPayload && rewriteGenerationIsDirty(repoRoot)) return
  replaceRewriteGeneration(repoRoot, token)
}

function staleLockContents(path) {
  try {
    const contents = readFileSync(path, 'utf8')
    const metadata = JSON.parse(contents)
    if (
      !Number.isFinite(metadata.createdAt) ||
      Date.now() - metadata.createdAt > SKILL_SOURCE_REWRITE_LOCK_MAX_AGE_MS
    ) {
      if (lockOwnerAlive(metadata)) {
        return null
      }
      return contents
    }
    return null
  } catch {
    // Previous releases wrote an empty sentinel. It cannot identify a live owner,
    // so reclaim it rather than leaving every future writer and snapshot blocked.
    return ''
  }
}

function lockOwnerStartTime(pid) {
  try {
    const startTime = execFileSync('ps', ['-o', 'lstart=', '-p', String(pid)], {
      encoding: 'utf8',
    }).trim()
    return startTime || null
  } catch {
    return null
  }
}

function lockOwnerAlive(metadata) {
  if (!Number.isInteger(metadata?.pid) || metadata.pid <= 0) return false
  const startTime = lockOwnerStartTime(metadata.pid)
  if (startTime !== null) {
    return typeof metadata.ownerStartTime !== 'string' || metadata.ownerStartTime === startTime
  }
  try {
    process.kill(metadata.pid, 0)
    return true
  } catch (err) {
    // A permission or platform error is not proof that the owner is gone.
    return err?.code !== 'ESRCH'
  }
}

function lockOwnerMetadata(token) {
  const ownerStartTime = lockOwnerStartTime(process.pid)
  return {
    createdAt: Date.now(),
    pid: process.pid,
    ...(ownerStartTime === null ? {} : { ownerStartTime }),
    ...(token === undefined ? {} : { token }),
  }
}

function abandonedReclaimClaim(claimPath) {
  let observed
  try {
    observed = reclaimClaimIdentity(claimPath)
  } catch {
    return null
  }
  try {
    const metadata = JSON.parse(observed.ownerContents)
    if (
      Number.isFinite(metadata.createdAt) &&
      Date.now() - metadata.createdAt <= SKILL_SOURCE_REWRITE_LOCK_MAX_AGE_MS
    ) {
      return null
    }
    return lockOwnerAlive(metadata) ? null : observed
  } catch {
    // A hard stop can leave the claim directory behind before its owner metadata
    // is written. Its age is the only safe liveness signal in that legacy gap.
    return Date.now() - observed.info.mtimeMs > SKILL_SOURCE_REWRITE_LOCK_MAX_AGE_MS
      ? observed
      : null
  }
}

function reclaimClaimIdentity(claimPath) {
  const info = statSync(claimPath)
  let ownerContents = null
  try {
    ownerContents = readFileSync(`${claimPath}/${RECLAIM_OWNER_FILE}`, 'utf8')
  } catch {
    // A hard stop can leave a claim with no owner metadata.
  }
  return { info, ownerContents }
}

function sameReclaimClaim(observed, current) {
  return (
    sameFilesystemEntry(observed.info, current.info) &&
    observed.ownerContents === current.ownerContents
  )
}

function sameFilesystemEntry(left, right) {
  return left.dev === right.dev && left.ino === right.ino
}

function abandonedReclaimRecovery(recoveryPath) {
  let observed
  try {
    observed = reclaimRecoveryIdentity(recoveryPath)
  } catch {
    return null
  }
  try {
    const metadata = JSON.parse(observed.ownerContents)
    if (
      Number.isFinite(metadata.createdAt) &&
      Date.now() - metadata.createdAt <= SKILL_SOURCE_REWRITE_LOCK_MAX_AGE_MS
    ) {
      return null
    }
    return lockOwnerAlive(metadata) ? null : observed
  } catch {
    return Date.now() - observed.info.mtimeMs > SKILL_SOURCE_REWRITE_LOCK_MAX_AGE_MS
      ? observed
      : null
  }
}

function reclaimRecoveryIdentity(recoveryPath) {
  return { info: statSync(recoveryPath), ownerContents: readFileSync(recoveryPath, 'utf8') }
}

function sameReclaimRecovery(observed, current) {
  return (
    sameFilesystemEntry(observed.info, current.info) &&
    observed.ownerContents === current.ownerContents
  )
}

function reclaimAbandonedReclaimRecovery(recoveryPath, onBeforeReclaimRecovery) {
  const observed = abandonedReclaimRecovery(recoveryPath)
  if (!observed) return false
  let current
  try {
    current = reclaimRecoveryIdentity(recoveryPath)
  } catch (err) {
    if (err?.code === 'ENOENT') return true
    throw err
  }
  if (!sameReclaimRecovery(observed, current)) return true
  onBeforeReclaimRecovery?.()
  try {
    current = reclaimRecoveryIdentity(recoveryPath)
  } catch (err) {
    if (err?.code === 'ENOENT') return true
    throw err
  }
  if (!sameReclaimRecovery(observed, current)) return false
  // Do not unlink a stale recovery marker. A pathname unlink can remove a
  // successor published after the identity check. The caller uses this exact
  // stale marker as a fence while removing the observed enclosing claim.
  return true
}

function reclaimAbandonedReclaimClaim(claimPath, onBeforeReclaimClaim, onBeforeReclaimRecovery) {
  const observed = abandonedReclaimClaim(claimPath)
  if (!observed) return false
  onBeforeReclaimClaim?.()
  const recoveryPath = `${claimPath}/${RECLAIM_RECOVERY_FILE}`
  let recovery
  try {
    recovery = openSync(recoveryPath, 'wx', 0o600)
  } catch (err) {
    if (err?.code === 'EEXIST') {
      if (!reclaimAbandonedReclaimRecovery(recoveryPath, onBeforeReclaimRecovery)) return false
    } else if (err?.code === 'ENOENT') {
      return true
    } else {
      throw err
    }
  }
  if (recovery !== undefined) {
    try {
      writeFileSync(recovery, JSON.stringify(lockOwnerMetadata()))
    } finally {
      closeSync(recovery)
    }
  }
  const current = reclaimClaimIdentity(claimPath)
  if (!sameReclaimClaim(observed, current)) return false
  rmSync(claimPath, { recursive: true, force: true })
  return true
}

function reclaimStaleLock(repoRoot, path, contents, options) {
  const claimPath = `${path}.reclaim`
  for (;;) {
    const pendingPath = `${claimPath}.${process.pid}-${Date.now()}-${Math.random()}.pending`
    let published = false
    try {
      mkdirSync(pendingPath, { mode: 0o700 })
      writeFileSync(`${pendingPath}/${RECLAIM_OWNER_FILE}`, JSON.stringify(lockOwnerMetadata()), {
        mode: 0o600,
      })
      try {
        // Publish a fully populated directory. Go reclaimers treat a missing
        // owner as an abandoned legacy claim, so exposing one here can let a
        // second process remove this live claim.
        renameSync(pendingPath, claimPath)
        published = true
      } catch (err) {
        if (err?.code !== 'EEXIST' && err?.code !== 'ENOTEMPTY') throw err
      }
    } finally {
      rmSync(pendingPath, { recursive: true, force: true })
    }
    if (published) break
    if (
      !reclaimAbandonedReclaimClaim(
        claimPath,
        options?.onBeforeReclaimClaim,
        options?.onBeforeReclaimRecovery,
      )
    ) {
      return false
    }
  }
  try {
    options?.onClaimPublished?.(claimPath)
    // The claim serializes reclaimers. Once the exact stale sentinel is
    // revalidated, no other reclaimer can replace it before this removal.
    if (staleLockContents(path) !== contents) return false
    // Preserve evidence that the abandoned writer may have left a partial
    // payload before allowing another writer to enter.
    markRewriteGenerationDirty(repoRoot)
    rmSync(path, { force: true })
    return true
  } finally {
    rmSync(claimPath, { recursive: true, force: true })
  }
}

function acquireLock(
  repoRoot,
  {
    onBeforePublish,
    onBeforeReclaim,
    onBeforeReclaimClaim,
    onBeforeReclaimRecovery,
    onClaimPublished,
  } = {},
) {
  const path = lockPath(repoRoot)
  for (;;) {
    const token = `${process.pid}-${Date.now()}-${Math.random()}`
    const pendingPath = `${path}.${token}.pending`
    let pendingCreated = false
    let published = false
    try {
      const fd = openSync(pendingPath, 'wx', 0o600)
      pendingCreated = true
      try {
        writeFileSync(fd, JSON.stringify(lockOwnerMetadata(token)))
      } finally {
        closeSync(fd)
      }
      // A hard link creates the lock name only after its metadata is fully
      // written. Readers therefore never mistake a live, newly-created lock
      // for the empty legacy sentinel that may be reclaimed.
      onBeforePublish?.()
      try {
        linkSync(pendingPath, path)
        published = true
      } catch (err) {
        if (err?.code !== 'EEXIST') throw err
      }
    } finally {
      if (pendingCreated) rmSync(pendingPath, { force: true })
    }
    if (published) {
      return { path, token }
    }

    const staleContents = staleLockContents(path)
    if (staleContents === null) {
      Atomics.wait(lockRetryWaiter, 0, 0, LOCK_RETRY_WAIT_MS)
      continue
    }
    onBeforeReclaim?.()
    if (
      !reclaimStaleLock(repoRoot, path, staleContents, {
        onClaimPublished,
        onBeforeReclaimClaim,
        onBeforeReclaimRecovery,
      })
    ) {
      continue
    }
  }
}

function releaseLock({ path, token }) {
  try {
    if (JSON.parse(readFileSync(path, 'utf8')).token === token) rmSync(path, { force: true })
  } catch {
    // Another owner reclaimed the lock; never remove its sentinel.
  }
}

export function withSkillSourceRewriteLock(repoRoot, action, options = {}) {
  const lock = acquireLock(repoRoot, options)
  try {
    const result = action()
    recordRewriteGeneration(repoRoot, lock.token, options.repairsPayload === true)
    return result
  } catch (err) {
    // A failing action can have completed some writes. Never let a later
    // snapshot accept that state as a coherent payload.
    try {
      markRewriteGenerationDirty(repoRoot)
    } catch (markErr) {
      throw new AggregateError([err, markErr], 'record incomplete skill payload rewrite')
    }
    throw err
  } finally {
    releaseLock(lock)
  }
}

if (isMainModule(import.meta.url)) {
  const separator = process.argv.indexOf('--')
  const repoFlag = process.argv.indexOf('--repo-root')
  const repairFlags = process.argv.slice(2, separator).filter((arg) => arg === '--repairs-payload')
  if (
    repoFlag === -1 ||
    separator === -1 ||
    repoFlag + 1 >= separator ||
    separator + 1 >= process.argv.length ||
    repairFlags.length > 1
  ) {
    process.stderr.write(
      'usage: skill-source-rewrite-lock.mjs --repo-root <repo> [--repairs-payload] -- <command> [args...]\n',
    )
    process.exit(2)
  }
  const repoRoot = process.argv[repoFlag + 1]
  const repairsPayload = repairFlags.length === 1
  let result
  try {
    result = withSkillSourceRewriteLock(
      repoRoot,
      () => {
        const commandResult = spawnSync(
          process.argv[separator + 1],
          process.argv.slice(separator + 2),
          {
            cwd: repoRoot,
            env: { ...process.env, BOSS_SKILL_SOURCE_REWRITE_LOCK_HELD: '1' },
            stdio: 'inherit',
          },
        )
        if (commandResult.error || commandResult.status !== 0) {
          const error = commandResult.error ?? new Error('locked skill source rewrite failed')
          error.exitCode = commandResult.status ?? 1
          throw error
        }
        return commandResult
      },
      { repairsPayload },
    )
  } catch (err) {
    process.exit(err?.exitCode ?? 1)
  }
  process.exit(result.status ?? 1)
}
