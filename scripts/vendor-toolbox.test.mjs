import { test } from 'node:test'
import assert from 'node:assert/strict'
import { VENDOR_MAP, vendorToolbox, withSkillSourceRewriteLock } from './vendor-toolbox.mjs'
import {
  SKILL_SOURCE_REWRITE_GENERATION,
  SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX,
} from './skill-source-rewrite-lock.mjs'
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  rmSync,
  chmodSync,
  statSync,
  existsSync,
  utimesSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, relative } from 'node:path'
import { execFileSync, spawn } from 'node:child_process'
import { once } from 'node:events'
import { fileURLToPath, pathToFileURL } from 'node:url'

const REPO_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')

async function waitFor(predicate, timeoutMs = 1_000) {
  const deadline = Date.now() + timeoutMs
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error('timed out waiting for condition')
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
}

test('withSkillSourceRewriteLock guards canonical source rewrites', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const generationPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', SKILL_SOURCE_REWRITE_GENERATION], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )

  withSkillSourceRewriteLock(root, () => {
    assert.ok(existsSync(lockPath), 'source rewrite lock must cover the write')
  })
  assert.ok(!existsSync(lockPath), 'source rewrite lock must be removed after the write')
  assert.ok(existsSync(generationPath), 'completed rewrites must leave a persistent generation')
  rmSync(root, { recursive: true, force: true })
})

test('withSkillSourceRewriteLock keeps dirty generations until a payload repair succeeds', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const generationPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', SKILL_SOURCE_REWRITE_GENERATION], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const dirty = `${SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX}interrupted`
  writeFileSync(generationPath, `${dirty}\n`)

  withSkillSourceRewriteLock(root, () => {})
  assert.equal(
    readFileSync(generationPath, 'utf8'),
    `${dirty}\n`,
    'an unrelated subset rewrite must not clear a dirty payload marker',
  )

  assert.throws(() =>
    withSkillSourceRewriteLock(root, () => {
      throw new Error('partial rewrite')
    }),
  )
  assert.match(
    readFileSync(generationPath, 'utf8'),
    new RegExp(`^${SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX}`),
    'a failed rewrite must leave the payload dirty',
  )

  withSkillSourceRewriteLock(root, () => {}, { repairsPayload: true })
  assert.doesNotMatch(
    readFileSync(generationPath, 'utf8'),
    new RegExp(`^${SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX}`),
    'only a successful whole-payload repair may clear the dirty marker',
  )
  rmSync(root, { recursive: true, force: true })
})

test('skill-source-rewrite-lock CLI clears a dirty generation only after a successful payload repair', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const generationPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', SKILL_SOURCE_REWRITE_GENERATION], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const lockScript = join(process.cwd(), 'scripts', 'skill-source-rewrite-lock.mjs')
  writeFileSync(generationPath, `${SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX}interrupted\n`)

  execFileSync(
    process.execPath,
    [lockScript, '--repo-root', root, '--repairs-payload', '--', process.execPath, '-e', ''],
    { stdio: 'ignore' },
  )
  assert.doesNotMatch(
    readFileSync(generationPath, 'utf8'),
    new RegExp(`^${SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX}`),
    'a successful explicit payload repair must clear the dirty marker',
  )

  writeFileSync(generationPath, `${SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX}interrupted\n`)
  assert.throws(() =>
    execFileSync(
      process.execPath,
      [
        lockScript,
        '--repo-root',
        root,
        '--repairs-payload',
        '--',
        process.execPath,
        '-e',
        'process.exit(1)',
      ],
      { stdio: 'ignore' },
    ),
  )
  assert.match(
    readFileSync(generationPath, 'utf8'),
    new RegExp(`^${SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX}`),
    'a failed repair command must retain an incomplete payload marker',
  )
  rmSync(root, { recursive: true, force: true })
})

