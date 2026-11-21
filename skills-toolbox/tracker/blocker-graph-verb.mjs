import crypto from 'node:crypto'
import { writeFileSync, renameSync, rmSync } from 'node:fs'
import { resolve } from 'node:path'
import { parseArgs } from 'node:util'
import { blockerGraphDefect } from './blocker-graph.mjs'

/** @param {string[]} args
 * @param {{write: (text: string) => void, errWrite: (text: string) => void, env: object, resolveAdapter: Function}} io */
export async function runFetchBlockerGraph(args, { write, errWrite, env, resolveAdapter }) {
  let ids, out, maxNodes
  try {
    const { values, tokens } = parseArgs({
      args,
      strict: true,
      tokens: true,
      options: {
        'out-file': { type: 'string' },
        id: { type: 'string', multiple: true },
        'max-nodes': { type: 'string' },
      },
    })
    const seen = new Set()
    for (const token of tokens) {
      if (token.kind !== 'option') continue
      if (seen.has(token.name) && token.name !== 'id') throw new Error('repeated option')
      seen.add(token.name)
      if (!token.value?.trim() || /^--/.test(token.value)) throw new Error('missing option value')
    }
    if (!values['out-file'] || !values.id?.length) throw new Error('missing required option')
    ids = [...new Set(values.id.flatMap((id) => id.split(',').map((id) => id.trim())))]
    if (ids.some((id) => !id)) throw new Error('empty id')
    if (values['max-nodes'] !== undefined && !/^\d+$/.test(values['max-nodes']))
      throw new Error('invalid node cap')
    maxNodes = Number(values['max-nodes'] ?? 100)
    if (!Number.isInteger(maxNodes) || maxNodes < 1 || maxNodes > 500)
      throw new Error('invalid node cap')
    out = resolve(values['out-file'])
  } catch {
    errWrite(
      'fetch-blocker-graph: usage: --out-file <path> --id <id>[,<id>…]... [--max-nodes <1-500>]\n',
    )
    return 64
  }
  let temp
  try {
    const adapter = resolveAdapter({ env })
    if (typeof adapter.readBlockerGraph !== 'function') {
      errWrite('fetch-blocker-graph: missing readBlockerGraph capability; could not evaluate\n')
      return 2
    }
    const graph = await adapter.readBlockerGraph({ ids, maxNodes })
    const defect = blockerGraphDefect(graph)
    if (defect) throw new Error(defect)
    const body = JSON.stringify(graph) + '\n'
    temp = out + '.' + crypto.randomBytes(6).toString('hex') + '.tmp'
    writeFileSync(temp, body, { flag: 'wx', mode: 0o600 })
    renameSync(temp, out)
    write(
      JSON.stringify({
        outcome: 'blocker-graph-written',
        nodes: graph.nodes.length,
        complete: graph.complete,
        bytes: Buffer.byteLength(body),
        out,
      }) + '\n',
    )
    return 0
  } catch {
    // Adapter errors may embed credentials or tracker response bodies.
    errWrite('fetch-blocker-graph: graph read or atomic write failed; could not evaluate\n')
    return 2
  } finally {
    if (temp) rmSync(temp, { force: true })
  }
}
