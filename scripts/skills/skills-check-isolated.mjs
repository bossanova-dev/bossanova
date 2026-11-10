#!/usr/bin/env node
import { randomUUID } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

/** @param {string[]} argv */
export function main(argv) {
  const { values } = parseArgs({
    args: argv,
    options: { boss: { type: 'string' }, build: { type: 'boolean', default: false } },
    strict: true,
    allowPositionals: false,
  })
  if (!values.boss) throw new Error('Usage: skills-check-isolated.mjs --boss <binary>')
  const boss = path.resolve(values.boss)
  const home = mkdtempSync(path.join(tmpdir(), 'boss-skills-check-'))
  const workspace = path.resolve(`.skills-check-${randomUUID()}.work`)
  try {
    if (values.build) {
      // Keep relative use/replace paths anchored at the caller's workspace root.
      copyFileSync('go.work', workspace)
      if (existsSync('go.work.sum')) copyFileSync('go.work.sum', `${workspace}.sum`)
      const result = spawnSync('make', ['build-boss'], {
        env: { ...process.env, GOWORK: workspace },
        stdio: 'inherit',
      })
      if (result.error) process.stderr.write(`build boss: ${result.error.message}\n`)
      if (result.error || result.status !== 0) return result.status || 1
    }
    const bin = path.join(home, 'bin')
    mkdirSync(bin)
    for (const agent of ['claude', 'codex']) {
      writeFileSync(path.join(bin, agent), '#!/bin/sh\nexit 0\n', { mode: 0o755 })
    }
    /** @type {NodeJS.ProcessEnv} */
    const env = {
      ...process.env,
      HOME: home,
      PATH: `${bin}${path.delimiter}${process.env.PATH || ''}`,
      BOSS_TRUST_CHECKOUT_SKILLS: '1',
    }
    delete env.HERMES_HOME
    for (const step of ['install', 'check']) {
      const result = spawnSync(boss, ['skills', step], {
        env,
        encoding: 'utf8',
        maxBuffer: 16 * 1024 * 1024,
      })
      if (result.stdout) process.stdout.write(result.stdout)
      if (result.stderr) process.stderr.write(result.stderr)
      if (result.error) {
        process.stderr.write(`boss skills ${step}: ${result.error.message}\n`)
        return result.status || 1
      }
      if (result.status !== 0) return result.status || 1
      if (step === 'check') {
        const output = result.stdout + result.stderr
        if (
          output.includes('no supported agents found on PATH') ||
          !/^\s*installed: yes\s*$/m.test(output)
        ) {
          process.stderr.write(
            'boss skills check: isolated check did not prove an installed agent tree\n',
          )
          return 1
        }
      }
    }
    return 0
  } finally {
    rmSync(workspace, { force: true })
    rmSync(`${workspace}.sum`, { force: true })
    rmSync(home, { recursive: true, force: true, maxRetries: 3 })
  }
}

if (isMainModule(import.meta.url)) {
  try {
    process.exitCode = main(process.argv.slice(2))
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 1
  }
}