test('withSkillSourceRewriteLock marks a reclaimed abandoned rewrite dirty', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const generationPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', SKILL_SOURCE_REWRITE_GENERATION], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  writeFileSync(lockPath, '')

  withSkillSourceRewriteLock(root, () => {})
  assert.match(
    readFileSync(generationPath, 'utf8'),
    new RegExp(`^${SKILL_SOURCE_REWRITE_GENERATION_DIRTY_PREFIX}`),
    'a replacement writer must not hide an abandoned predecessor',
  )
  rmSync(root, { recursive: true, force: true })
})

test('withSkillSourceRewriteLock publishes ownership before a contender can reclaim it', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  let attemptedContention = false

  withSkillSourceRewriteLock(
    root,
    () => {
      assert.ok(existsSync(lockPath), 'owner must retain a published lock while writing')
    },
    {
      onBeforePublish: () => {
        attemptedContention = true
        withSkillSourceRewriteLock(root, () => {})
      },
    },
  )

  assert.equal(attemptedContention, true, 'test seam must exercise pre-publication contention')
  assert.ok(!existsSync(lockPath), 'source rewrite lock must be removed after the write')
  rmSync(root, { recursive: true, force: true })
})

test('withSkillSourceRewriteLock waits for a live lock to be released', async () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const lockScript = join(process.cwd(), 'scripts', 'skill-source-rewrite-lock.mjs')
  const holder = spawn(
    process.execPath,
    [
      lockScript,
      '--repo-root',
      root,
      '--',
      process.execPath,
      '-e',
      'setTimeout(() => process.exit(0), 100)',
    ],
    { stdio: 'ignore' },
  )

  try {
    await waitFor(() => existsSync(lockPath))
    let actionRan = false
    withSkillSourceRewriteLock(root, () => {
      actionRan = true
    })
    assert.equal(actionRan, true)
    const [code] = await once(holder, 'exit')
    assert.equal(code, 0)
  } finally {
    holder.kill()
    rmSync(root, { recursive: true, force: true })
  }
})

test('withSkillSourceRewriteLock keeps an expired lock while its writer is live', async () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const releasedPath = join(root, 'released')
  const holder = spawn(
    process.execPath,
    [
      '-e',
      'setTimeout(() => { require("node:fs").writeFileSync(process.argv[2], "released"); require("node:fs").rmSync(process.argv[1], { force: true }) }, 200)',
      lockPath,
      releasedPath,
    ],
    { stdio: 'ignore' },
  )

  try {
    writeFileSync(lockPath, JSON.stringify({ createdAt: 0, pid: holder.pid, token: 'live-writer' }))
    withSkillSourceRewriteLock(root, () => {
      assert.ok(
        existsSync(releasedPath),
        'action must wait for the live writer to release its expired lock',
      )
    })
    const [code] = await once(holder, 'exit')
    assert.equal(code, 0)
  } finally {
    holder.kill()
    rmSync(root, { recursive: true, force: true })
  }
})

test('withSkillSourceRewriteLock reclaims an expired lock whose PID was reused', async () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const lockScript = join(process.cwd(), 'scripts', 'skill-source-rewrite-lock.mjs')
  const releasedPath = join(root, 'reclaimed')
  writeFileSync(
    lockPath,
    JSON.stringify({
      createdAt: 0,
      pid: process.pid,
      ownerStartTime: 'a different process start time',
      token: 'dead-writer',
    }),
  )
  const child = spawn(
    process.execPath,
    [
      '--input-type=module',
      '-e',
      `import { writeFileSync } from 'node:fs'; import { withSkillSourceRewriteLock } from ${JSON.stringify(lockScript)}; withSkillSourceRewriteLock(process.argv[1], () => writeFileSync(process.argv[2], 'reclaimed'))`,
      root,
      releasedPath,
    ],
    { stdio: 'ignore' },
  )
  let timeout

  try {
    await Promise.race([
      once(child, 'exit').then(([code]) =>
        assert.equal(code, 0, 'reused PID lock must be reclaimed'),
      ),
      new Promise(
        (_, reject) =>
          (timeout = setTimeout(
            () => reject(new Error('reused PID lock blocked the writer')),
            5_000,
          )),
      ),
    ])
    assert.ok(existsSync(releasedPath), 'writer must run after reclaiming the reused PID lock')
  } finally {
    clearTimeout(timeout)
    child.kill()
    rmSync(root, { recursive: true, force: true })
  }
})

