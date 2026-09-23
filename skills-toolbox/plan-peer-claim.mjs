// plan-peer-claim.mjs
//
// Is another run already claiming this ticket, right now?
//
// A peer that selected the same ticket sixty seconds ago has made ZERO tracker
// mutations: the ticket still reads unplanned, still carries its queue label, and
// still has no plan attachment, so every tracker-sourced rule correctly answers
// "proceed" while a peer is mid-drafting. The one signal that exists inside that
// window is local: the peer's run scratch.
//
// THE DISCRIMINATING RULE. A peer is claiming this ticket iff there is a run
// scratch directory, OTHER THAN THIS RUN'S OWN, that (a) holds an entry whose name
// carries this issue id, and (b) was touched inside a liveness window much shorter
// than the scratch reap TTL.
//
// PRESENCE ALONE IS NOT A CLAIM, and that is the whole point. plan-scratch-reap.mjs
// keeps scratch for 24h, so a run that aborted before its own cleanup leaves its
// plan, description and draft metadata on disk for a day afterwards. A presence-only match
// calls every one of those a live peer — a standing false positive that would stall
// the queue permanently. Recency is the discriminator.
//
// FAILING OPEN IS DELIBERATE. An absent root, an unreadable directory, a missing
// issue id: every one of them answers `proceed` with a recorded reason. A detector
// that cannot read must not be able to refuse work, because a stalled queue is a
// worse failure than the duplicate dispatch this exists to prevent.
//
// The scratch shape is IMPORTED, never spelled here (see plan-scratch-paths.mjs):
// a relocation of the scratch contract must move this detector with it rather than
// silently blind it, which is exactly how the recorded top-level recipe rotted.
//
// Node built-ins only — cron worktrees are dependency-free. Mirrors the shape of
// the sibling helpers (plan-scratch-reap.mjs, bs-run-sentinel.mjs): a pure core, a
// thin IO wrapper, and a CLI that prints one JSON line.

import { readdirSync, statSync } from 'node:fs'
import { basename, join } from 'node:path'
import { isMainModule } from './main-module.mjs'
import { PLAN_SCRATCH_ROOT, RUN_SCRATCH_DIR_PREFIX } from './plan-scratch-paths.mjs'

/**
 * How recently a run scratch directory must have been touched to count as a live
 * peer. Well under plan-scratch-reap.mjs's 24h TTL — a stale directory has to fall
 * outside this window long before it is reaped, or presence would be the signal
 * again — and comfortably longer than one drafting dispatch, so a peer that is
 * merely thinking between scratch writes is not mistaken for a crashed one.
 */
export const PEER_CLAIM_LIVENESS_MS = 45 * 60 * 1000

/** Test/operator override for the window above. */
export const PEER_CLAIM_LIVENESS_ENV = 'BOSS_PLAN_PEER_CLAIM_LIVENESS_MS'

/**
 * Resolve the liveness window: an explicit argument wins, then the env override,
 * then the default. A non-numeric or negative override is ignored rather than
 * honoured — a typo must not silently widen or close the window.
 * @param {number|undefined} explicit
 * @param {Record<string, string|undefined>} [env]
 * @returns {number}
 */
export function peerClaimLivenessMs(explicit, env = process.env) {
  if (typeof explicit === 'number' && Number.isFinite(explicit) && explicit >= 0) return explicit
  const raw = env?.[PEER_CLAIM_LIVENESS_ENV]
  if (raw !== undefined && raw !== '') {
    const parsed = Number(raw)
    if (Number.isFinite(parsed) && parsed >= 0) return parsed
  }
  return PEER_CLAIM_LIVENESS_MS
}

/** A run scratch directory, however the caller spelled it: path, or bare name. */
function runDirName(dir) {
  const raw = String(dir ?? '')
    .trim()
    .replace(/[/\\]+$/, '')
  return raw ? basename(raw) : ''
}

/**
 * Match the issue id as a whole token inside an entry name.
 *
 * `BOS-12` must not match `BOS-1283-slug.md`: a strict substring of a different
 * ticket id is a different ticket, and treating it as a claim would defer against
 * an unrelated peer. Boundaries are alphanumeric-only, because `-` and `.` are both
 * separators INSIDE a declared scratch basename (`<ISSUE-ID>-<slug>.md`).
 * @param {string} issueId
 * @returns {(name: string) => boolean}
 */
