import fs from 'node:fs'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

/** @param {string[]} args */
export function runCli(args) {
  try {
    const { values, positionals } = parseArgs({
      args,
      strict: true,
      allowPositionals: true,
      options: {
        lenses: { type: 'string' },
        lens: { type: 'string', multiple: true },
        out: { type: 'string' },
      },
    })
    if (positionals.length !== 1 || positionals[0] !== 'select' || !values.lenses || !values.out) {
      throw new Error('usage: select --lenses FILE [--lens ID ...] --out FILE')
    }
    const { lenses } = JSON.parse(fs.readFileSync(values.lenses, 'utf8'))
    const requested = new Set(values.lens ?? [])
    const ids = new Set(lenses.map((/** @type {{id: string}} */ lens) => lens.id))
    const unknown = [...requested].filter((id) => !ids.has(id))
    if (unknown.length) throw new Error(`unknown lens IDs: ${unknown.join(', ')}`)
    const selected = lenses.filter(
      (/** @type {{id: string}} */ lens) => requested.size === 0 || requested.has(lens.id),
    )
    fs.writeFileSync(values.out, `${JSON.stringify(selected, null, 2)}\n`)
    return 0
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    return 2
  }
}

if (isMainModule(import.meta.url)) process.exitCode = runCli(process.argv.slice(2))
