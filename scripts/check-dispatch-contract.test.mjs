#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  checkDispatchContract,
  discoverScanFiles,
  EXCLUDED_PREFIXES,
  EXTRA_SCAN_ROOTS,
  resolveScanRoots,
  scanDispatchContractText,
} from './check-dispatch-contract.mjs'
import { SKILL_ROOTS } from './check-skill-shell.mjs'

const fixture = (...lines) => lines.join('\n') + '\n'

function makeTempRepo() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-dispatch-contract-'))
  for (const dir of [
    'services/boss/internal/skillinstall/skills/boss-build',
    '.claude/skills/boss-review',
    '.codex/skills/boss-review',
    'plugins/bossd-plugin-claude/skilldata/skills/boss-review',
    'docs/plans',
    'services/bossd/internal/session',
  ]) {
    fs.mkdirSync(path.join(root, dir), { recursive: true })
  }
  fs.writeFileSync(path.join(root, 'CLAUDE.md'), '')
  fs.writeFileSync(path.join(root, 'services/bossd/internal/session/tmux_chat.go'), '')
  return root
}

test('flags an affirmative claim that subagent dispatch accepts run_in_background', () => {
  const findings = scanDispatchContractText(
    fixture(
      'Dispatch the reviewer with the Agent tool.',
      'The dispatch call signature is `prompt` / `run_in_background` / `subagent_type`.',
    ),
  )

  assert.deepEqual(findings, [
    {
      line: 2,
      text: 'The dispatch call signature is `prompt` / `run_in_background` / `subagent_type`.',
    },
  ])
})

test('does not flag Bash-tool run_in_background prose', () => {
  const findings = scanDispatchContractText(
    fixture(
      'The Bash tool exposes `run_in_background`; this is unrelated to subagent dispatch.',
      'Use it only for shell jobs whose launcher result is not the job result.',
    ),
  )

  assert.deepEqual(findings, [])
})

test('does not flag a prohibition fixture', () => {
  const findings = scanDispatchContractText(
    fixture('Never `run_in_background` a subagent; wait for its completion notification.'),
  )

  assert.deepEqual(findings, [])
})

test('main checker returns findings and non-zero semantics for an affirmative fixture', () => {
  const repoRoot = makeTempRepo()
  const file = path.join(repoRoot, 'CLAUDE.md')
  fs.writeFileSync(
    file,
    fixture('The Agent dispatch signature includes `run_in_background` for foreground execution.'),
  )

  const findings = checkDispatchContract({ repoRoot })

  assert.equal(findings.length, 1)
  assert.equal(findings[0].file, 'CLAUDE.md')
})

test('scan roots import SKILL_ROOTS and exclude mirrors and historical plans', () => {
  const repoRoot = makeTempRepo()
  const roots = resolveScanRoots(repoRoot).map((root) => path.relative(repoRoot, root))

  assert.deepEqual(roots, [...SKILL_ROOTS, ...EXTRA_SCAN_ROOTS])
  assert.ok(EXCLUDED_PREFIXES.includes('plugins/bossd-plugin-claude/skilldata/'))
  assert.ok(EXCLUDED_PREFIXES.includes('.codex/'))
  assert.ok(EXCLUDED_PREFIXES.includes('docs/plans/'))
})

test('file discovery skips generated mirrors and docs/plans', () => {
  const repoRoot = makeTempRepo()
  fs.writeFileSync(
    path.join(repoRoot, 'services/boss/internal/skillinstall/skills/boss-build/SKILL.md'),
    'Agent dispatch signature includes `run_in_background`.\n',
  )
  fs.writeFileSync(
    path.join(repoRoot, '.codex/skills/boss-review/SKILL.md'),
    'Agent dispatch signature includes `run_in_background`.\n',
  )
  fs.writeFileSync(
    path.join(repoRoot, 'plugins/bossd-plugin-claude/skilldata/skills/boss-review/SKILL.md'),
    'Agent dispatch signature includes `run_in_background`.\n',
  )
  fs.writeFileSync(
    path.join(repoRoot, 'docs/plans/historical.md'),
    'Agent dispatch signature includes `run_in_background`.\n',
  )

  const files = discoverScanFiles(repoRoot).map((file) => path.relative(repoRoot, file))
  const findings = checkDispatchContract({ repoRoot })

  assert.ok(files.includes('services/boss/internal/skillinstall/skills/boss-build/SKILL.md'))
  assert.ok(!files.includes('.codex/skills/boss-review/SKILL.md'))
  assert.ok(!files.includes('plugins/bossd-plugin-claude/skilldata/skills/boss-review/SKILL.md'))
  assert.ok(!files.includes('docs/plans/historical.md'))
  assert.deepEqual(
    findings.map((finding) => finding.file),
    ['services/boss/internal/skillinstall/skills/boss-build/SKILL.md'],
  )
})

test('real tree is clean', () => {
  assert.deepEqual(checkDispatchContract(), [])
})
