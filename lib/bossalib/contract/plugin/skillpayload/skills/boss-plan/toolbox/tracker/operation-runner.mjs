import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import {
  operationTransport,
  REQUIRED_TRACKER_OPERATIONS,
  OPTIONAL_TRACKER_OPERATIONS,
} from './adapter-core.mjs'

export function toolboxRoot() {
  return fileURLToPath(new URL('../', import.meta.url))
}

export function resolveOperation(operationMap, key) {
  if (!operationMap || !Object.hasOwn(operationMap, key)) return null
  const op = operationMap[key]
  const transport = operationTransport(op)
  if (!transport) return null
  const summary = typeof op.summary === 'string' ? op.summary : null
  return transport === 'mcp'
    ? { transport, tool: op.tool, summary }
    : { transport, command: op.command, summary }
}

export class OperationFailure extends Error {
  constructor(status, diagnostic) {
    super(`command failed (${status}): ${diagnostic}`)
    this.status = status
    this.diagnostic = diagnostic
  }
}
const lastLine = (text) =>
  String(text ?? '')
    .trim()
    .split(/\r?\n/)
    .at(-1) || 'no stderr diagnostic'

/** @param {{transport: string, tool?: string, command?: {helper: string, verb: string, args?: string[]}}} op @param {object} args
 * @param {{toolboxRoot?: () => string, spawn?: typeof spawnSync, timeoutMs?: number}} [deps] */
export function runOperation(op, args, deps = {}) {
  if (op.transport === 'mcp')
    return { transport: 'mcp', tool: op.tool, args, outcome: 'descriptor-emitted' }
  if (op.transport !== 'command' || operationTransport(op) !== 'command') {
    throw new OperationFailure('invalid', 'unusable command helper')
  }
  let temp
  try {
    const root = realpathSync((deps.toolboxRoot ?? toolboxRoot)())
    const helper = realpathSync(path.resolve(root, op.command.helper))
    const relative = path.relative(root, helper)
    if (
      relative === '' ||
      relative.startsWith(`..${path.sep}`) ||
      relative === '..' ||
      path.isAbsolute(relative)
    ) {
      throw new Error('helper resolves outside toolbox root')
    }
    temp = mkdtempSync(path.join(tmpdir(), 'tracker-operation-'))
    const argsFile = path.join(temp, 'args.json')
    writeFileSync(argsFile, JSON.stringify(args), { encoding: 'utf8', mode: 0o600 })
    const child = (deps.spawn ?? spawnSync)(
      process.execPath,
      [helper, op.command.verb, ...(op.command.args ?? []), '--args-file', argsFile],
      {
        encoding: 'utf8',
        timeout: deps.timeoutMs ?? 120_000,
        maxBuffer: 16 * 1024 * 1024,
      },
    )
    if (child.error || child.signal || child.status !== 0) {
      throw new OperationFailure(
        (child.error && 'code' in child.error ? child.error.code : undefined) ??
          child.signal ??
          child.status,
        lastLine(child.stderr || child.error?.message),
      )
    }
    let result
    try {
      result = JSON.parse(child.stdout)
    } catch {
      throw new OperationFailure(child.status, `invalid JSON stdout; ${lastLine(child.stderr)}`)
    }
    return { transport: 'command', result, outcome: 'operation-executed' }
  } catch (error) {
    if (error instanceof OperationFailure) throw error
    throw new OperationFailure(error.code ?? 'unavailable', lastLine(error.message))
  } finally {
    if (temp) rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
  }
}

export function emitOperation(op, args, { write }, extra = {}, legacyOutcome = true) {
  const result = runOperation(op, args)
  const value =
    result.transport === 'mcp'
      ? {
          tool: result.tool,
          args,
          ...extra,
          ...(legacyOutcome ? { outcome: 'descriptor-emitted' } : {}),
        }
      : { ...result, ...extra }
  write(JSON.stringify(value) + '\n')
}

export function runInvokeOperation(rest, { write, errWrite, env, resolveAdapter }) {
  const fail = (message, code) => {
    errWrite(`invoke-operation: ${message}\n`)
    return code
  }
  let key, args
  try {
    const parsed = parseArgs({
      args: rest,
      strict: true,
      allowPositionals: true,
      options: { 'args-file': { type: 'string' } },
    })
    const known = [...REQUIRED_TRACKER_OPERATIONS, ...OPTIONAL_TRACKER_OPERATIONS]
    if (parsed.positionals.length !== 1 || !known.includes(parsed.positionals[0]))
      throw new Error('expected a known operation name')
    key = parsed.positionals[0]
    args =
      parsed.values['args-file'] === undefined
        ? {}
        : JSON.parse(readFileSync(parsed.values['args-file'], 'utf8'))
    if (!args || typeof args !== 'object' || Array.isArray(args))
      throw new Error('--args-file must contain a JSON object')
  } catch (error) {
    return fail(lastLine(error.message), 64)
  }
  try {
    const op = resolveOperation(resolveAdapter({ env }).operationMap, key)
    if (!op) return fail(`${key} has no usable tool or command`, 2)
    write(JSON.stringify(runOperation(op, args)) + '\n')
    return 0
  } catch (error) {
    return fail(`${key} ${lastLine(error.message)}`, 2)
  }
}
