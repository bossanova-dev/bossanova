// Pure ffmpeg arg builder for the clickable video poster: a darkened middle
// screenshot with a YouTube-style play button composited over the centre. The
// README links the poster image to the raw video, so a reviewer sees a
// recognisable "click to play" thumbnail instead of a bare file link.
//
// Kept separate from proof-video.mjs (like proof-video-intro.mjs) so it
// unit-tests without ffmpeg. Returns args only — the caller prepends
// `-y -loglevel error` and runs ffmpeg.

const ENCODE_ARGS = ['-frames:v', '1']

/**
 * Build the ffmpeg arg vector that composites `playButton` over a scrimmed
 * `base` screenshot and writes one PNG to `outPath`.
 *
 * Filtergraph:
 *   [1:v]scale=<scaleW>:-1[pb];
 *   [0:v][crop=in_w:<cropHeight>:0:0,]drawbox=...:color=black@<scrim>:t=fill[bg];
 *   [bg][pb]overlay=centered[v]
 *
 * @param {{ base: string, playButton: string, outPath: string, scaleW?: number, scrim?: number, cropHeight?: number, overlayCenterHeight?: number }} a
 * @returns {string[]}
 */
export function buildPosterArgs({
  base,
  playButton,
  outPath,
  scaleW = 150,
  scrim = 0.16,
  cropHeight,
  overlayCenterHeight,
}) {
  const crop =
    cropHeight != null && Number.isInteger(cropHeight) && cropHeight > 0
      ? `crop=in_w:${cropHeight}:0:0,`
      : ''
  // Center the play button over the content region when given (clamped to the
  // crop), so short, padded pages don't put the button in the empty band.
  const centerH =
    overlayCenterHeight != null && Number.isInteger(overlayCenterHeight) && overlayCenterHeight > 0
      ? cropHeight
        ? Math.min(overlayCenterHeight, cropHeight)
        : overlayCenterHeight
      : null
  const yExpr = centerH != null ? `(${centerH}-overlay_h)/2` : '(main_h-overlay_h)/2'
  const filter = [
    `[1:v]scale=${scaleW}:-1[pb]`,
    `[0:v]${crop}drawbox=x=0:y=0:w=iw:h=ih:color=black@${scrim}:t=fill[bg]`,
    `[bg][pb]overlay=(main_w-overlay_w)/2:${yExpr}[v]`,
  ].join(';')
  return [
    '-i',
    base,
    '-i',
    playButton,
    '-filter_complex',
    filter,
    '-map',
    '[v]',
    ...ENCODE_ARGS,
    outPath,
  ]
}