export function issueIdMatcher(issueId) {
  const escaped = String(issueId).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const pattern = new RegExp(`(?<![A-Za-z0-9])${escaped}(?![A-Za-z0-9])`)
  return (name) => pattern.test(String(name ?? ''))
}

/**
 * @typedef {object} PeerClaimEntry
 * @property {string} dir      the run scratch directory, as a path or a bare name
 * @property {string[]} names  its entry names (one level down — the depth the ids live at)
 * @property {number} mtimeMs  when it was last touched
 */

/**
 * @typedef {object} PeerClaimVerdict
 * @property {'proceed'|'defer'} action
 * @property {{dir: string, entry: string, ageMs: number}[]} peers
 * @property {string[]} reasons
 */

/**
 * The pure verdict, over an injected listing. No clock, no filesystem: stale-vs-live,
 * self-vs-peer and depth are all decided from the argument, which is what lets the
 * matrix be exercised deterministically.
 * @param {{issueId?: string, selfRunDir?: string, entries?: PeerClaimEntry[],
 *          now?: number, livenessMs?: number}} [input]
 * @returns {PeerClaimVerdict}
 */
export function peerClaimVerdict(input = {}) {
  const { issueId, selfRunDir, entries, now: rawNow, livenessMs } = input
  // `now` is validated the way `livenessMs` is, and for the same reason: a non-finite
  // clock makes `ageMs` NaN, `NaN >= windowMs` false, and every id-matching directory a
  // live claim — recency bypassed and the detector failing CLOSED, which is the one
  // direction this module forbids. An unusable clock reads as "now", never as "forever".
  const now = Number.isFinite(rawNow) ? rawNow : Date.now()
  const windowMs = peerClaimLivenessMs(livenessMs)
  const reasons = []
  /** @type {{dir: string, entry: string, ageMs: number}[]} */
  const peers = []

  const id = String(issueId ?? '').trim()
  if (!id) {
    reasons.push('proceed: no issue id supplied, so no claim can be matched')
    return { action: 'proceed', peers, reasons }
  }
  if (!Array.isArray(entries)) {
    reasons.push(`proceed: no readable run-scratch listing for ${id} — failing open`)
    return { action: 'proceed', peers, reasons }
  }

  const self = runDirName(selfRunDir)
  if (!self) {
    reasons.push('no own run-scratch directory was named, so no directory is excluded by identity')
  }
  const matches = issueIdMatcher(id)

  for (const entry of entries) {
    const dir = String(entry?.dir ?? '')
    const name = runDirName(dir)
    if (!name) continue
    // Only run-scoped directories carry a run identity, so only they can be
    // attributed to a peer. Anything else at the scratch root is unowned residue.
    if (!name.startsWith(RUN_SCRATCH_DIR_PREFIX)) {
      reasons.push(`ignored ${dir}: not a ${RUN_SCRATCH_DIR_PREFIX}* run scratch directory`)
      continue
    }
    // R2 — excluded by IDENTITY, before recency is even consulted. A detector that
    // can defer to itself deadlocks the queue permanently, which is strictly worse
    // than having no detector at all.
    if (self && name === self) {
      reasons.push(`ignored ${dir}: this run's own scratch directory`)
      continue
    }
    const names = Array.isArray(entry?.names) ? entry.names : []
    const hit = names.map(String).filter(matches).sort()[0]
    if (hit === undefined) continue
    const mtimeMs = Number(entry?.mtimeMs)
    if (!Number.isFinite(mtimeMs)) {
      reasons.push(`ignored ${dir}: holds ${hit} but reports no readable mtime`)
      continue
    }
    // A future mtime (clock skew, a copied tree) reads as just-touched rather than
    // as a negative age, so skew can only make the detector more cautious.
    const ageMs = Math.max(0, now - mtimeMs)
    if (ageMs >= windowMs) {
      reasons.push(
        `ignored ${dir}: holds ${hit} but was last touched ${ageMs}ms ago, ` +
          `past the ${windowMs}ms liveness window`,
      )
      continue
    }
    peers.push({ dir, entry: hit, ageMs })
    reasons.push(`peer claim: ${dir} holds ${hit}, touched ${ageMs}ms ago`)
  }

  if (peers.length === 0) {
    reasons.push(`proceed: no live peer scratch claims ${id}`)
    return { action: 'proceed', peers, reasons }
  }
  return { action: 'defer', peers, reasons }
}

