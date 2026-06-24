// Pure ffmpeg arg builders for prepending a branded intro card to proof.mp4.
// Kept separate from proof-video.mjs so they unit-test without touching ffmpeg.
// ENCODE_ARGS mirrors proof-video.mjs so the intro clip matches the main video's
// codec/pix_fmt and the filter-concat re-encode stays consistent.
//
// Also exports buildIntroCardHtml — the HTML/CSS for the white-label-on-black
// intro card rendered by a headless browser (Playwright). Ported from:
//   wondercanvas-mono/apps/e2e/scripts/render-intro-card.ts

export const INTRO_SEC = 2;
const ENCODE_ARGS = [
  '-c:v',
  'libx264',
  '-preset',
  'veryfast',
  '-crf',
  '23',
  '-pix_fmt',
  'yuv420p',
  '-movflags',
  '+faststart',
  '-an',
];

/** A still PNG → an INTRO_SEC clip at the exact video geometry + fps. */
export function buildIntroClipArgs({ pngPath, width, height, fps, outPath }) {
  return [
    '-loop',
    '1',
    '-t',
    String(INTRO_SEC),
    '-i',
    pngPath,
    '-vf',
    `scale=${width}:${height}:flags=lanczos,format=yuv420p,fps=${fps}`,
    ...ENCODE_ARGS,
    outPath,
  ];
}

/** Concat [intro][main] via the filter graph (robust across two encodes). */
export function buildIntroConcatArgs({ introPath, mainPath, outPath }) {
  return [
    '-i',
    introPath,
    '-i',
    mainPath,
    '-filter_complex',
    '[0:v][1:v]concat=n=2:v=1:a=0[v]',
    '-map',
    '[v]',
    ...ENCODE_ARGS,
    outPath,
  ];
}

function escapeHtml(s) {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

/**
 * Build the HTML string for the intro card rendered by a headless browser.
 * White label-on-black, grey-clamped title. Sizes are in vh so the card scales
 * to any video resolution. Ported verbatim from render-intro-card.ts.
 * @param {{ label: string, title: string }} opts
 * @returns {string}
 */
export function buildIntroCardHtml({ label, title }) {
  // Sizes are in vh so the card scales to any video resolution. Title is
  // clamped to 2 lines with an ellipsis. White-on-black for a dark thumbnail.
  return `<!doctype html><html><head><meta charset="utf-8"><style>
    html,body{margin:0;height:100%;background:#000;}
    .wrap{height:100%;display:flex;flex-direction:column;align-items:center;justify-content:center;
      font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;text-align:center;padding:0 8vw;box-sizing:border-box;}
    .label{color:#fff;font-weight:700;font-size:9vh;line-height:1.05;letter-spacing:-0.01em;}
    .title{color:#b8b8b8;font-weight:400;font-size:4vh;line-height:1.25;margin-top:3vh;max-width:84vw;
      display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden;text-overflow:ellipsis;}
  </style></head><body><div class="wrap">
    <div class="label">${escapeHtml(label)}</div>
    <div class="title">${escapeHtml(title)}</div>
  </div></body></html>`;
}
