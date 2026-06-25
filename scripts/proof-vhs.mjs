#!/usr/bin/env node

const VHS_KEY = { enter: 'Enter', esc: 'Escape' };

function vhsDirectiveForKey(key) {
  if (VHS_KEY[key]) return VHS_KEY[key];
  const m = /^ctrl\+([a-z])$/.exec(key);
  if (m) return `Ctrl+${m[1].toUpperCase()}`;
  if (key.length === 1) return `Type "${key}"`;
  throw new Error(`unsupported tape key: ${key}`);
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
    for (const key of step.keys ?? []) {
      lines.push(vhsDirectiveForKey(key));
    }
    lines.push(`Sleep ${frameDelay}ms`);
  }

  return lines.join('\n') + '\n';
}