/**
 * Read the run scratch directories and delegate to the pure verdict.
 *
 * Only `<root>/<run dir>/` children are listed, because that is the depth the ids
 * live at: no plan basename is written at the root any more, so a root-only scan
 * sees nothing. An unreadable ROOT is swallowed into a reason; an unreadable, symlinked
 * or non-directory `run-*` entry is skipped silently. Both fail open — R5.
 * @param {{issueId?: string, selfRunDir?: string, root?: string,
 *          livenessMs?: number, now?: number}} [input]
 * @returns {PeerClaimVerdict}
 */
export function scanPeerClaims(input = {}) {
  const { issueId, selfRunDir, root = PLAN_SCRATCH_ROOT, livenessMs, now = Date.now() } = input
  let dirents
  try {
    dirents = readdirSync(root, { withFileTypes: true })
  } catch (err) {
    // An absent root is the ordinary quiet case and an unreadable one is a fault,
    // but both answer the same way. The reason is what tells a reader the scan saw
    // nothing because it could not look, rather than because the queue was quiet.
    const verdict = peerClaimVerdict({ issueId, selfRunDir, entries: [], now, livenessMs })
    verdict.reasons.unshift(`run-scratch root ${root} is unreadable (${err.message})`)
    return verdict
  }
  const entries = []
  for (const dirent of dirents) {
    if (!dirent.isDirectory()) continue
    if (!dirent.name.startsWith(RUN_SCRATCH_DIR_PREFIX)) continue
    const dir = join(root, dirent.name)
    let names = []
    let mtimeMs = Number.NaN
    try {
      names = readdirSync(dir)
      // The newest of the directory's own mtime and its children's. A directory's
      // mtime only moves when an entry is created or removed, so a peer that is
      // rewriting one artifact in place would otherwise age out mid-flight.
      mtimeMs = statSync(dir).mtimeMs
      for (const name of names) {
        try {
          mtimeMs = Math.max(mtimeMs, statSync(join(dir, name)).mtimeMs)
        } catch {
          // An entry that vanished between readdir and stat is a live peer's
          // cleanup, not a reason to abandon the directory.
        }
      }
    } catch {
      continue
    }
    entries.push({ dir, names, mtimeMs })
  }
  return peerClaimVerdict({ issueId, selfRunDir, entries, now, livenessMs })
}

const USAGE = [
  'usage: plan-peer-claim.mjs scan <ISSUE-ID> [--self <run-dir>] [--root <dir>]',
  '                                          [--liveness <ms>] [--now <epoch-ms>]',
  '',
  'Prints one JSON line: {"action":"proceed"|"defer","peers":[…],"reasons":[…]}.',
  'EXITS 0 FOR BOTH ANSWERS — a verdict is not a refusal, and a caller that read a',
  'non-zero exit as "the check failed" would route a deferral into its error path.',
  `The liveness window defaults to ${PEER_CLAIM_LIVENESS_MS}ms and is overridable`,
  `with --liveness or ${PEER_CLAIM_LIVENESS_ENV}.`,
].join('\n')

if (isMainModule(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2)
  if (cmd === 'scan') {
    const flags = {}
    const positional = []
    for (let i = 0; i < rest.length; i += 1) {
      const token = rest[i]
      if (token.startsWith('--')) {
        const value = rest[i + 1]
        // A value-less flag must not read as "not supplied": a blank --self silently
        // disables identity exclusion, which is the permanent self-defer deadlock.
        if (value === undefined || value === '' || value.startsWith('--')) {
          process.stderr.write(`plan-peer-claim.mjs: ${token} requires a value\n${USAGE}\n`)
          process.exit(2)
        }
        flags[token.slice(2)] = value
        i += 1
      } else {
        positional.push(token)
      }
    }
    const verdict = scanPeerClaims({
      issueId: positional[0],
      selfRunDir: flags.self,
      root: flags.root,
      livenessMs: flags.liveness === undefined ? undefined : Number(flags.liveness),
      // A non-numeric or empty --now must fall back to the real clock, never through as
      // NaN: a NaN age compares false against the window, which would bypass recency and
      // defer against every id-matching directory — the fail-CLOSED direction.
      now: Number.isFinite(Number(flags.now)) && flags.now !== '' ? Number(flags.now) : undefined,
    })
    process.stdout.write(`${JSON.stringify(verdict)}\n`)
    process.exit(0)
  } else if (cmd === undefined || cmd === 'help' || cmd === '--help' || cmd === '-h') {
    process.stdout.write(`${USAGE}\n`)
    process.exit(0)
  } else {
    process.stderr.write(`${USAGE}\n`)
    process.exit(2)
  }
}
