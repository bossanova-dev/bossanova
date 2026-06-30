#!/usr/bin/env node
// Re-run screencast post-processing (elapsed timer + idle-section speedup)
// on an existing bs-proof recipe directory. Rewrites <id>.mp4 from the raw
// recording <id>.webm in place.
//
// Usage: node scripts/proof-postprocess-video.mjs --proof-dir <recipeDir>
//
// CAVEAT: The normal proof pipeline deletes <id>.webm after a successful run.
// This command only works when the webm was preserved — e.g. when the proof
// run was executed with BOSS_PROOF_UPLOAD=0 (which keeps the recipe directory
// intact). If the webm is absent, run a fresh capture instead.
//
// bs-proof recipe dir layout:
//   <recipeDir>/<id>.webm   — raw browser recording (input; preserved only when upload skipped)
//   <recipeDir>/<id>.png    — screenshot / poster (overwritten with play-button composite on success)
//   <recipeDir>/<id>.mp4    — condensed output (written/overwritten by this script)

import { copyFileSync, existsSync, readdirSync, rmSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { postprocessProofVideo } from './proof-video.mjs'
import { buildPosterArgs } from './proof-poster.mjs'

// Only drive the CLI when invoked directly; importing this module (e.g. from
// tests) must not start a run.
const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)

if (invokedDirectly) {
  try {
    run(process.argv.slice(2))
  } catch (error) {
    process.stderr.write(error.message + '\n')
    process.exitCode = 1
  }
}

function run(argv) {
  const dirIdx = argv.indexOf('--proof-dir')
  const proofDir = dirIdx >= 0 ? argv[dirIdx + 1] : undefined
  if (!proofDir) {
    process.stderr.write('usage: proof-postprocess-video.mjs --proof-dir <recipeDir>\n')
    process.exit(1)
  }

  if (!existsSync(proofDir)) {
    process.stderr.write(`[bs-proof] ERROR: directory not found: ${proofDir}\n`)
    process.exit(1)
  }

  // Find the single *.webm in the recipe dir.
  const entries = readdirSync(proofDir)
  const webms = entries.filter((f) => f.endsWith('.webm'))
  if (webms.length === 0) {
    process.stderr.write(
      `[bs-proof] ERROR: no .webm found in ${proofDir}\n` +
        `  The normal proof pipeline deletes the .webm after a successful run.\n` +
        `  Re-run the capture with BOSS_PROOF_UPLOAD=0 to preserve it, then retry.\n`,
    )
    process.exit(1)
  }
  if (webms.length > 1) {
    process.stderr.write(
      `[bs-proof] ERROR: multiple .webm files found in ${proofDir} — cannot determine which to use:\n` +
        webms.map((f) => `  ${f}`).join('\n') +
        '\n',
    )
    process.exit(1)
  }

  const id = path.basename(webms[0], '.webm')
  const webmPath = path.join(proofDir, webms[0])
  const outPath = path.join(proofDir, `${id}.mp4`)
  const timedPath = path.join(proofDir, `${id}-timed.mp4`)
  const scratchPath = path.join(proofDir, `${id}-timer.raw`)

  // No introPngPath — a bare re-process has no PR context (the wc-proof
  // reference omits the intro card here too).
  const result = postprocessProofVideo({ webmPath, timedPath, outPath, scratchPath })

  // Clean up the timed intermediate on every path (scratchPath is already
  // removed by postprocessProofVideo). postprocessProofVideo leaves timedPath
  // to the caller, so it can survive a pass2 failure too.
  if (existsSync(timedPath)) rmSync(timedPath, { force: true })

  if (!result.ok) {
    process.stderr.write(`[bs-proof] ERROR: post-processing failed — ${result.warning}\n`)
    process.exit(1)
  }

  const fmtMs = (ms) =>
    `${Math.floor(ms / 60000)}:${String(Math.round(ms / 1000) % 60).padStart(2, '0')}`
  if (result.condensed) {
    process.stdout.write(
      `[bs-proof] wrote ${outPath} — condensed ${fmtMs(result.originalMs)} → ${fmtMs(result.outputMs)} (${result.fastSegments} idle stretch(es) sped up)\n`,
    )
  } else {
    process.stdout.write(`[bs-proof] wrote ${outPath} — timer burned, nothing to condense\n`)
  }

  // Best-effort: composite the play-button poster over the existing screenshot.
  // ffmpeg refuses the same path as both input and output, so render to a temp
  // file and copy it back over the poster.
  const pngPath = path.join(proofDir, `${id}.png`)
  if (existsSync(pngPath)) {
    const tmpPosterPath = path.join(proofDir, `${id}-poster-tmp.png`)
    const playButton = fileURLToPath(new URL('./assets/youtube-play-button.png', import.meta.url))
    const posterArgs = buildPosterArgs({ base: pngPath, playButton, outPath: tmpPosterPath })
    const posterResult = spawnSync('ffmpeg', ['-y', '-loglevel', 'error', ...posterArgs], {
      stdio: ['ignore', 'inherit', 'inherit'],
    })
    if (posterResult.status === 0 && existsSync(tmpPosterPath)) {
      copyFileSync(tmpPosterPath, pngPath)
      process.stdout.write(`[bs-proof] updated poster ${pngPath}\n`)
    } else {
      process.stderr.write(
        `[bs-proof] WARN: poster composite failed (ffmpeg exit ${posterResult.status}) — ${pngPath} left unchanged\n`,
      )
    }
    rmSync(tmpPosterPath, { force: true })
  }
}
