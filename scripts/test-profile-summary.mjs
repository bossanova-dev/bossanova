#!/usr/bin/env node

import fs from 'node:fs';

function normalizeLimit(limit) {
  const parsed = Number(limit);
  if (!Number.isFinite(parsed)) return 20;
  return Math.max(0, Math.trunc(parsed));
}

export function summarizeGoTestJson(input, limit = 20) {
  const events = [];
  for (const line of input.split('\n')) {
    if (!line.trim()) continue;
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      continue;
    }
    if (event.Action !== 'pass' || typeof event.Elapsed !== 'number') continue;
    if (!event.Package) continue;
    if (event.Test) {
      events.push({ elapsed: event.Elapsed, label: `test ${event.Package} ${event.Test}` });
    } else {
      events.push({ elapsed: event.Elapsed, label: `package ${event.Package}` });
    }
  }
  return events
    .sort((a, b) => b.elapsed - a.elapsed)
    .slice(0, normalizeLimit(limit))
    .map((event) => `${event.elapsed.toFixed(2)}s ${event.label}`)
    .join('\n');
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const file = process.argv[2];
  const limit = Number(process.env.LIMIT || '20');
  const input = file ? fs.readFileSync(file, 'utf8') : fs.readFileSync(0, 'utf8');
  console.log(summarizeGoTestJson(input, limit));
}
