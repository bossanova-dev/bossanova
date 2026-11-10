// The core's proof page: after a terminal verdict, build the head's boss-proof/1 record from the
// judge output, render it, upload `index.html` under `<owner>/<repo>/<pr>/<head>/` and leave one
// proof note per head carrying the comment block and the proof line the daemon reads.
//
// Publisher, first that applies: the trusted base tree's `proof-publish` hook (never the PR head's),
// else the daemon's UploadEvidence when the base config sets `publishConfig.proof` (the daemon
// refuses the page unless its destination enables pages), else none and `page` stays null.
//
// A repo's own proof wins. When any PR comment other than a proof note already carries a valid
// boss-proof line for the head from an author with write access (a repo evidence extension's
// comment), the core records nothing for that head: its note would be the newer comment and
// replace the richer record as the line the daemon reads. An unreadable comment list also records
// nothing. Nothing here throws, changes a verdict or keeps a hook's output.
import { spawn as nodeSpawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import {
  closeSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  rmSync,
  rmdirSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { scanText } from '../plan/plan-secret-scan.mjs'
import { loadSkillConfig, publishConfigFor } from '../skill-config.mjs'
import { discoverExtensions } from '../skill-extensions.mjs'
import { daemonSocket, daemonUnary } from './verify-daemon-rpc.mjs'
import { afterMergeItems } from './verify-park-note.mjs'
import { PROOF_VERDICTS, buildProofRecord } from './verify-proof-build.mjs'
import {
  MAX_AFTER_MERGE,
  MAX_TIMELINE,
  renderProofComment,
  renderProofHtml,
  validatePageInput,
} from './verify-proof-page.mjs'
import {
  MAX_TEXT_CHARS,
  parseProofLine,
  proofLine,
  validateProof,
  withDigest,
} from './verify-proof.mjs'
import {
  PAGE_OBJECT,
  PUBLISH_ROLE,
  publishContentType,
  publishPrefix,
  runProofPublish,
} from './verify-publish-hook.mjs'

// One small page, not a recording: the hook gets far less than the media budget.
export const PROOF_HOOK_TIMEOUT_MS = 2 * 60 * 1000
export const PROOF_NOTE_MARKER =
  /^<!-- boss-proof-note head=([0-9a-fA-F]{40}) digest=([0-9a-f]{16}) -->$/
/** @param {string} headSha @param {string} digest */
export const proofNoteMarker = (headSha, digest) =>
  `<!-- boss-proof-note head=${headSha} digest=${digest} -->`

// An environment value shorter than this is too common to treat as a credential by exact match.
const MIN_SECRET_LENGTH = 8
const SECRET_NAME =
  /(TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|ACCESS_KEY|PRIVATE_KEY|CREDENTIAL|_JWT$|_DSN$)/i

/**
 * Credential hits in `texts`: token shapes, plus the exact value (8+ characters) of every
 * credential-named variable in `env`. `[{line, kind}]`; a value is never returned.
 * @param {string[]} texts @param {Record<string, string | undefined>} env
 */
export function findPageSecrets(texts, env = process.env) {
  const values = Object.entries(env ?? {}).filter(
    ([key, value]) =>
      SECRET_NAME.test(key) && typeof value === 'string' && value.length >= MIN_SECRET_LENGTH,
  )
  const hits = []
  for (const text of texts) {
    for (const hit of scanText(text)) hits.push({ line: hit.line, kind: hit.kind })
    for (const [key, value] of values) {
      const at = text.indexOf(value)
      if (at >= 0) hits.push({ line: text.slice(0, at).split('\n').length, kind: `env:${key}` })
    }
  }
  return hits
}

/**
 * The publisher for a page on `baseSha`: the trusted tree's first `proof-publish` hook, else the
 * daemon when that tree's config sets `publishConfig.proof`, else null. Only the base tree is read,
 * so a PR can neither supply a publisher nor switch uploads on. Never throws.
 */
export function discoverProofPublisher({
  baseSha,
  cwd,
  read,
  trustedTree,
  discover = discoverExtensions,
  loadConfig = (root) => loadSkillConfig({ cwd: root }),
}) {
  try {
    const trusted = trustedTree({ baseSha, cwd, read })
    if (!trusted?.ok) return { publisher: null, reason: 'trusted-tree-unavailable' }
    const { extensions } = discover({ core: 'boss-verify', role: PUBLISH_ROLE, root: trusted.root })
    if (extensions?.[0]) return { publisher: { kind: 'hook', extension: extensions[0] } }
    let config = {}
    try {
      config = loadConfig(trusted.root) ?? {}
    } catch {
      /* An unreadable base config configures no upload. */
    }
    return publishConfigFor(config, 'proof')
      ? { publisher: { kind: 'daemon' } }
      : { publisher: null, reason: 'not-configured' }
  } catch {
    return { publisher: null, reason: 'publisher-discovery-failed' }
  }
}

const DAEMON_REASONS = {
  unimplemented: 'daemon-unsupported',
  failed_precondition: 'page-refused',
  permission_denied: 'upload-refused',
  invalid_argument: 'upload-refused',
  unavailable: 'upload-failed',
}

/**
 * Upload `html` as `index.html` for `target` (`{repo, pr, headSha}`) through `publisher`:
 * `{ok: true, url}` or `{ok: false, reason}`. A hook gets a private temporary directory; the
 * daemon accepts only `<workspace>/.verify-evidence/<run>`, removed afterwards. Never throws.
 */
export async function uploadProofPage({
  publisher,
  html,
  target,
  cwd,
  workspaceRoot,
  env = process.env,
  runHook = runProofPublish,
  callDaemon = daemonUnary,
  hookTimeoutMs = PROOF_HOOK_TIMEOUT_MS,
}) {
  const prefix = publishPrefix(target)
  if (!prefix) return { ok: false, reason: 'no-prefix' }
  let dir = null
  try {
    if (publisher.kind === 'hook') {
      dir = mkdtempSync(join(tmpdir(), 'boss-verify-proof-'))
      writeFileSync(join(dir, PAGE_OBJECT), html)
      const outcome = runHook({
        extension: publisher.extension,
        envelope: {
          repo: target.repo,
          pr: target.pr,
          headSha: target.headSha,
          prefix,
          evidenceDir: dir,
          objects: [
            { name: PAGE_OBJECT, path: PAGE_OBJECT, contentType: publishContentType(PAGE_OBJECT) },
          ],
        },
        cwd,
        env,
        timeoutMs: hookTimeoutMs,
      })
      if (!outcome.ok)
        return {
          ok: false,
          reason: `hook-failed:${/** @type {{reason: string}} */ (outcome).reason}`,
        }
      return { ok: true, url: `${outcome.baseUrl}/${prefix}${PAGE_OBJECT}` }
    }
    if (!workspaceRoot) return { ok: false, reason: 'no-workspace' }
    dir = join(workspaceRoot, '.verify-evidence', `proof-${target.pr}-${target.headSha}`)
    mkdirSync(dir, { recursive: true })
    writeFileSync(join(dir, PAGE_OBJECT), html)
    let result
    try {
      result = await callDaemon(
        daemonSocket(env),
        'UploadEvidence',
        {
          evidenceDir: dir,
          objects: [{ name: PAGE_OBJECT, path: PAGE_OBJECT }],
          prNumber: target.pr,
          headSha: target.headSha,
        },
        { timeoutMs: hookTimeoutMs },
      )
    } catch (error) {
      const code = /** @type {{code?: string}} */ (error)?.code
      return {
        ok: false,
        reason:
          DAEMON_REASONS[/** @type {keyof typeof DAEMON_REASONS} */ (code)] ?? 'daemon-unavailable',
      }
    }
    // The daemon derives the slug from the workspace's origin; the page is keyed by this PR head.
    const keyed = new RegExp(`^[^/]+/[^/]+/${target.pr}/${target.headSha}/$`)
    if (!keyed.test(String(result?.keyPrefix ?? '')))
      return { ok: false, reason: 'page-url-mismatch' }
    const base = String(result.publicBaseUrl ?? '').replace(/\/+$/, '')
    return { ok: true, url: `${base}/${result.keyPrefix}${PAGE_OBJECT}` }
  } catch {
    return { ok: false, reason: 'upload-failed' }
  } finally {
    if (dir) {
      try {
        rmSync(dir, { recursive: true, force: true })
        if (publisher.kind !== 'hook') rmdirSync(join(String(workspaceRoot), '.verify-evidence'))
      } catch {
        /* A non-empty evidence directory belongs to another run. */
      }
    }
  }
}

/**
 * The page sidecar: diff excerpts for asking areas, the commit timeline, the PR body's after-merge
 * items and the files link, plus the park verdict for a human verdict. A part git cannot read is
 * left out; a sidecar the renderer would refuse falls back to the bare record.
 */
export function proofPageInput({ proof, areas, judged, git, prBody, repo, pr }) {
  const run = (args) => {
    try {
      const out = git(args)
      return out?.ok ? String(out.output ?? out.stdout ?? '') : null
    } catch {
      return null
    }
  }
  /** @type {Record<string, {diff: string}>} */
  const asks = {}
  for (const area of areas) {
    if (!area.ask || !area.paths.length) continue
    const diff = run([
      'diff',
      '--no-color',
      '--no-ext-diff',
      proof.base,
      proof.head,
      '--',
      ...area.paths,
    ])
    if (diff) asks[area.name] = { diff }
  }
  const log = run([
    'log',
    `--max-count=${MAX_TIMELINE}`,
    '--format=%cI%x09%s',
    `${proof.base}..${proof.head}`,
  ])
  const timeline = (log ?? '')
    .split('\n')
    .filter((line) => line.includes('\t'))
    .slice(0, MAX_TIMELINE)
    .map((line) => {
      const tab = line.indexOf('\t')
      return { at: line.slice(0, tab), text: line.slice(tab + 1).slice(0, MAX_TEXT_CHARS) || '-' }
    })
  const afterMerge = afterMergeItems(String(prBody ?? ''))
    .slice(0, MAX_AFTER_MERGE)
    .map((item) => item.replaceAll('&lt;!--', '<!--').slice(0, MAX_TEXT_CHARS))
  const codes = Array.isArray(judged.reasons)
    ? judged.reasons.filter((c) => typeof c === 'string')
    : []
  const input = {
    proof,
    ...(Object.keys(asks).length ? { asks } : {}),
    ...(timeline.length ? { timeline } : {}),
    ...(afterMerge.length ? { afterMerge } : {}),
    filesUrl: `https://github.com/${repo}/pull/${pr}/files`,
    ...(judged.verdict === 'human' && codes.length
      ? {
          verdict: {
            codes,
            ...(typeof judged.label === 'string' && judged.label
              ? { label: judged.label.slice(0, MAX_TEXT_CHARS) }
              : {}),
          },
        }
      : {}),
  }
  return validatePageInput(input).ok ? input : { proof }
}

/**
 * The proof note for `headSha`: the comment block (or one line when there is none), the proof
 * line, and the note marker. `{body, digest}`; the digest covers the content, so an unchanged
 * note is never rewritten.
 */
export function proofNote({ headSha, proof, block }) {
  const lead =
    block ??
    `Verify's proof for \`${headSha.slice(0, 12)}\` is **${proof.confidence.level}**${proof.page ? `: ${proof.page}` : '.'}`
  const content = `${lead}\n\n${proofLine(proof)}`
  const digest = createHash('sha256').update(content).digest('hex').slice(0, 16)
  return { body: `${content}\n\n${proofNoteMarker(headSha, digest)}`, digest }
}

/**
 * Build, publish and record the proof for `headSha` from the judge output `judged`. `actions`
 * carries the gate's comment helpers (`readOperatorMarkers`, `recordHeadNote`) and its
 * `trustedTree`; the rest are injectable for tests. Returns `{recorded, reason?, level?, note?,
 * page?}` and never throws: no outcome here changes the verdict it follows.
 */
export async function recordHeadProof(
  {
    read,
    repo,
    pr,
    headSha,
    dryRun = false,
    writes = [],
    judged,
    intent = null,
    prBody = '',
    cwd,
    env = process.env,
  },
  actions,
) {
  try {
    if (judged === null || typeof judged !== 'object')
      return { recorded: false, reason: 'judge-unreadable' }
    if (String(judged.headSha ?? '').toLowerCase() !== headSha)
      return { recorded: false, reason: 'judge-other-head' }
    const built = buildProofRecord({ judged, intent, repo, pr })
    if (!built.ok) return { recorded: false, reason: built.reason }
    const markers = actions.readOperatorMarkers(read, repo, pr)
    if (!markers.readable) return { recorded: false, reason: 'comments-unreadable' }
    if (markers.markers.some((m) => m.kind === 'repo-proof' && m.head === headSha))
      return { recorded: false, reason: 'repo-proof-present' }
    const git = (args) => read('git', args, { raw: true })
    const { proof: record, areas } = /** @type {{proof: any, areas: any[]}} */ (built)
    const input = proofPageInput({ proof: record, areas, judged, git, prBody, repo, pr })
    let html = null
    /** @type {{published: boolean, reason?: string, url?: string, publisher?: string}} */
    let page = { published: false, reason: 'render-failed' }
    try {
      html = renderProofHtml(input)
    } catch {
      /* The note still records the level. */
    }
    let block = null
    try {
      block = renderProofComment(input)
    } catch {
      /* Over the comment budget: the note carries one line. */
    }
    const secret = findPageSecrets([html ?? '', block ?? ''], env).length > 0
    if (secret) {
      html = block = null
      page = { published: false, reason: 'page-contains-secret' }
    }
    let proof = record
    // A standing note that already says exactly this, page included, needs no second upload.
    const standing = markers.markers
      .filter((m) => m.kind === 'proof-note' && m.head === headSha && m.page)
      .sort((a, b) => b.at - a.at || b.id - a.id)[0]
    if (html !== null && standing) {
      const again = withDigest({ ...record, page: standing.page })
      let againBlock = null
      try {
        againBlock = renderProofComment({ ...input, proof: again })
      } catch {
        /* Compared as the one-line note. */
      }
      if (
        validateProof(again).ok &&
        proofNote({ headSha, proof: again, block: againBlock }).digest === standing.digest
      )
        return {
          recorded: true,
          level: again.confidence.level,
          note: 'current',
          page: { published: true, url: standing.page, reused: true },
        }
    }
    if (html !== null) {
      if (dryRun) page = { published: false, reason: 'dry-run' }
      else {
        const found = (actions.discoverPublisher ?? discoverProofPublisher)({
          baseSha: proof.base,
          cwd,
          read,
          trustedTree: actions.trustedTree,
          ...(actions.discover ? { discover: actions.discover } : {}),
          ...(actions.loadConfig ? { loadConfig: actions.loadConfig } : {}),
        })
        if (!found.publisher) page = { published: false, reason: found.reason }
        else {
          const top = read('git', ['rev-parse', '--show-toplevel'])
          const uploaded = await (actions.uploadPage ?? uploadProofPage)({
            publisher: found.publisher,
            html,
            target: { repo, pr, headSha },
            cwd,
            workspaceRoot: top?.ok ? top.output : null,
            env,
            ...(actions.runHook ? { runHook: actions.runHook } : {}),
            ...(actions.callDaemon ? { callDaemon: actions.callDaemon } : {}),
          })
          const candidate = uploaded.ok ? withDigest({ ...proof, page: uploaded.url }) : null
          if (candidate && validateProof(candidate).ok) {
            proof = candidate
            page = { published: true, url: uploaded.url, publisher: found.publisher.kind }
            try {
              block = renderProofComment({ ...input, proof })
            } catch {
              block = null
            }
          } else
            page = { published: false, reason: uploaded.ok ? 'invalid-page-url' : uploaded.reason }
        }
      }
    }
    const note = proofNote({ headSha, proof, block })
    const noted = actions.recordHeadNote(read, {
      kind: 'proof-note',
      repo,
      pr,
      headSha,
      note,
      markers,
      dryRun,
      writes,
    })
    return {
      recorded: ['posted', 'updated', 'current', 'planned'].includes(noted.outcome),
      level: proof.confidence.level,
      note: noted.outcome,
      page,
    }
  } catch {
    return { recorded: false, reason: 'proof-failed' }
  }
}

/** @param {string} file */
function readJson(file) {
  try {
    return JSON.parse(readFileSync(file, 'utf8'))
  } catch {
    return null
  }
}

/**
 * The `post` flags that ask for a proof, as `{proof}`: `--judge <file>` (the judge output the
 * verdict came from), `--intent <file>` (a verify-intent record) and `--proof-detach`. `{}` when
 * neither file is given; a usage error on a claim or wait. An unreadable file is no usage error:
 * the proof is then skipped, not the post.
 * @param {{judge?: string, intent?: string, proofDetach?: boolean}} options @param {string} verdict
 * @param {new (message: string) => Error} UsageError
 */
export function parseProofOptions(options, verdict, UsageError) {
  if (options.judge === undefined && options.intent === undefined) return {}
  if (!PROOF_VERDICTS.includes(verdict) || options.judge === undefined)
    throw new UsageError('verify-gate: --judge [--intent] take a pass|blocked|defect|human post')
  const proof = {
    judged: readJson(options.judge),
    intent: options.intent === undefined ? null : readJson(options.intent),
    detach: options.proofDetach === true,
    files: { judge: options.judge, intent: options.intent },
  }
  return { proof }
}

const GATE = fileURLToPath(new URL('./verify-gate.mjs', import.meta.url))

/**
 * Start `verify-gate.mjs proof` for the head in its own process group, its JSON line appended to
 * `boss-verify-proof-<pr>-<head>.log` under `logDir`: `{detached: true, log}`, or a skipped
 * proof when it cannot start. The proof is the head's own idempotent note, so nothing waits on it.
 */
export function spawnProof({
  pr,
  headSha,
  repo,
  files,
  cwd,
  env,
  spawn = nodeSpawn,
  logDir = tmpdir(),
}) {
  try {
    const log = join(logDir, `boss-verify-proof-${pr}-${headSha}.log`)
    const fd = openSync(log, 'a')
    try {
      const args = ['proof', '--pr', String(pr), '--head', headSha, '--judge', String(files.judge)]
      if (files.intent) args.push('--intent', String(files.intent))
      if (repo) args.push('--repo', repo)
      const child = spawn(process.execPath, [GATE, ...args], {
        cwd,
        env,
        detached: true,
        stdio: ['ignore', fd, fd],
      })
      child.unref()
      return { detached: true, log }
    } finally {
      closeSync(fd)
    }
  } catch {
    return { recorded: false, reason: 'spawn-failed' }
  }
}

/**
 * `result` with the head's `proof` outcome once the verdict it carries has landed (posted, or
 * already standing). The proof runs after every verdict write, so it can only add to the result.
 */
export async function proofAfterPost(ctx, proof, result, actions) {
  if (!proof || !(result.posted || result.skipped === 'already-posted')) return result
  if (proof.detach && !ctx.dryRun && proof.judged)
    return {
      ...result,
      proof: (actions.spawnProof ?? spawnProof)({
        cwd: actions.cwd,
        env: actions.env,
        ...ctx,
        ...proof,
      }),
    }
  const outcome = proof.judged
    ? await recordHeadProof({ cwd: actions.cwd, env: actions.env, ...ctx, ...proof }, actions)
    : { recorded: false, reason: 'judge-unreadable' }
  return { ...result, proof: outcome }
}

/**
 * The `proof` verb: record the proof for `--head` from a saved judge output after its verdict was
 * posted elsewhere (the sweep runs it detached, off its post budget). Never writes a status.
 */
export async function proofVerb(options, deps, actions) {
  const { UsageError, writeAwareReader, commandReader, repoFor, readPr } = actions
  const headSha = String(options.head ?? '').toLowerCase()
  if (!/^[0-9a-f]{40}$/.test(headSha) || options.judge === undefined)
    throw new UsageError('verify-gate: proof needs --head <40-hex sha> and --judge <file>')
  const read = writeAwareReader(deps.read ?? commandReader(deps))
  const repo = repoFor(read, options.repo)
  const view = readPr(read, options.pr, repo)
  if (!view.ok) return { recorded: false, reason: 'pr-unreadable' }
  if (String(view.payload?.headRefOid ?? '').toLowerCase() !== headSha)
    return { recorded: false, reason: 'head-moved' }
  const writes = []
  const result = await proofAfterPost(
    {
      read,
      repo,
      pr: options.pr,
      headSha,
      dryRun: options['dry-run'] === true,
      writes,
      prBody: view.payload?.body ?? '',
    },
    parseProofOptions(options, 'pass', UsageError).proof,
    { skipped: 'already-posted' },
    actions,
  )
  return { ...result.proof, ...(options['dry-run'] === true ? { dryRun: true, writes } : {}) }
}

/**
 * The proof marker a PR comment carries, for operatorMarkers: `proof-note` (the core's own note,
 * with its digest) or `repo-proof` (a valid boss-proof line in any other comment), else null.
 * @param {string} body @param {string} last the comment's last non-empty line
 */
export function proofCommentMarker(body, last) {
  const note = last.match(PROOF_NOTE_MARKER)
  if (note)
    return {
      kind: 'proof-note',
      head: note[1],
      fields: { digest: note[2], page: parseProofLine(body)?.page ?? null },
    }
  const line = parseProofLine(body)
  return line ? { kind: 'repo-proof', head: line.head, fields: { page: line.page } } : null
}
