import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { compareToolbox, hashFile, main, resolveSourceDir } from './toolbox-drift.mjs'

const HELPER = fileURLToPath(new URL('./toolbox-drift.mjs', import.meta.url))

function scratch(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'toolbox-drift-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  const installedDir = path.join(root, 'installed')
  const sourceDir = path.join(root, 'source')
  fs.mkdirSync(installedDir)
  fs.mkdirSync(sourceDir)
  const write = (dir, name, body, mode = 0o644) => {
    const file = path.join(dir, name)
    fs.mkdirSync(path.dirname(file), { recursive: true })
    fs.writeFileSync(file, body)
    fs.chmodSync(file, mode)
    return file
  }
  const both = (name, body, mode) => {
    write(installedDir, name, body, mode)
    write(sourceDir, name, body, mode)
  }
  return { root, installedDir, sourceDir, write, both }
}

function capture() {
  const chunks = []
  return { write: (chunk) => chunks.push(chunk), text: () => chunks.join('') }
}

test('a toolbox that matches its source reports no drift and prints nothing', (t) => {
  const fx = scratch(t)
  fx.both('skill-config.mjs', 'export const a = 1\n')
  fx.both('worktree-lock.sh', '#!/bin/sh\n', 0o755)

  const report = compareToolbox({ installedDir: fx.installedDir, sourceDir: fx.sourceDir })
  assert.equal(report.sourcePresent, true)
  assert.equal(report.checked, 2)
  assert.deepEqual(report.drifted, [])

  const stderr = capture()
  assert.equal(
    main(['check', '--toolbox', fx.installedDir, '--source', fx.sourceDir], { stderr }),
    0,
  )
  assert.equal(stderr.text(), '')
})

test('a single changed byte is reported once, as content drift, still exiting 0', (t) => {
  const fx = scratch(t)
  fx.both('skill-config.mjs', 'export const a = 1\n')
  fx.both('plan-attachment.mjs', 'export const b = 2\n')
  fx.write(fx.installedDir, 'skill-config.mjs', 'export const a = 2\n')

  const report = compareToolbox({ installedDir: fx.installedDir, sourceDir: fx.sourceDir })
  assert.deepEqual(report.drifted, [{ file: 'skill-config.mjs', reason: 'content' }])

  const stderr = capture()
  assert.equal(
    main(['check', '--toolbox', fx.installedDir, '--source', fx.sourceDir], { stderr }),
    0,
  )
  const lines = stderr.text().trimEnd().split('\n')
  assert.equal(lines.length, 1)
  assert.match(lines[0], /^boss-toolbox-drift: 1 of 2 installed helpers differ from source /)
  assert.match(lines[0], /\(skill-config\.mjs\)/)
  assert.match(lines[0], /continuing\.$/)
})

// An installed copy that lost its executable bit hashes identical to source yet fails the
// moment the skill invokes it directly, so the mode must be part of the comparison.
test('a cleared executable bit is drift even when the bytes match', (t) => {
  const fx = scratch(t)
  fx.both('worktree-lock.sh', '#!/bin/sh\necho hi\n', 0o755)
  fs.chmodSync(path.join(fx.installedDir, 'worktree-lock.sh'), 0o644)

  const report = compareToolbox({ installedDir: fx.installedDir, sourceDir: fx.sourceDir })
  assert.deepEqual(report.drifted, [{ file: 'worktree-lock.sh', reason: 'mode' }])
  assert.equal(
    hashFile(path.join(fx.installedDir, 'worktree-lock.sh')),
    hashFile(path.join(fx.sourceDir, 'worktree-lock.sh')),
  )
})

// The other direction is not symmetric. Installers routinely add an executable bit the source
// does not carry — on this machine 9 helpers across 4 installed skills sit at 755 against a
// 644 source, byte-identical. Reporting that would fire on nearly every clean install and
// spend the single warning line on something nobody can act on.
test('an executable bit the install added, but source lacks, is not drift', (t) => {
  const fx = scratch(t)
  fx.both('plan-image-guard.mjs', 'export const a = 1\n', 0o644)
  fs.chmodSync(path.join(fx.installedDir, 'plan-image-guard.mjs'), 0o755)

  const report = compareToolbox({ installedDir: fx.installedDir, sourceDir: fx.sourceDir })
  assert.equal(report.checked, 1)
  assert.deepEqual(report.drifted, [])

  const stderr = capture()
  assert.equal(
    main(['check', '--toolbox', fx.installedDir, '--source', fx.sourceDir], { stderr }),
    0,
  )
  assert.equal(stderr.text(), '')
})

test('an installed helper with no counterpart in source is drift', (t) => {
  const fx = scratch(t)
  fx.both('skill-config.mjs', 'export const a = 1\n')
  fx.write(fx.installedDir, 'nested/stale-helper.mjs', 'export const gone = true\n')

  const report = compareToolbox({ installedDir: fx.installedDir, sourceDir: fx.sourceDir })
  assert.deepEqual(report.drifted, [
    { file: 'nested/stale-helper.mjs', reason: 'absent-from-source' },
  ])
})

