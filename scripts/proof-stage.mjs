/**
 * proof-stage.mjs — Stage-tagged error capture for the bs-proof pipeline (BOS-138 P1c).
 *
 * A pipeline crash is captured as a `pipeline-error` — OUR bug, named by stage
 * with a stderr tail — rather than being mislabelled as an "environment
 * limitation". The two historical regressions this classification pins are:
 *   - the proof-render-terminal TDZ crash (ReferenceError at first frame), and
 *   - the ffmpeg `-loop 1` hang (an unbounded encode that never returned).
 *
 * Wiring: `proof.mjs`'s outer catch routes any thrown pipeline error through
 * `toStageErrorRecord`, and `proof-video.mjs`'s `ffmpeg()` carries the hard
 * timeout that prevents the hang. `runStage`/`runFfmpegStage` are the canonical
 * stage-tagging + timeout primitives the regression fixtures exercise and that
 * new call sites should adopt; they are NOT yet wrapped around every individual
 * stage (a documented follow-up), so a generic crash records stage `pipeline`.
 *
 * Pure/injectable so the classification can be unit-tested without real binaries.
 */

import { spawnSync } from 'node:child_process'

/** Default hard ceiling for a single ffmpeg invocation before it is killed. */
export const DEFAULT_FFMPEG_TIMEOUT_MS = 4 * 60_000

/**
 * A pipeline-stage failure. Carries the stage name and a trimmed stderr tail so
 * the deferred comment can name exactly where the pipeline broke.
 */
export class StageError extends Error {
  /**
   * @param {string} stage
   * @param {string} message
   * @param {{ stderrTail?: string, elapsedMs?: number }} [extra]
   */
  constructor(stage, message, { stderrTail = '', elapsedMs } = {}) {
    super(message)
    this.name = 'StageError'
    this.stage = stage
    this.stderrTail = stderrTail
    if (elapsedMs !== undefined) this.elapsedMs = elapsedMs
  }

  /** Plain-object shape recorded into the manifest. Never carries secret values. */
  toRecord() {
    const record = { stage: this.stage, message: this.message, stderrTail: this.stderrTail }
    if (this.elapsedMs !== undefined) record.elapsedMs = this.elapsedMs
    return record
  }
}

/**
 * Returns the last `maxLines` non-empty lines of `text`, capped at `maxChars`
 * from the end so an enormous log can't bloat a PR comment. Pure.
 * @param {unknown} text
 * @param {number} [maxLines]
 * @param {number} [maxChars]
 * @returns {string}
 */
export function tailLines(text, maxLines = 20, maxChars = 4000) {
  const s = text == null ? '' : String(text)
  const capped = s.length > maxChars ? s.slice(-maxChars) : s
  const lines = capped.split(/\r?\n/).filter((line) => line.trim().length > 0)
  return lines.slice(-maxLines).join('\n')
}

/**
 * Runs `fn` tagged as pipeline stage `stage`. Any throw (sync or async) is
 * captured as a StageError naming the stage with a stderr tail. An error that is
 * already a StageError is re-thrown untouched (the innermost stage wins).
 * @template T
 * @param {string} stage
 * @param {() => T | Promise<T>} fn
 * @returns {Promise<T>}
 */
export async function runStage(stage, fn) {
  try {
    return await fn()
  } catch (err) {
    if (err instanceof StageError) throw err
    const message = err instanceof Error ? err.message : String(err)
    // Prefer a captured stderr (spawn results attach `.stderr`); fall back to the
    // stack (which includes the ReferenceError name+site for a TDZ crash).
    const raw = (err && (err.stderr ?? err.stack)) || message
    throw new StageError(stage, message, { stderrTail: tailLines(raw) })
  }
}

/**
 * Runs a single ffmpeg (or ffprobe) invocation under a hard timeout. A hung
 * encode is killed and reported as a StageError at the given stage with the
 * elapsed time, so it classifies as `pipeline-error` (never a silent 30-minute
 * hang). Injectable (`spawn`, `now`, `bin`) so the timeout path is unit-testable
 * with a fake sleeping binary.
 *
 * @param {string[]} args
 * @param {{
 *   stage?: string,
 *   bin?: string,
 *   timeoutMs?: number,
 *   spawn?: typeof spawnSync,
 *   now?: () => number,
 *   spawnOptions?: object,
 * }} [opts]
 * @returns {import('node:child_process').SpawnSyncReturns<string>}
 */
