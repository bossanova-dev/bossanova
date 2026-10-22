import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtempSync, mkdirSync, rmSync, realpathSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { resolveCheckout, NOT_A_CHECKOUT_EXIT } from './git-checkout.mjs'

test('resolves a checkout and common directory from a nested directory', () => {
  const dir = realpathSync(mkdtempSync(path.join(tmpdir(), 'git-checkout-')))
  try {
    assert.equal(spawnSync('git', ['init', dir]).status, 0)
    const nested = path.join(dir, 'a', 'b')
    mkdirSync(nested, { recursive: true })
    assert.deepEqual(resolveCheckout({ cwd: nested }), {
      root: dir,
      commonDir: path.join(dir, '.git'),
    })
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})
test('rejects a non-checkout with stable reason and exit code', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'git-checkout-'))
  try {
    assert.throws(
      () =>
        resolveCheckout({
          cwd: dir,
          run: (cmd, args, opts) =>
            spawnSync(cmd, args, {
              ...opts,
              env: { ...process.env, GIT_CEILING_DIRECTORIES: dir },
            }),
        }),
      { reason: 'not-a-checkout', exitCode: NOT_A_CHECKOUT_EXIT },
    )
    assert.equal(NOT_A_CHECKOUT_EXIT, 3)
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})
test('classifies unavailable git', () => {
  assert.throws(() => resolveCheckout({ run: () => ({ error: { code: 'ENOENT' } }) }), {
    reason: 'git-unavailable',
    exitCode: 3,
  })
})
