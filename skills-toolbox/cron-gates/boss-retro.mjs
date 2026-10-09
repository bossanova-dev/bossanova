#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { resolveBossBinary } from '../boss-binary.mjs'
import { loadSkillConfig, retroConfig } from '../skill-config.mjs'
import { gateExit } from '../linear-gate-lib.mjs'
import { isMainModule } from '../main-module.mjs'

export async function evaluateBossRetroGate({
  argv = [],
  env = process.env,
  config = {},
  listNotes,
  readState,
  collectSignals,
}) {
  try {
    let flag
    for (let i = 0; i < argv.length; i++) {
      const [name, ...value] = argv[i].split('=')
      if (name !== '--threshold') throw new Error(`unknown flag ${argv[i]}`)
      flag = value.length ? value.join('=') : argv[++i]
      if (flag === undefined) throw new Error('--threshold requires a value')
    }
    const source =
      flag !== undefined
        ? '--threshold'
        : env.BOSS_RETRO_GATE_THRESHOLD !== undefined
          ? 'BOSS_RETRO_GATE_THRESHOLD'
          : config.retro?.gateThreshold !== undefined
            ? 'retro.gateThreshold'
            : 'default'
    const value =
      flag ??
      env.BOSS_RETRO_GATE_THRESHOLD ??
      config.retro?.gateThreshold ??
      retroConfig(config).gateThreshold ??
      5
    if (!/^\d+$/.test(String(value)) || !Number.isSafeInteger(Number(value)) || Number(value) < 1)
      throw new Error(`invalid threshold from ${source}`)
    const threshold = Number(value),
      notes = await listNotes()
    if (notes?.unreachable === true)
      return { hasWork: true, reason: 'boss CLI unreachable from the gate; the session decides' }
    if (
      !Array.isArray(notes) ||
      notes.some(
        (note) =>
          !note ||
          typeof note !== 'object' ||
          typeof note.id !== 'string' ||
          !note.id.trim() ||
          !Array.isArray(note.tags) ||
          !note.tags.includes('improvement') ||
          typeof note.created_at !== 'string' ||
          !Number.isFinite(Date.parse(note.created_at)),
      )
    )
      throw new Error('malformed or untagged note')
    let state,
      warning = ''
    try {
      state = await readState()
      if (state?.version !== 1 || !Number.isFinite(Date.parse(state.lastRunAt)))
        throw new Error('missing or corrupt state')
    } catch {
      warning = '; missing or corrupt state, all notes count'
      state = null
    }
    const count = notes.filter(
      (note) => !state || Date.parse(note.created_at) > Date.parse(state.lastRunAt),
    ).length
    let signals = 0
    if (count < threshold) {
      try {
        const result = await collectSignals()
        if (!Array.isArray(result.planned)) throw new Error('non-JSON or malformed signals')
        signals = result.planned.length
      } catch (error) {
        warning += `; signals contribute 0 (${error.message})`
      }
    }
    const hasWork = count + signals >= threshold
    return {
      hasWork,
      reason: `boss-retro gate: ${count} new notes + ${signals} signals since ${state?.lastRunAt ?? 'never'}, ${hasWork ? 'at or above' : 'below'} threshold ${threshold} (${source})${warning}`,
    }
  } catch (error) {
    return { hasWork: false, reason: `boss-retro gate: ${error.message}` }
  }
}
if (isMainModule(import.meta.url)) {
  try {
    const binary = resolveBossBinary()
    const verdict = await evaluateBossRetroGate({
      argv: process.argv.slice(2),
      config: loadSkillConfig(),
      listNotes: () => {
        if (!binary.ok) return { unreachable: true }
        const result = spawnSync(
          binary.path,
          ['notes', 'ls', '--tag', 'improvement', '--json', '--limit', '0'],
          { encoding: 'utf8', timeout: 10000 },
        )
        if (result.error || result.status !== 0) throw new Error('notes store read failed')
        return JSON.parse(result.stdout)
      },
      readState: () => {
        const common = execFileSync('git', ['rev-parse', '--git-common-dir'], {
          encoding: 'utf8',
        }).trim()
        return JSON.parse(fs.readFileSync(path.join(common, 'boss-retro', 'state.json'), 'utf8'))
      },
      collectSignals: () => {
        const result = spawnSync(
          process.execPath,
          [fileURLToPath(new URL('../retro-signals.mjs', import.meta.url)), 'collect', '--dry-run'],
          { encoding: 'utf8', timeout: 40000 },
        )
        if (result.error || result.status !== 0)
          throw new Error(result.error?.code ?? 'collector failed')
        return JSON.parse(result.stdout)
      },
    })
    if (verdict.hasWork && verdict.reason) process.stderr.write(verdict.reason + '\n')
    gateExit(verdict.hasWork, verdict.reason)
  } catch (error) {
    gateExit(false, `boss-retro gate: ${error.message}`)
  }
}