export function runFfmpegStage(
  args,
  {
    stage = 'ffmpeg',
    bin = 'ffmpeg',
    timeoutMs = DEFAULT_FFMPEG_TIMEOUT_MS,
    spawn = spawnSync,
    now = () => Date.now(),
    spawnOptions = {},
  } = {},
) {
  const start = now()
  const res = spawn(bin, args, {
    encoding: 'utf8',
    timeout: timeoutMs,
    killSignal: 'SIGKILL',
    ...spawnOptions,
  })
  const elapsedMs = now() - start
  if (wasKilledByTimeout(res)) {
    throw new StageError(
      stage,
      `ffmpeg killed after ${elapsedMs}ms hard timeout (${timeoutMs}ms)`,
      {
        stderrTail: tailLines(res.stderr),
        elapsedMs,
      },
    )
  }
  if (res.error) {
    throw new StageError(stage, `ffmpeg failed to spawn: ${res.error.message}`, {
      stderrTail: tailLines(res.stderr),
      elapsedMs,
    })
  }
  if (res.status !== 0) {
    throw new StageError(stage, `ffmpeg exited ${res.status}`, {
      stderrTail: tailLines(res.stderr),
      elapsedMs,
    })
  }
  return res
}

/**
 * True when a spawnSync result was terminated by its timeout. Node sets
 * `error.code === 'ETIMEDOUT'` and/or leaves `signal` at the kill signal.
 * @param {{ error?: Error & { code?: string }, signal?: string | null, status?: number | null }} res
 * @returns {boolean}
 */
export function wasKilledByTimeout(res) {
  if (res?.error && res.error.code === 'ETIMEDOUT') return true
  return res?.status == null && (res?.signal === 'SIGKILL' || res?.signal === 'SIGTERM')
}

/**
 * Maps a captured pipeline failure to its honest reason code. Any error that
 * escaped a stage is OUR bug, so it is always `pipeline-error` — never
 * `env-unavailable` (that class is decided up front by the doctor preflight).
 * @param {unknown} _err
 * @returns {'pipeline-error'}
 */
export function reasonCodeForStageError(_err) {
  return 'pipeline-error'
}

/**
 * Env keys whose VALUES are injected secrets (proofenv P1a). A pipeline-error
 * note embeds a raw subprocess stderr tail, and that subprocess runs with these
 * secrets in its environment — a tool that dumps its env on a fatal error could
 * otherwise leak a secret VALUE into the public PR comment / manifest. Scrub
 * them as defense-in-depth so the "no secret value reaches any log/error
 * string" invariant holds even on the crash path.
 */
const SECRET_ENV_KEYS = ['PROOF_ANTHROPIC_API_KEY', 'CLOUDFLARE_API_TOKEN']

/**
 * Replaces any occurrence of an injected secret value with a redaction marker.
 * Values shorter than 8 chars are ignored (too short to be a real token, and
 * scrubbing them would mangle unrelated text). Pure over an injectable env.
 * @param {unknown} text
 * @param {NodeJS.ProcessEnv} [env]
 * @returns {string}
 */
export function scrubSecrets(text, env = process.env) {
  let out = text == null ? '' : String(text)
  for (const key of SECRET_ENV_KEYS) {
    const value = env[key]
    if (value && value.length >= 8) out = out.split(value).join('«redacted-secret»')
  }
  return out
}

/**
 * Normalises any thrown pipeline error into the record recorded on the manifest
 * and passed to renderDeferredComment. A non-StageError is wrapped at stage
 * `pipeline` so the note still names a stage and a stderr tail. The stderr tail
 * is always secret-scrubbed before it can reach a posted note.
 * @param {unknown} err
 * @returns {{ stage: string, message: string, stderrTail: string, elapsedMs?: number }}
 */
export function toStageErrorRecord(err) {
  if (err instanceof StageError) {
    const record = err.toRecord()
    record.stderrTail = scrubSecrets(record.stderrTail)
    return record
  }
  const message = err instanceof Error ? err.message : String(err)
  const raw = (err && (err.stderr ?? err.stack)) || message
  return { stage: 'pipeline', message, stderrTail: scrubSecrets(tailLines(raw)) }
}