// The subset rule: each skill vendors a deliberate subset of the source helpers, so the
// installed tree — not the source tree — is the enumeration domain. Enumerating the source
// would flag every un-vendored helper as drift on a perfectly clean install.
test('a source helper that this toolbox does not install is not drift', (t) => {
  const fx = scratch(t)
  fx.both('skill-config.mjs', 'export const a = 1\n')
  fx.write(fx.sourceDir, 'dag-scheduler.mjs', 'export const notVendoredHere = true\n')

  const report = compareToolbox({ installedDir: fx.installedDir, sourceDir: fx.sourceDir })
  assert.equal(report.checked, 1)
  assert.deepEqual(report.drifted, [])
})

// Every real preflight passes no --source, so the walk-up is the branch that actually runs.
// Prove it by INJECTING drift and seeing it reported: silence is this probe's healthy output,
// so a test that only asserts exit 0 would keep passing after the walk stopped finding
// anything at all.
test('with no --source, the probe walks up from cwd and finds the helper source', (t) => {
  const fx = scratch(t)
  const sourceDir = path.join(fx.root, 'skills-toolbox')
  fs.renameSync(fx.sourceDir, sourceDir)
  fx.write(sourceDir, 'skill-config.mjs', 'export const a = 1\n')
  fx.write(fx.installedDir, 'skill-config.mjs', 'export const a = 2\n')
  const deep = path.join(fx.root, 'services', 'boss', 'internal')
  fs.mkdirSync(deep, { recursive: true })

  assert.equal(resolveSourceDir({ cwd: deep, env: {} }), sourceDir)

  const stderr = capture()
  assert.equal(main(['check', '--toolbox', fx.installedDir], { stderr, cwd: deep, env: {} }), 0)
  assert.match(stderr.text(), /boss-toolbox-drift: 1 of 1 /)
  assert.match(stderr.text(), /\(skill-config\.mjs\)/)
})

test('cwd with no source directory above it resolves to null, not an error', (t) => {
  const fx = scratch(t)
  assert.equal(resolveSourceDir({ cwd: fx.installedDir, env: {} }), null)
})

// The env var is how a caller points the probe at a source tree it cannot walk up to.
test('BOSS_TOOLBOX_SOURCE names the source, and an invalid one resolves to null', (t) => {
  const fx = scratch(t)
  fx.both('skill-config.mjs', 'export const a = 1\n')
  fx.write(fx.installedDir, 'skill-config.mjs', 'export const a = 2\n')
  const env = { BOSS_TOOLBOX_SOURCE: fx.sourceDir }

  assert.equal(resolveSourceDir({ env }), fx.sourceDir)
  assert.equal(resolveSourceDir({ env: { BOSS_TOOLBOX_SOURCE: path.join(fx.root, 'nope') } }), null)

  const stderr = capture()
  assert.equal(main(['check', '--toolbox', fx.installedDir], { stderr, env }), 0)
  assert.match(stderr.text(), /boss-toolbox-drift: 1 of 1 /)
})

// An explicit --source is a deliberate instruction, so a bad one must NOT quietly fall back to
// the walk-up and compare against some unrelated tree the caller never named.
test('an explicit source path that does not exist is silent, not an error', (t) => {
  const fx = scratch(t)
  fx.both('skill-config.mjs', 'export const a = 1\n')
  fx.write(fx.installedDir, 'skill-config.mjs', 'export const a = 999\n')

  assert.equal(resolveSourceDir({ explicit: path.join(fx.root, 'no-such-source') }), null)
  const stderr = capture()
  const code = main(
    ['check', '--toolbox', fx.installedDir, '--source', path.join(fx.root, 'no-such-source')],
    { stderr },
  )
  assert.equal(code, 0)
  assert.equal(stderr.text(), '')
})

// Installed skill homes are routinely reached through a symlink; printing the path as typed
// invites an investigation into two names for one directory.
test('the drift line names the resolved installed directory, not the symlink', (t) => {
  const fx = scratch(t)
  fx.both('skill-config.mjs', 'export const a = 1\n')
  fx.write(fx.installedDir, 'skill-config.mjs', 'export const a = 2\n')
  const link = path.join(fx.root, 'linked-toolbox')
  fs.symlinkSync(fx.installedDir, link, 'dir')

  const stderr = capture()
  assert.equal(main(['check', '--toolbox', link, '--source', fx.sourceDir], { stderr }), 0)
  // Substring, not a RegExp: a temp path carries regex metacharacters on some platforms.
  assert.ok(stderr.text().includes(`installed=${fs.realpathSync(fx.installedDir)} `))
  assert.doesNotMatch(stderr.text(), /linked-toolbox/)
})

test('an unreadable installed toolbox exits 0 without a warning', (t) => {
  const fx = scratch(t)
  fx.both('skill-config.mjs', 'export const a = 1\n')
  const notADirectory = path.join(fx.installedDir, 'skill-config.mjs')

  for (const toolbox of [notADirectory, path.join(fx.root, 'never-installed')]) {
    const stderr = capture()
    assert.equal(main(['check', '--toolbox', toolbox, '--source', fx.sourceDir], { stderr }), 0)
    assert.equal(stderr.text(), '')
  }
})

// The probe is vendored into more than one skill, so it must not name any of them.
test('the helper names no individual skill', () => {
  const source = fs.readFileSync(HELPER, 'utf8')
  for (const core of ['boss-plan', 'boss-build', 'boss-review', 'boss-epic', 'boss-repair']) {
    assert.equal(source.includes(core), false, `toolbox-drift.mjs must not name ${core}`)
  }
})
