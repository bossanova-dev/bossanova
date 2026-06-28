#!/usr/bin/env node

const VHS_KEY = { enter: 'Enter', esc: 'Escape' };

// How long a VHS `Wait+Screen` may block before failing the recording. Mirrors
// the 10s per-step wait the still-validation driver uses (proof_capture_test.go)
// so a step that the validator can satisfy will not time the recording out.
const VHS_WAIT_TIMEOUT_MS = 10000;

function vhsDirectiveForKey(key) {
  if (VHS_KEY[key]) return VHS_KEY[key];
  const m = /^ctrl\+([a-z])$/.exec(key);
  if (m) return `Ctrl+${m[1].toUpperCase()}`;
  if (key.length === 1) return `Type "${key}"`;
  throw new Error(`unsupported tape key: ${key}`);
}

/**
 * Builds a VHS `Wait+Screen` regex from a recipe's plain-text anchor. Go-regexp
 * metacharacters are escaped, then every run of whitespace becomes `\s+` so the
 * pattern still matches when VHS's headless terminal reflows the text across a
 * line wrap (the boss output is identical to the validator's; only the wrapping
 * can differ). Pure — no fs/env/Date.
 * @param {string} text
 * @returns {string}
 */
export function vhsWaitRegex(text) {
  return String(text ?? '')
    .replace(/[.*+?^${}()|[\]\\/]/g, '\\$&')
    .replace(/\s+/g, '\\s+');
}

export function buildTape({ recipe, launcherCmd, outputPath }) {
  if (!/^[a-z0-9][a-z0-9-]*$/.test(String(recipe?.id ?? ''))) {
    throw new Error(`invalid recipe id for tape: ${recipe?.id ?? '<missing>'}`);
  }
  const cols = recipe.terminal?.width ?? 140;
  const rows = recipe.terminal?.height ?? 36;
  const fontSize = 14;
  const padding = 10;
  // Generous px sizing so boss has at least `cols`x`rows` to render into.
  const width = cols * 9 + padding * 2;
  const height = rows * 19 + padding * 2;
  const frameDelay = recipe.frameDelayMs ?? 650; // was 400 — a little slower to watch
  const playbackSpeed = recipe.playbackSpeed ?? 0.65; // VHS final-render slowdown
  const bootSleep = 2500;

  const lines = [
    // VHS requires the Output path quoted; an unquoted absolute path (with
    // leading slash / path separators) trips its lexer ("Invalid command").
    `Output "${outputPath}"`,
    `Set FontSize ${fontSize}`,
    `Set Width ${width}`,
    `Set Height ${height}`,
    `Set Padding ${padding}`,
    `Set PlaybackSpeed ${playbackSpeed}`,
    // Hide the launcher typing + boot so the recording opens on the first real
    // app frame (not "> proof/tui/run-fixture.sh demo").
    'Hide',
    `Type "${launcherCmd}"`,
    'Enter',
    `Sleep ${bootSleep}ms`,
    'Show',
  ];

  const steps = Array.isArray(recipe.steps) ? recipe.steps : [];
  for (const step of steps) {
    // Gate the keypress on the prior screen being ready, then wait for the
    // target screen to actually render before the dwell Sleep. This keeps the
    // recording synced to real transitions instead of a blind fixed delay: a
    // screen slower than `frameDelay` no longer desyncs the recorded keystrokes
    // (the previous behaviour, which emitted only `Sleep`).
    if (step.waitForReadyText) {
      lines.push(`Wait+Screen@${VHS_WAIT_TIMEOUT_MS}ms /${vhsWaitRegex(step.waitForReadyText)}/`);
    }
    for (const key of step.keys ?? []) {
      lines.push(vhsDirectiveForKey(key));
    }
    if (step.waitForText) {
      lines.push(`Wait+Screen@${VHS_WAIT_TIMEOUT_MS}ms /${vhsWaitRegex(step.waitForText)}/`);
    }
    lines.push(`Sleep ${frameDelay}ms`);
  }

  return lines.join('\n') + '\n';
}
