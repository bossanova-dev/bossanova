import { spawn, execFileSync } from 'node:child_process'
import { appendFile, mkdir, readFile, readdir, rename, rm, writeFile } from 'node:fs/promises'
import { constants } from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from './main-module.mjs'

const identifier = /^[a-z0-9][a-z0-9._-]{0,63}$/

/** @param {string} file @param {object} value */
async function atomicWrite(file, value) {
  const temporary = `${file}.${process.pid}.tmp`
  await writeFile(temporary, JSON.stringify(value) + '\n')
  await rename(temporary, file)
}

/** @param {string} directory @param {string} dispatch @param {string} id @param {string[]} command */
async function run(directory, dispatch, id, command) {
  const file = path.join(directory, `${id}.json`)
  const logPath = path.join(directory, `${id}.log`)
  const started = Date.now()
  const receipt = {
    version: 1,
    dispatch,
    id,
    status: 'pending',
    command,
    cwd: process.cwd(),
    pid: process.pid,
    startedAt: new Date(started).toISOString(),
  }
  await mkdir(directory, { recursive: true })
  await atomicWrite(file, receipt)
  await writeFile(logPath, '')
  let output = ''
  let writes = Promise.resolve()
  /** @param {Buffer | string} chunk */
  const capture = (chunk) => {
    const value = chunk.toString()
    output = (output + value).split('\n').slice(-21).join('\n')
    writes = writes.then(async () => {
      await appendFile(logPath, value)
    })
  }
  const child = spawn(command[0], command.slice(1), {
    shell: false,
    env: { ...process.env, BOSS_VERIFICATION_RECEIPT: file },
    stdio: ['inherit', 'pipe', 'pipe'],
  })
  /** @param {NodeJS.Signals} signal */
  const forward = (signal) => {
    child.kill(signal)
  }
  const interrupt = () => forward('SIGINT')
  const terminate = () => forward('SIGTERM')
  process.on('SIGINT', interrupt)
  process.on('SIGTERM', terminate)
  child.stdout.on('data', (chunk) => {
    process.stdout.write(chunk)
    capture(chunk)
  })
  child.stderr.on('data', (chunk) => {
    process.stderr.write(chunk)
    capture(chunk)
  })
  /** @type {Error | undefined} */
  let spawnError
  child.on('error', (error) => {
    spawnError = error
    capture(error.message)
  })
  const result = await new Promise((resolve) =>
    child.on('close', (code, signal) => resolve({ code, signal })),
  )
  process.off('SIGINT', interrupt)
  process.off('SIGTERM', terminate)
  await writes
  const exitCode = spawnError
    ? 127
    : result.signal
      ? 128 + constants.signals[result.signal]
      : (result.code ?? 1)
  await atomicWrite(file, {
    ...receipt,
    status: exitCode === 0 ? 'pass' : 'fail',
    exitCode,
    signal: result.signal,
    finishedAt: new Date().toISOString(),
    durationMs: Date.now() - started,
    logPath,
    summary: output.trimEnd().split('\n').slice(-20).join('\n'),
  })
  return exitCode
}

/** @param {string} directory @param {string} dispatch @param {string} id */
async function classify(directory, dispatch, id) {
  const file = path.join(directory, `${id}.json`)
  try {
    const receipt = JSON.parse(await readFile(file, 'utf8'))
    if (
      receipt.version !== 1 ||
      receipt.dispatch !== dispatch ||
      receipt.id !== id ||
      !Array.isArray(receipt.command) ||
      !receipt.command.every((/** @type {unknown} */ part) => typeof part === 'string') ||
      !receipt.command.length
    )
      throw new Error('invalid receipt')
    if (receipt.status === 'pass' || receipt.status === 'fail') {
      if (
        !Number.isInteger(receipt.exitCode) ||
        receipt.exitCode < 0 ||
        typeof receipt.summary !== 'string' ||
        typeof receipt.logPath !== 'string' ||
        (receipt.status === 'pass') !== (receipt.exitCode === 0)
      )
        throw new Error('invalid terminal receipt')
      return { ...receipt }
    }
    if (receipt.status !== 'pending' || !Number.isInteger(receipt.pid) || receipt.pid <= 0)
      throw new Error('invalid pending receipt')
    let alive = true
    try {
      process.kill(receipt.pid, 0)
    } catch (error) {
      if (error.code === 'ESRCH') alive = false
      else if (error.code !== 'EPERM') throw error
    }
    return { ...receipt, status: alive ? 'pending' : 'orphaned' }
  } catch (error) {
    return {
      id,
      status: error.code === 'ENOENT' ? 'absent' : 'orphaned',
      reason: error.code === 'ENOENT' ? 'absent' : 'unreadable',
    }
  }
}

