import assert from 'node:assert/strict'
import { test } from 'node:test'

import { describeMoves } from './verify-describe-moves.mjs'
import { envelopeFor, gitFixture, runDescriberScript } from './verify-git-fixture.mjs'

const MODULE = new URL('./verify-describe-moves.mjs', import.meta.url)

const BODY = Array.from({ length: 12 }, (_, i) => `// line ${i}`).join('\n') + '\n'

function movedRepo(t, { behaviour = false } = {}) {
  const repo = gitFixture(t)
  repo.write({
    'src/widgets/Button.tsx': BODY + 'export const Button = () => null\n',
    'src/widgets/Card.tsx': 'export const Card = () => null\n',
    'src/pages/Home.tsx':
      "import { Button } from '../widgets/Button'\nimport { Card } from '../widgets/Card'\n\nexport const Home = () => Button\n",
  })
  const base = repo.commit('base')
  repo.move('src/widgets/Button.tsx', 'src/ui/Button.tsx')
  repo.move('src/widgets/Card.tsx', 'src/ui/Card.tsx')
  if (behaviour) repo.write({ 'src/ui/Button.tsx': BODY + 'export const Button = () => 1\n' })
  repo.write({
    'src/pages/Home.tsx':
      "import { Card } from '../ui/Card'\nimport { Button } from '../ui/Button'\n\nexport const Home = () => " +
      (behaviour ? 'Card\n' : 'Button\n'),
  })
  return { repo, base, head: repo.commit('move') }
}

test('a pure move reports moved and import-only files, valid and never breaking', (t) => {
  const { repo, base, head } = movedRepo(t)
  const result = runDescriberScript(
    MODULE,
    envelopeFor(repo.dir, base, head, 'housekeeping'),
    repo.dir,
  )
  assert.equal(result.breaking, false)
  assert.equal(result.items.length, 2)
  assert.ok(result.items.some((item) => item.includes('2')))
  assert.ok(result.items.some((item) => item.includes('1')))
})

test('a move that also changes behaviour reports the impure files', (t) => {
  const { repo, base, head } = movedRepo(t, { behaviour: true })
  const ui = envelopeFor(repo.dir, base, head, 'ui')
  assert.deepEqual(ui.area.paths, [
    'src/pages/Home.tsx',
    'src/ui/Button.tsx',
    'src/widgets/Button.tsx',
  ])
  const result = describeMoves(ui, { cwd: repo.dir })
  assert.equal(result.breaking, false)
  assert.equal(result.items.length, 2)
  assert.ok(result.items.some((item) => item.includes('2')))
})

test('an area with no renames or import-only files gets no items', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'src/main.go': 'package main\n' })
  const base = repo.commit()
  repo.write({ 'src/main.go': 'package main\n\nfunc main() {}\n' })
  const head = repo.commit()
  const result = runDescriberScript(MODULE, envelopeFor(repo.dir, base, head, 'other'), repo.dir)
  assert.deepEqual(result.items, [])
  assert.equal(result.breaking, false)
})