test('withSkillSourceRewriteLock propagates pending-file collisions instead of retrying them', async () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockScript = join(process.cwd(), 'scripts', 'skill-source-rewrite-lock.mjs')
  const childScript = join(root, 'pending-file-collision.mjs')
  writeFileSync(
    childScript,
    `
      import { writeFileSync } from 'node:fs'
      import { execFileSync } from 'node:child_process'
      import { resolve } from 'node:path'
      import { withSkillSourceRewriteLock } from ${JSON.stringify(lockScript)}

      const root = process.argv[2]
      Date.now = () => 1
      Math.random = () => 0
      const gitPath = execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
        cwd: root,
        encoding: 'utf8',
      }).trim()
      const lockPath = resolve(root, gitPath)
      writeFileSync(\`${'${lockPath}'}.\${process.pid}-1-0.pending\`, '')
      withSkillSourceRewriteLock(root, () => {})
    `,
  )
  const child = spawn(process.execPath, [childScript, root], { stdio: 'ignore' })
  let timeout

  try {
    await Promise.race([
      once(child, 'exit').then(([code]) => {
        assert.notEqual(code, 0, 'pending-file failure must surface to the caller')
      }),
      new Promise(
        (_, reject) =>
          (timeout = setTimeout(
            () => reject(new Error('non-contention errors must not retry indefinitely')),
            5_000,
          )),
      ),
    ])
  } finally {
    clearTimeout(timeout)
    child.kill()
    rmSync(root, { recursive: true, force: true })
  }
})

test('make gen-skill runs the multi-file generator under the shared rewrite lock', () => {
  const makefile = readFileSync(join(process.cwd(), 'Makefile'), 'utf8')
  assert.match(
    makefile,
    /gen-skill:\n\tnode scripts\/skill-source-rewrite-lock\.mjs --repo-root "\$\(CURDIR\)" -- sh -c 'cd services\/boss && go run \.\/cmd gen-skill'/,
  )
})

test('services/boss format locks its complete canonical toolbox rewrite as a payload repair', () => {
  const makefile = readFileSync(join(process.cwd(), 'services', 'boss', 'Makefile'), 'utf8')
  assert.match(
    makefile,
    /format:\n\t@if \[ -n "\$\$\{BOSS_SKILL_SOURCE_REWRITE_LOCK_HELD:-\}" \]; then \\\n\t\t\$\(MAKE\) format-unlocked; \\\n\telse \\\n\t\tnode \.\.\/\.\.\/scripts\/skill-source-rewrite-lock\.mjs --repo-root "\.\.\/\.\." --repairs-payload -- \$\(MAKE\) -C services\/boss format-unlocked;/,
  )
  assert.match(makefile, /format-unlocked:\n\tgofmt -w \./)
})

test('withSkillSourceRewriteLock reclaims an abandoned legacy sentinel', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  writeFileSync(lockPath, '')

  withSkillSourceRewriteLock(root, () => {
    assert.ok(existsSync(lockPath), 'reclaimed lock must cover the write')
  })
  assert.ok(!existsSync(lockPath), 'reclaimed lock must be removed after the write')
  rmSync(root, { recursive: true, force: true })
})

test('withSkillSourceRewriteLock atomically publishes reclaim claim ownership', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  writeFileSync(lockPath, '')
  let published = false

  try {
    withSkillSourceRewriteLock(root, () => {}, {
      onClaimPublished: (claimPath) => {
        published = true
        assert.equal(claimPath, `${lockPath}.reclaim`)
        assert.equal(
          JSON.parse(readFileSync(join(claimPath, 'owner.json'), 'utf8')).pid,
          process.pid,
        )
      },
    })
    assert.equal(published, true, 'reclaim claim must be published with its owner')
  } finally {
    rmSync(root, { recursive: true, force: true })
  }
})

