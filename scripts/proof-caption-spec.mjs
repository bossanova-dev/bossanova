// scripts/proof-caption-spec.mjs — ONE source of truth for proof caption styling
// (BOS-140 / epic D8). Imported by the web recipe runner AND the web agent
// overlay (previously comment-enforced "keep BYTE-IDENTICAL" twins), and by the
// TUI terminal renderer for its blue caption bar. Pure constants — no fs/env.

/** Horizontal px the top-anchored caption must leave clear for the burned-in
 * elapsed-timer pill (TIMER_W 158 + margins, per proof-video.mjs). */
export const TIMER_CLEARANCE_PX = 380

/** Single-line caption length bound shared by overlay.ts formatCaption and
 * proof-lib.mjs formatCaption. */
export const MAX_CAPTION = 140

/** Web caption (recipe runner + agent overlay). max-width is
 * max(60%, calc(100% - TIMER_CLEARANCE_PX)): wide viewports clear the
 * top-right timer pill; 390px mobile recipes fall back to the 60% floor. */
export const OVERLAY_CAPTION_CSS =
  'position:absolute;top:24px;left:50%;transform:translateX(-50%);' +
  'background:rgba(0,0,0,0.72);color:#fff;font:600 15px/1.4 sans-serif;' +
  `padding:6px 18px;border-radius:6px;white-space:pre-wrap;max-width:max(60%, calc(100% - ${TIMER_CLEARANCE_PX}px));` +
  'text-align:center;pointer-events:none;display:none;'

/** TUI blue narration bar (stills + burned video strip). */
export const CAPTION_BAR_STYLE =
  'background:#1d4ed8;color:#fff;font:600 14px/1.5 sans-serif;padding:6px 14px;'
