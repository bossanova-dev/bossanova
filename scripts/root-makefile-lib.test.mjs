import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { rootMakefileSources, readRootMakefile } from './root-makefile-lib.mjs'

for (const [name, root, fragments, expected, throws] of [
  ['no includes', 'all:\n', {}, ['Makefile']],
  [
    'fragments in include order',
    'include mk/a.mk mk/b.mk\n',
    { 'mk/a.mk': 'a:\n', 'mk/b.mk': 'b:\n' },
    ['Makefile', 'mk/a.mk', 'mk/b.mk'],
  ],
  [
    'anchored includes',
    'include $(dir $(lastword $(MAKEFILE_LIST)))mk/a.mk\n',
    { 'mk/a.mk': 'a:\n' },
    ['Makefile', 'mk/a.mk'],
  ],
  ['optional missing include', '-include absent.mk\n', {}, ['Makefile']],
  ['required missing include', 'include absent.mk\n', {}, [], true],
]) {
  test(name, (t) => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'root-makefile-'))
    t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
    fs.writeFileSync(path.join(dir, 'Makefile'), root)
    for (const [file, text] of Object.entries(fragments)) {
      fs.mkdirSync(path.dirname(path.join(dir, file)), { recursive: true })
      fs.writeFileSync(path.join(dir, file), text)
    }
    if (throws) return assert.throws(() => rootMakefileSources(dir), /absent\.mk/)
    const sources = rootMakefileSources(dir)
    assert.deepEqual(
      sources.map(({ path: file }) => file),
      expected,
    )
    assert.equal(readRootMakefile(dir), sources.map(({ text }) => text).join('\n'))
  })
}