test('withSkillSourceRewriteLock reclaims an abandoned reclaim sidecar', async () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const claimPath = `${lockPath}.reclaim`
  const lockScript = join(process.cwd(), 'scripts', 'skill-source-rewrite-lock.mjs')
  writeFileSync(lockPath, '')
  mkdirSync(claimPath)
  utimesSync(claimPath, new Date(0), new Date(0))
  const child = spawn(
    process.execPath,
    [
      '--input-type=module',
      '-e',
      `import { withSkillSourceRewriteLock } from ${JSON.stringify(lockScript)}; withSkillSourceRewriteLock(process.argv[1], () => {})`,
      root,
    ],
    { stdio: 'ignore' },
  )
  let timeout

  try {
    await Promise.race([
      once(child, 'exit').then(([code]) => {
        assert.equal(code, 0, 'abandoned reclaim sidecar must not block writers')
      }),
      new Promise(
        (_, reject) =>
          (timeout = setTimeout(
            () => reject(new Error('abandoned reclaim sidecar blocked the writer')),
            5_000,
          )),
      ),
    ])
  } finally {
    clearTimeout(timeout)
    child.kill()
    rmSync(root, { recursive: true, force: true })
  }
})

test('withSkillSourceRewriteLock keeps a successor recovery marker', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const claimPath = `${lockPath}.reclaim`
  const recoveryPath = join(claimPath, '.recovering')
  const successor = JSON.stringify({ createdAt: Date.now(), pid: process.pid, token: 'successor' })
  writeFileSync(lockPath, '')
  mkdirSync(claimPath)
  utimesSync(claimPath, new Date(0), new Date(0))
  writeFileSync(recoveryPath, '')
  utimesSync(recoveryPath, new Date(0), new Date(0))
  utimesSync(claimPath, new Date(0), new Date(0))
  let successorActive = false

  try {
    withSkillSourceRewriteLock(root, () => {}, {
      onBeforeReclaim: () => {
        if (!successorActive) return
        assert.equal(
          readFileSync(recoveryPath, 'utf8'),
          successor,
          'successor recovery marker is retained while the contender waits',
        )
        // A reclaimer owns the enclosing sidecar for the lifetime of its
        // recovery marker, so its normal release removes both together.
        rmSync(claimPath, { recursive: true, force: true })
        successorActive = false
      },
      onBeforeReclaimRecovery: () => {
        // A successor can reuse the same filesystem entry (or be rewritten in
        // place), so its owner token must fence reclamation as well as inode.
        writeFileSync(recoveryPath, successor)
        successorActive = true
      },
    })
    assert.equal(successorActive, false, 'successor recovery marker is released before entry')
  } finally {
    rmSync(root, { recursive: true, force: true })
  }
})

test('withSkillSourceRewriteLock never reclaims a successor sidecar', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  const claimPath = `${lockPath}.reclaim`
  const activePath = join(root, 'successor-active')
  const releasedPath = join(root, 'successor-released')
  writeFileSync(lockPath, '')
  mkdirSync(claimPath)
  writeFileSync(join(claimPath, 'stale'), '')
  utimesSync(claimPath, new Date(0), new Date(0))
  let successorActive = false

  try {
    withSkillSourceRewriteLock(
      root,
      () => {
        assert.ok(existsSync(releasedPath), 'writer must wait for the successor sidecar to release')
        assert.ok(!existsSync(activePath), 'writer overlapped the successor sidecar')
      },
      {
        onBeforeReclaim: () => {
          if (!successorActive) return
          rmSync(claimPath, { recursive: true, force: true })
          rmSync(activePath, { force: true })
          writeFileSync(releasedPath, '')
        },
        onBeforeReclaimClaim: () => {
          rmSync(claimPath, { recursive: true, force: true })
          mkdirSync(claimPath)
          writeFileSync(
            join(claimPath, 'owner.json'),
            JSON.stringify({ createdAt: Date.now(), pid: process.pid }),
          )
          writeFileSync(activePath, '')
          successorActive = true
        },
      },
    )
  } finally {
    rmSync(root, { recursive: true, force: true })
  }
})

