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
  const frameDelay = recipe.frameDelayMs ?? 400;
  // Boot covers daemon start + boss's first render. The orchestrator pre-builds
  // the binaries (see proof.mjs), so this need not cover a `go build`.
  const bootSleep = 2500;

  const lines = [
    // VHS requires the Output path quoted; an unquoted absolute path (with
    // leading slash / path separators) trips its lexer ("Invalid command").
    `Output "${outputPath}"`,
    `Set FontSize ${fontSize}`,
    `Set Width ${width}`,
    `Set Height ${height}`,
    `Set Padding ${padding}`,
    `Type "${launcherCmd}"`,
    'Enter',
    `Sleep ${bootSleep}ms`,
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
