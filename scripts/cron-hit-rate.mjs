#!/usr/bin/env node
// Compose deterministic expected-fire counts with observed hit counts.

import { expectedFires as countExpectedFires } from './cron-schedule.mjs'

export function cronHitRate({ slug, schedule, from, to, observedHits }) {
  const expectedFires = countExpectedFires(schedule, from, to)
  const observed = Number(observedHits)
  return {
    slug,
    schedule,
    expectedFires,
    observedHits: observed,
    hitRate: expectedFires && expectedFires > 0 ? observed / expectedFires : null,
  }
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const [slug, schedule, from, to, observedHits] = process.argv.slice(2)
  if (!slug || !schedule || !from || !to || observedHits === undefined) {
    console.error(
      'usage: node scripts/cron-hit-rate.mjs <slug> <schedule> <from> <to> <observedHits>',
    )
    process.exit(2)
  }
  console.log(JSON.stringify(cronHitRate({ slug, schedule, from, to, observedHits }), null, 2))
}