test('withSkillSourceRewriteLock waits for a successor to the stale sentinel', async () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-lock-'))
  execFileSync('git', ['init', '--quiet'], { cwd: root })
  const lockPath = join(
    root,
    execFileSync('git', ['rev-parse', '--git-path', 'boss-skill-snapshot.lock'], {
      cwd: root,
      encoding: 'utf8',
    }).trim(),
  )
  writeFileSync(lockPath, '')
  const successor = JSON.stringify({ createdAt: Date.now(), pid: 1, token: 'successor' })
  let releaseSuccessor
  let successorInstalled = false

  try {
    withSkillSourceRewriteLock(root, () => {}, {
      onBeforeReclaim: () => {
        if (successorInstalled) return
        successorInstalled = true
        rmSync(lockPath)
        writeFileSync(lockPath, successor)
        releaseSuccessor = spawn(
          process.execPath,
          [
            '-e',
            'setTimeout(() => require("node:fs").rmSync(process.argv[1], { force: true }), 100)',
            lockPath,
          ],
          { stdio: 'ignore' },
        )
      },
    })
    assert.ok(!existsSync(lockPath), 'successor must be released before this contender enters')
    const [code] = await once(releaseSuccessor, 'exit')
    assert.equal(code, 0)
  } finally {
    releaseSuccessor?.kill()
    rmSync(root, { recursive: true, force: true })
  }
})

test('VENDOR_MAP routes each helper to the right skills', () => {
  assert.deepEqual(VENDOR_MAP['boss-epic'].sort(), [
    // boss-binary.mjs backs callbacksAvailable's executable check; callback/adapter.mjs
    // imports it, so it ships wherever the adapter does.
    'boss-binary.mjs',
    'bossd-present.mjs',
    'bs-dispatch-await.mjs',
    'bs-epic-lib.mjs',
    'bs-run-sentinel.mjs',
    'callback/adapter.mjs',
    'callback/boss.mjs',
    'callback/epic-target.mjs',
    'dag-scheduler.mjs',
    'linear-claim.mjs',
    'linear-deps-lib.mjs',
    'linear-gate-lib.mjs',
    'main-module.mjs',
    'progress-comment.mjs',
    'session/adapter.mjs',
    'session/boss.mjs',
    'skill-config.mjs',
    'skill-extensions.mjs',
    'tracker/adapter-core.mjs',
    'tracker/adapter.mjs',
    'tracker/cli.mjs',
    'tracker/linear.mjs',
    'tracker/preflight.mjs',
  ])
  assert.ok(VENDOR_MAP['boss-plan'].includes('bs-run-sentinel.mjs'))
  assert.ok(VENDOR_MAP['boss-build'].includes('worktree-lock.sh'))
  for (const files of Object.values(VENDOR_MAP)) {
    assert.ok(files.includes('main-module.mjs'), 'every toolbox payload vendors main-module.mjs')
  }
})

test('every vendored payload is import-closed — no helper resolves outside its own toolbox', () => {
  // A vendored toolbox is a COPY, not a link: whatever a listed helper imports must
  // itself be listed, or the installed skill fails to resolve at runtime in a
  // consuming repo — a failure that never reproduces here, where the repo-root
  // skills-toolbox/ happens to sit one directory up. Deriving the requirement from the
  // real import graph is what makes this a gate rather than a hand-maintained list:
  // splitting a helper in two (tracker/adapter.mjs -> adapter-core.mjs) or adding any
  // new `import './x.mjs'` fails here until VENDOR_MAP catches up.
  const sourceRoot = join(REPO_ROOT, 'skills-toolbox')
  const gaps = []
  for (const [skill, files] of Object.entries(VENDOR_MAP)) {
    const declared = new Set(files)
    const seen = new Set()
    const walk = (file) => {
      if (seen.has(file) || !existsSync(file)) return
      seen.add(file)
      if (!file.endsWith('.mjs')) return // shell helpers have no ESM imports to follow
      const src = readFileSync(file, 'utf8')
      // Match the `from '<relative>'` clause on its own rather than anchoring to a
      // line-leading `import`/`export`: a braced specifier list spans lines, and a
      // newline-forbidding pattern would sail straight past the most common import
      // shape in this tree, reporting "closed" for a payload with real gaps.
      // Over-inclusion is safe — this gate must fail closed.
      const specs = [
        ...src.matchAll(/\bfrom\s*['"](\.[^'"]+)['"]/g), // static import/export ... from
        ...src.matchAll(/(?:^|\n)\s*import\s*['"](\.[^'"]+)['"]/g), // side-effect import './x'
        ...src.matchAll(/\bimport\(\s*['"](\.[^'"]+)['"]\s*\)/g), // dynamic import('./x')
      ]
      for (const [, spec] of specs) walk(join(dirname(file), spec))
    }
    for (const file of files) walk(join(sourceRoot, file))
    for (const file of seen) {
      const rel = relative(sourceRoot, file)
      if (!declared.has(rel)) gaps.push(`${skill}/toolbox is missing ${rel}`)
    }
  }
  assert.deepEqual(gaps, [], `add the missing helpers to VENDOR_MAP:\n${gaps.join('\n')}`)
})