/** @param {string} directory @param {string} dispatch @param {string[]} reported @param {number} waitMs */
async function check(directory, dispatch, reported, waitMs) {
  const deadline = Date.now() + waitMs
  let receipts
  do {
    let files = []
    try {
      files = await readdir(directory)
    } catch (error) {
      if (error.code !== 'ENOENT') throw error
    }
    const ids = [
      ...new Set([
        ...reported,
        ...files.filter((file) => file.endsWith('.json')).map((file) => file.slice(0, -5)),
      ]),
    ].sort()
    receipts = await Promise.all(ids.map((id) => classify(directory, dispatch, id)))
    if (!receipts.some((receipt) => receipt.status === 'pending') || Date.now() >= deadline) break
    await new Promise((resolve) => setTimeout(resolve, Math.min(1000, deadline - Date.now())))
  } while (true)
  const incomplete = receipts
    .filter((receipt) => !['pass', 'fail'].includes(receipt.status))
    .map((receipt) => ({
      id: receipt.id,
      reason: receipt.reason ?? receipt.status,
      command: receipt.command,
    }))
  process.stdout.write(
    JSON.stringify({
      verdict: incomplete.length ? 'incomplete' : 'complete',
      receipts,
      incomplete,
    }) + '\n',
  )
  return incomplete.length ? 2 : receipts.some((receipt) => receipt.status === 'fail') ? 1 : 0
}

/** @param {string[]} argv */
export async function main(argv = process.argv.slice(2)) {
  try {
    const { values, positionals, tokens } = parseArgs({
      args: argv,
      tokens: true,
      strict: true,
      allowPositionals: true,
      options: {
        dispatch: { type: 'string' },
        id: { type: 'string', multiple: true },
        'wait-ms': { type: 'string' },
      },
    })
    const [command, ...commandArgv] = positionals
    const ids = values.id ?? []
    if (
      !command ||
      !values.dispatch ||
      !identifier.test(values.dispatch) ||
      ids.some((id) => !identifier.test(id))
    )
      throw new Error('invalid command, dispatch or id')
    const waitMs = Number(values['wait-ms'] ?? '0')
    if (!Number.isSafeInteger(waitMs) || waitMs < 0) throw new Error('invalid wait-ms')
    if (
      command === 'run' &&
      (ids.length !== 1 ||
        commandArgv.length === 0 ||
        !tokens.some((token) => token.kind === 'option-terminator') ||
        values['wait-ms'] !== undefined)
    )
      throw new Error('run requires one id and a command')
    if (command !== 'run' && commandArgv.length) throw new Error('unexpected command arguments')
    if (command === 'clear' && (ids.length || values['wait-ms'] !== undefined))
      throw new Error('clear takes only dispatch')
    if (!['run', 'check', 'clear'].includes(command))
      throw new Error('expected run, check or clear')
    const root = execFileSync('git', ['rev-parse', '--git-path', 'boss-verification'], {
      encoding: 'utf8',
      timeout: 10000,
    }).trim()
    const directory = path.resolve(root, values.dispatch)
    if (command === 'run') return await run(directory, values.dispatch, ids[0], commandArgv)
    if (command === 'check') return await check(directory, values.dispatch, ids, waitMs)
    if (command === 'clear') {
      await rm(directory, { recursive: true, force: true, maxRetries: 3 })
      return 0
    }
  } catch (error) {
    process.stderr.write(`${error.message}\n`)
    return 64
  }
}

if (isMainModule(import.meta.url)) process.exitCode = await main()
