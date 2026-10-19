#!/usr/bin/env node

import defaultFs from 'node:fs'
import path from 'node:path'
import { randomUUID } from 'node:crypto'
import { parseArgs } from 'node:util'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

function filesUnder(root, fs) {
  return fs
    .readdirSync(root, { withFileTypes: true })
    .flatMap((entry) => {
      const file = path.join(root, entry.name)
      if (entry.isDirectory()) return filesUnder(file, fs)
      if (!entry.isFile()) throw new Error(`Expected regular generated file: ${file}`)
      return [file]
    })
    .sort()
}

export function syncGeneratedTree({ from, to, prune = [], fs = defaultFs }) {
  const sourceFiles = filesUnder(from, fs)
  if (sourceFiles.length === 0) throw new Error(`Generated source is empty: ${from}`)
  const sourceNames = new Set(sourceFiles.map((file) => path.relative(from, file)))
  const result = { written: [], removed: [], unchanged: [] }
  for (const source of sourceFiles) {
    const destination = path.join(to, path.relative(from, source))
    const bytes = fs.readFileSync(source)
    const mode = fs.statSync(source).mode & 0o777
    let unchanged = false
    try {
      unchanged =
        fs.readFileSync(destination).equals(bytes) &&
        (fs.statSync(destination).mode & 0o777) === mode
    } catch (error) {
      if (error.code !== 'ENOENT') throw error
    }
    const relative = path.relative(process.cwd(), destination)
    if (unchanged) {
      result.unchanged.push(relative)
      continue
    }
    fs.mkdirSync(path.dirname(destination), { recursive: true })
    const temporary = path.join(
      path.dirname(destination),
      `.${path.basename(destination)}.${process.pid}.${randomUUID()}.gen-tmp`,
    )
    try {
      fs.writeFileSync(temporary, bytes, { mode, flag: 'wx' })
      fs.chmodSync(temporary, mode)
      fs.renameSync(temporary, destination)
    } catch (error) {
      fs.rmSync(temporary, { force: true })
      throw error
    }
    result.written.push(relative)
  }
  if (prune.length > 0) {
    for (const destination of filesUnder(to, fs)) {
      const relative = path.relative(to, destination)
      if (!sourceNames.has(relative) && prune.some((glob) => path.matchesGlob(relative, glob))) {
        fs.unlinkSync(destination)
        result.removed.push(path.relative(process.cwd(), destination))
      }
    }
  }
  return result
}

if (isMainModule(import.meta.url)) {
  try {
    const { values } = parseArgs({
      options: {
        from: { type: 'string' },
        to: { type: 'string' },
        prune: { type: 'string', multiple: true, default: [] },
      },
      strict: true,
    })
    if (!values.from || !values.to) throw new Error('Required: --from <directory> --to <directory>')
    const result = syncGeneratedTree(values)
    console.log(
      `sync ${values.to}: ${result.written.length} written, ${result.removed.length} removed, ${result.unchanged.length} unchanged`,
    )
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}