test('each notes-consuming core vendors the extension helper', () => {
  for (const core of ['boss-build', 'boss-plan', 'boss-review', 'boss-epic', 'boss-repair']) {
    assert.ok(VENDOR_MAP[core].includes('skill-extensions.mjs'), core)
  }
})

test('the review-specific helpers route only to boss-review (BOS-196)', () => {
  // detect/cross-review-lib/codex-review/claude-review are review-only: bundling
  // them into any other skill's toolbox would leak review machinery it never runs.
  // Assert exclusive boss-review routing.
  const reviewOnly = [
    'bs-review-detect.mjs',
    'cross-review-lib.mjs',
    'codex-review.mjs',
    'claude-review.mjs',
  ]
  for (const helper of reviewOnly) {
    assert.ok(VENDOR_MAP['boss-review'].includes(helper), `boss-review must vendor ${helper}`)
    for (const [skill, files] of Object.entries(VENDOR_MAP)) {
      if (skill === 'boss-review') continue
      assert.ok(!files.includes(helper), `${helper} must not leak into ${skill}`)
    }
  }
  // skill-config.mjs (BOS-192) is NOT review-exclusive: boss-review vendors it as a
  // transitive dep of bs-review-detect.mjs, boss-build vendors it as a direct
  // dep of the Step 4 plan-contract check (validatePlanDescription, BOS-204), and every
  // core that runs a post-terminal notes phase vendors it for notesSampleRate (BOS-1099)
  // — boss-repair included, which reads the knob to take its per-run sampling roll. It
  // must still not leak into any skill that consumes none of those.
  const skillConfigConsumers = new Set([
    'boss-review',
    'boss-build',
    'boss-epic',
    'boss-plan',
    'boss-repair',
  ])
  for (const [skill, files] of Object.entries(VENDOR_MAP)) {
    if (skillConfigConsumers.has(skill)) {
      assert.ok(files.includes('skill-config.mjs'), `${skill} must vendor skill-config.mjs`)
    } else {
      assert.ok(!files.includes('skill-config.mjs'), `skill-config.mjs must not leak into ${skill}`)
    }
  }
  // All load-bearing review helpers are present in boss-review's toolbox.
  for (const helper of [
    'bs-review-caps.mjs',
    'bs-review-report.mjs',
    'skill-config.mjs',
    ...reviewOnly,
  ]) {
    assert.ok(VENDOR_MAP['boss-review'].includes(helper), `boss-review missing ${helper}`)
  }
})

test('shipped Codex review helpers fail closed without Tier A read confinement', async () => {
  const copies = [
    'services/boss/internal/skillinstall/skills/boss-review/toolbox/codex-review.mjs',
    'plugins/bossd-plugin-claude/skilldata/skills/boss-review/toolbox/codex-review.mjs',
  ]
  for (const copy of copies) {
    const { buildCodexArgs } = await import(pathToFileURL(join(REPO_ROOT, copy)).href)
    const args = buildCodexArgs({
      base: 'abc',
      head: 'def',
      repo: '/tmp/reviewed-checkout',
      falsificationReference: '/opt/skills/boss-review/references/falsification.md',
      tierAProbeRoot: '/Users/tester/.cache/boss-review/tier-a-private',
    })
    const sandboxIndex = args.indexOf('-s')
    const workspaceIndex = args.indexOf('-C')
    assert.equal(args[sandboxIndex + 1], 'read-only', `${copy} must disable Tier A`)
    assert.equal(args[workspaceIndex + 1], '/tmp/reviewed-checkout')
    assert.ok(!args.includes('--skip-git-repo-check'))
    assert.ok(!args.some((arg) => arg.startsWith('sandbox_workspace_write.')))
  }
})

