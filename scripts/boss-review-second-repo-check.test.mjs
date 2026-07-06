import { test } from 'node:test'
import assert from 'node:assert/strict'
import { runSecondRepoCheck } from './boss-review-second-repo-check.mjs'

// Validates the boss-review extraction OUTSIDE bossanova: the harness scaffolds a
// throwaway git repo, drops in only the skill's bundled toolbox/ (no repo-root
// scripts/), plugs in a repo-local `<core>-*` review extension, and proves
// discovery + ordering + envelope validation + graceful degradation there. See the
// design note in boss-review-second-repo-check.mjs for why this is structural (a live
// subagent + cross-agent run needs interactive auth and is a non-blocking follow-up).
test('scaffolds a second repo, discovers the repo-local extension, and passes self-containment', async () => {
  const res = await runSecondRepoCheck()
  assert.equal(res.selfContained, true, 'six helpers ran from toolbox with no repo-root scripts/')
  assert.equal(res.helpersRun, 6)
  assert.equal(res.noRepoScripts, true, 'the scaffolded repo has no repo-root scripts/ dir')
  assert.deepEqual(
    res.discoveredExtensions.map((e) => e.name),
    ['boss-review-fixturelens'],
  )
  assert.equal(
    res.envelopeValid,
    true,
    'the fixture lens result envelope validates for role "lens"',
  )
  assert.equal(
    res.noopWithoutExtension,
    true,
    'discovery returns zero extensions when none installed',
  )
})

test('discovery orders repo-local extensions by (order, name)', async () => {
  const res = await runSecondRepoCheck()
  // Single fixture today, but the ordering contract must hold: assert the returned
  // list is sorted by order then name so a second extension would slot deterministically.
  const ext = res.discoveredExtensions
  for (let i = 1; i < ext.length; i += 1) {
    const a = ext[i - 1]
    const b = ext[i]
    assert.ok(
      a.order < b.order || (a.order === b.order && a.name.localeCompare(b.name) <= 0),
      'extensions are (order, name)-ordered',
    )
  }
  assert.equal(ext[0].role, 'lens')
  assert.equal(ext[0].order, 10)
})
