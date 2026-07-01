import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { introCardCommand } from './proof-lib.mjs'
import { buildPosterArgs } from './proof-poster.mjs'
import { evenCropHeight, postprocessProofVideo, probeDimensions } from './proof-video.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const playButtonAsset = fileURLToPath(new URL('./assets/youtube-play-button.png', import.meta.url))

function runCommand(commandTuple) {
  const [command, args, options = {}] = commandTuple
  const result = spawnSync(command, args, {
    cwd: options.cwd ? path.join(repoRoot, options.cwd) : repoRoot,
    stdio: 'inherit',
    env: { ...process.env, ...(options.env ?? {}) },
  })
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(' ')} exited ${result.status}`)
  }
}

// Shared video finish: optional intro card, condense/timer per flags, then a
// play-button poster composite. Both browser and TUI pass
// timer/idleSpeedup/trimLeadingBlank true; the leading-trim is generalized
// (detectLeadingStaticMs) so beyond the browser's bright white flash it also
// drops the VHS dark/blank-terminal boot lead-in and a light surface's static
// pre-roll (e.g. marketing) that the brightness heuristic alone can't trim.
export function finishVideo({
  recipeDir,
  recipeId,
  webmPath,
  pngPath,
  label,
  cardTitle,
  surface,
  cropHeight,
  contentHeight,
  timer,
  idleSpeedup,
  trimLeadingBlank,
  keepWebm,
  captionTimings,
  renderCaptionStrip,
}) {
  const mp4Path = path.join(recipeDir, `${recipeId}.mp4`)
  const timedPath = path.join(recipeDir, `${recipeId}-timed.mp4`)
  const scratchPath = path.join(recipeDir, `${recipeId}-timer.raw`)
  const introPngPath = path.join(recipeDir, `${recipeId}-intro.png`)
  const tmpPosterPath = path.join(recipeDir, `${recipeId}-poster-tmp.png`)
  const dims = probeDimensions(webmPath)

  let resolvedIntroPngPath
  if (label && dims) {
    const introHeight = evenCropHeight(cropHeight, dims.height) ?? dims.height
    try {
      runCommand(
        introCardCommand({
          surface,
          out: path.relative(repoRoot, introPngPath),
          width: dims.width,
          height: introHeight,
          label,
          title: cardTitle,
        }),
      )
      resolvedIntroPngPath = introPngPath
    } catch (err) {
      console.warn(`[proof] intro-card render failed — continuing without it: ${err.message}`)
    }
  }

  const post = postprocessProofVideo({
    webmPath,
    timedPath,
    outPath: mp4Path,
    scratchPath,
    introPngPath: resolvedIntroPngPath,
    cropHeight,
    timer,
    idleSpeedup,
    trimLeadingBlank,
    captionTimings,
    renderCaptionStrip,
  })
  if (!post.ok) {
    console.warn(`[proof] video post-processing failed (${post.warning}) — plain mp4 fallback`)
    const fb = spawnSync('ffmpeg', ['-y', '-loglevel', 'error', '-i', webmPath, mp4Path], {
      stdio: 'inherit',
    })
    if (fb.status !== 0)
      throw new Error('ffmpeg fallback mp4 conversion failed — no usable video artifact')
  }

  try {
    const pr = spawnSync(
      'ffmpeg',
      [
        '-y',
        '-loglevel',
        'error',
        ...buildPosterArgs({
          base: pngPath,
          playButton: playButtonAsset,
          outPath: tmpPosterPath,
          cropHeight,
          overlayCenterHeight: contentHeight,
        }),
      ],
      { stdio: 'inherit' },
    )
    if (pr.status === 0 && fs.existsSync(tmpPosterPath)) fs.copyFileSync(tmpPosterPath, pngPath)
    else console.warn('[proof] play-button poster compositing failed — using plain poster frame')
  } catch (err) {
    console.warn(`[proof] play-button poster compositing error: ${err.message}`)
  }

  const tmpFiles = [timedPath, scratchPath, tmpPosterPath, introPngPath]
  if (!keepWebm) tmpFiles.unshift(webmPath)
  for (const f of tmpFiles) fs.rmSync(f, { force: true })
  return { mp4Path }
}