// Every distinct filename any VENDOR_MAP skill needs — the engine reads each
// canonical source, so all must exist for a full run.
const ALL_SOURCES = [...new Set(Object.values(VENDOR_MAP).flat())]

test('vendorToolbox copies bytes and --check detects drift', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-'))
  const sourceRoot = join(root, 'skills-toolbox')
  const skillsRoot = join(root, 'skills')
  mkdirSync(sourceRoot, { recursive: true })
  for (const file of ALL_SOURCES) {
    mkdirSync(dirname(join(sourceRoot, file)), { recursive: true })
    writeFileSync(join(sourceRoot, file), `export const x = '${file}'\n`)
  }
  writeFileSync(join(sourceRoot, 'bs-run-sentinel.mjs'), 'export const x = 1\n')
  const write = vendorToolbox({ sourceRoot, skillsRoot, check: false })
  assert.equal(write.changed, true)
  const copied = readFileSync(
    join(skillsRoot, 'boss-plan', 'toolbox', 'bs-run-sentinel.mjs'),
    'utf8',
  )
  assert.equal(copied, 'export const x = 1\n')
  const clean = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(clean.changed, false)
  writeFileSync(join(skillsRoot, 'boss-plan', 'toolbox', 'bs-run-sentinel.mjs'), 'tampered\n')
  const drift = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(drift.changed, true)
  assert.ok(drift.differences.length > 0)
  rmSync(root, { recursive: true, force: true })
})

test('vendorToolbox --check skips when the skills root is stripped', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-'))
  const sourceRoot = join(root, 'skills-toolbox')
  const skillsRoot = join(root, 'skills') // deliberately never created (mirror strips .claude)
  mkdirSync(sourceRoot, { recursive: true })
  for (const file of ALL_SOURCES) {
    mkdirSync(dirname(join(sourceRoot, file)), { recursive: true })
    writeFileSync(join(sourceRoot, file), `export const x = '${file}'\n`)
  }
  const res = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(res.skipped, true)
  assert.equal(res.changed, false)
  assert.deepEqual(res.differences, [])
  rmSync(root, { recursive: true, force: true })
})

test('vendorToolbox preserves and --check detects executable-mode drift', () => {
  const root = mkdtempSync(join(tmpdir(), 'vt-'))
  const sourceRoot = join(root, 'skills-toolbox')
  const skillsRoot = join(root, 'skills')
  mkdirSync(sourceRoot, { recursive: true })
  for (const file of ALL_SOURCES) {
    mkdirSync(dirname(join(sourceRoot, file)), { recursive: true })
    writeFileSync(join(sourceRoot, file), `export const x = '${file}'\n`)
  }
  // worktree-lock.sh is invoked directly, so it ships +x; vendoring must preserve that bit.
  chmodSync(join(sourceRoot, 'worktree-lock.sh'), 0o755)
  vendorToolbox({ sourceRoot, skillsRoot, check: false })
  const dest = join(skillsRoot, 'boss-build', 'toolbox', 'worktree-lock.sh')
  assert.equal(statSync(dest).mode & 0o777, 0o755)
  const clean = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(clean.changed, false)

  // Content stays byte-identical, only the executable bit is lost.
  chmodSync(dest, 0o644)
  const drift = vendorToolbox({ sourceRoot, skillsRoot, check: true })
  assert.equal(drift.changed, true)
  assert.ok(drift.differences.some((d) => d.startsWith('mode mismatch')))
  rmSync(root, { recursive: true, force: true })
})
