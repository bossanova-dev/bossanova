import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import test from 'node:test'
import { checkBossTuiScreensIsolated } from './boss-tui-screens-isolated.mjs'
import { checkBossTuiScreensNoAdapters } from './boss-tui-screens-no-adapters.mjs'

function write(root, file, text) {
  fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
  fs.writeFileSync(path.join(root, file), text)
  execFileSync('git', ['add', file], { cwd: root })
}
function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-screen-guard-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  execFileSync('git', ['init', '-q'], { cwd: root })
  write(root, '.golangci.yml', fs.readFileSync('.golangci.yml', 'utf8'))
  write(root, 'baseline.json', '[]\n')
  return root
}
for (const [id, check, forbidden] of [
  ['isolated', checkBossTuiScreensIsolated, 'github.com/recurser/boss/api/tui/peer'],
  ['no-adapters', checkBossTuiScreensNoAdapters, 'os/exec'],
]) {
  test(`boss screen ${id} rejects new violations and permits ui, tests and router wiring`, (t) => {
    const repoRoot = fixture(t)
    const run = () => check({ repoRoot, baselinePath: 'baseline.json' })
    for (const file of ['api/tui/ui/nav.go', 'api/tui/router.go', 'api/tui/screen/view_test.go'])
      write(repoRoot, `services/boss/${file}`, `package fixture\nimport _ "${forbidden}"\n`)
    write(
      repoRoot,
      'services/boss/api/tui/screen/view.go',
      'package fixture\nimport _ "github.com/recurser/boss/api/tui/ui"\n',
    )
    assert.deepEqual(run().failures, [])
    write(
      repoRoot,
      'services/boss/api/tui/future/nested/view.go',
      `package fixture\nimport _ "${forbidden}"\n`,
    )
    assert.equal(run().failures.length, 1)
    assert.deepEqual(run().offenders, ['services/boss/api/tui/future/nested/view.go'])
  })
  test(`boss screen ${id} baseline generation is idempotent and detects stale exemptions`, (t) => {
    const repoRoot = fixture(t)
    const run = (write = false) => check({ repoRoot, baselinePath: 'baseline.json', write })
    const file = 'services/boss/api/tui/screen/view.go'
    write(repoRoot, file, `package fixture\nimport _ "${forbidden}"\n`)
    assert.deepEqual(run(true).failures, [])
    const config = fs.readFileSync(path.join(repoRoot, '.golangci.yml'), 'utf8')
    assert.deepEqual(run(true).failures, [])
    assert.equal(fs.readFileSync(path.join(repoRoot, '.golangci.yml'), 'utf8'), config)
    write(repoRoot, '.golangci.yml', config.replace(`!**/${file}`, '!**/wrong.go'))
    assert.match(run().failures.join('\n'), /generated.*stale/)
    run(true)
    write(repoRoot, file, 'package fixture\n')
    assert.match(run().failures.join('\n'), /no longer occurs/)
  })
}
test('boss screen isolation rejects a mode that loses the ui specificity override', (t) => {
  const repoRoot = fixture(t)
  write(
    repoRoot,
    '.golangci.yml',
    fs
      .readFileSync(path.join(repoRoot, '.golangci.yml'), 'utf8')
      .replace('list-mode: lax', 'list-mode: original'),
  )
  assert.match(
    checkBossTuiScreensIsolated({ repoRoot, baselinePath: 'baseline.json' }).failures.join('\n'),
    /list-mode must be lax/,
  )
})
