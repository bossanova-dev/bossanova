#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

const here = path.dirname(new URL(import.meta.url).pathname)
const scriptPath = path.join(here, 'check-build-drift.sh')

test('build-drift diagnostics do not use globally shared temporary files', () => {
  const script = fs.readFileSync(scriptPath, 'utf8')

  assert.doesNotMatch(
    script,
    /\/tmp\/(?:inventory|ledger)-drift\.diff/,
    'concurrent guard runs must not overwrite shared /tmp diff files',
  )
})
