import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { checkActionFile, checkDependabot, checkTree, checkWorkflow } from './check-action-pins.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const script = path.join(repoRoot, 'scripts/check-action-pins.mjs')
const SHA = 'd23441a48e516b6c34aea4fa41551a30e30af803'

// The public mirror strips the private workflows and dependabot.yml and may keep older public
// workflow copies, so the real-tree assertions only hold in the private source tree, which is
// the one that carries the mirror's exporter.
const isPrivateSourceTree = fs.existsSync(
  path.join(repoRoot, '.github/scripts/export-public-tree.sh'),
)

function workflow({ permissions = 'permissions:\n  contents: read\n', jobs }) {
  return `name: fixture\n\non:\n  push:\n\n${permissions}\njobs:\n${jobs}`
}

const readJob = (steps) => `  build:\n    runs-on: ubuntu-latest\n    steps:\n${steps}`

function messages(findings) {
  return findings.map((finding) => finding.message)
}

test('an action pinned by tag fails', () => {
  const findings = checkWorkflow('w.yml', workflow({ jobs: readJob('      - uses: foo/bar@v1\n') }))
  assert.equal(findings.length, 1)
  assert.equal(findings[0].line, 13)
  assert.match(findings[0].message, /foo\/bar@v1/)
})

test('an action pinned by a 40-hex commit SHA passes, with or without a tag comment', () => {
  const jobs = readJob(`      - uses: foo/bar@${SHA} # v1\n      - uses: foo/bar/sub/path@${SHA}\n`)
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs })), [])
})

test('short, uppercase or branch refs fail', () => {
  for (const ref of ['main', SHA.slice(0, 7), SHA.toUpperCase(), `${SHA}0`]) {
    const findings = checkWorkflow(
      'w.yml',
      workflow({ jobs: readJob(`      - uses: foo/bar@${ref}\n`) }),
    )
    assert.equal(findings.length, 1, ref)
  }
})

test('a ref without @ fails', () => {
  const findings = checkWorkflow('w.yml', workflow({ jobs: readJob('      - uses: foo/bar\n') }))
  assert.equal(findings.length, 1)
})

test('local ./.github/actions references are exempt', () => {
  const jobs = readJob(
    "      - uses: ./.github/actions/x\n      - name: quoted\n        uses: './.github/actions/y'\n",
  )
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs })), [])
})

test('docker:// actions need a sha256 digest', () => {
  const digest = 'a'.repeat(64)
  const unpinned = checkWorkflow(
    'w.yml',
    workflow({ jobs: readJob('      - uses: docker://alpine:3.20\n') }),
  )
  assert.equal(unpinned.length, 1)
  const pinned = checkWorkflow(
    'w.yml',
    workflow({ jobs: readJob(`      - uses: docker://alpine@sha256:${digest}\n`) }),
  )
  assert.deepEqual(pinned, [])
})

test('uses: text inside a run block scalar or a comment is not an action reference', () => {
  const jobs = readJob(
    [
      '      # uses: foo/bar@v1 in a comment',
      '      - run: |',
      '          echo "uses: foo/bar@v1"',
      '          uses: foo/baz@v2',
      '      - run: >-',
      '          uses: foo/qux@v3',
      `      - uses: foo/bar@${SHA} # uses: foo/bar@v1`,
      '',
    ].join('\n'),
  )
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs })), [])
})

test('a reusable-workflow job call must be pinned too', () => {
  const jobs = '  call:\n    uses: foo/bar/.github/workflows/x.yml@v1\n'
  assert.equal(checkWorkflow('w.yml', workflow({ jobs })).length, 1)
  const local = '  call:\n    uses: ./.github/workflows/x.yml\n'
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs: local })), [])
})

test('a workflow without a top-level permissions block fails', () => {
  const findings = checkWorkflow(
    'w.yml',
    workflow({
      permissions: '',
      jobs: `  build:\n    runs-on: x\n    permissions:\n      contents: read\n    steps:\n      - run: true\n`,
    }),
  )
  assert.equal(findings.length, 1)
  assert.match(findings[0].message, /top-level `permissions:`/)
})

test('a top-level write grant fails, in block or shorthand form', () => {
  const jobs = readJob('      - run: true\n')
  const block = checkWorkflow(
    'w.yml',
    workflow({ permissions: 'permissions:\n  contents: write\n  issues: read\n', jobs }),
  )
  assert.equal(block.length, 1)
  assert.match(block[0].message, /contents: write/)
  assert.equal(
    checkWorkflow('w.yml', workflow({ permissions: 'permissions: write-all\n', jobs })).length,
    1,
  )
})

test('read-only and empty top-level permissions pass', () => {
  const jobs = readJob('      - run: true\n')
  for (const permissions of [
    'permissions:\n  contents: read\n  actions: read # comment\n',
    'permissions: {}\n',
    'permissions: read-all\n',
  ]) {
    assert.deepEqual(checkWorkflow('w.yml', workflow({ permissions, jobs })), [], permissions)
  }
})

test('job-level write grants are allowed', () => {
  const jobs = `  push:\n    runs-on: x\n    permissions:\n      contents: write # pushes a tag\n    steps:\n      - run: git push\n`
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs })), [])
})

test('an allowlisted workflow may keep a top-level write grant', () => {
  const text = workflow({
    permissions: 'permissions:\n  contents: write\n',
    jobs: readJob('      - run: true\n'),
  })
  const allowlist = new Map([['w.yml', 'cannot be split']])
  assert.deepEqual(checkWorkflow('w.yml', text, { allowlist }), [])
})

test('a write-granted job that installs packages must not persist checkout credentials', () => {
  const install = (checkoutWith) =>
    `  release:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n      - uses: actions/checkout@${SHA} # v6\n${checkoutWith}      - run: pnpm install --frozen-lockfile\n`

  const persisted = checkWorkflow(
    'w.yml',
    workflow({ jobs: install('        with:\n          fetch-depth: 0\n') }),
  )
  assert.equal(persisted.length, 1)
  assert.match(persisted[0].message, /persist-credentials: false/)
  assert.equal(persisted[0].line, 15)

  const bare = checkWorkflow('w.yml', workflow({ jobs: install('') }))
  assert.equal(bare.length, 1)

  const safe = checkWorkflow(
    'w.yml',
    workflow({ jobs: install('        with:\n          persist-credentials: false\n') }),
  )
  assert.deepEqual(safe, [])
})

test('the credential rule covers pnpm add and npm ci, and inherits a top-level write grant', () => {
  const steps = (cmd) =>
    readJob(
      `      - name: checkout\n        uses: actions/checkout@${SHA}\n      - run: |\n          ${cmd}\n`,
    )
  for (const cmd of ['pnpm add -Dw semantic-release', 'npm ci', 'yarn install']) {
    const jobs = steps(cmd).replace(
      '    steps:',
      '    permissions:\n      contents: write\n    steps:',
    )
    assert.equal(checkWorkflow('w.yml', workflow({ jobs })).length, 1, cmd)
  }
  const inherited = checkWorkflow(
    'w.yml',
    workflow({ permissions: 'permissions: write-all\n', jobs: steps('pnpm install') }),
  )
  assert.deepEqual(messages(inherited).filter((m) => /persist-credentials/.test(m)).length, 1)
})

test('a read-only job that installs packages may persist checkout credentials', () => {
  const jobs = readJob(`      - uses: actions/checkout@${SHA}\n      - run: pnpm install\n`)
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs })), [])
})

test('composite actions must pin their nested uses', () => {
  const action = (ref) =>
    `name: x\nruns:\n  using: composite\n  steps:\n    - name: s\n      uses: actions/github-script@${ref}\n`
  assert.equal(checkActionFile('a/action.yml', action('v7')).length, 1)
  assert.deepEqual(checkActionFile('a/action.yml', action(SHA)), [])
})

test('dependabot must track the github-actions ecosystem', () => {
  const base = 'version: 2\nupdates:\n  - package-ecosystem: npm\n    directory: /\n'
  assert.equal(checkDependabot('.github/dependabot.yml', base).length, 1)
  assert.equal(checkDependabot('.github/dependabot.yml', null).length, 1)
  assert.deepEqual(
    checkDependabot(
      '.github/dependabot.yml',
      `${base}  - package-ecosystem: 'github-actions'\n    directory: /\n`,
    ),
    [],
  )
})

function writeTree(files) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'action-pins-'))
  for (const [relative, contents] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(dir, relative)), { recursive: true })
    fs.writeFileSync(path.join(dir, relative), contents)
  }
  return dir
}

const cleanTree = {
  '.github/workflows/ok.yml': workflow({ jobs: readJob(`      - uses: foo/bar@${SHA} # v1\n`) }),
  '.github/actions/a/action.yml': `runs:\n  using: composite\n  steps:\n    - uses: foo/bar@${SHA}\n`,
  '.github/dependabot.yml':
    'version: 2\nupdates:\n  - package-ecosystem: github-actions\n    directory: /\n',
}

test('checkTree scans workflows, composite actions and dependabot', () => {
  const clean = writeTree(cleanTree)
  try {
    assert.deepEqual(checkTree(clean).findings, [])
  } finally {
    fs.rmSync(clean, { recursive: true, force: true })
  }

  const dirty = writeTree({
    ...cleanTree,
    '.github/actions/a/action.yml': `runs:\n  using: composite\n  steps:\n    - uses: foo/bar@v1\n`,
    '.github/workflows/nope.yaml': `on: push\njobs:\n${readJob('      - run: true\n')}`,
  })
  try {
    const files = checkTree(dirty)
      .findings.map((finding) => finding.file)
      .sort()
    assert.deepEqual(files, ['.github/actions/a/action.yml', '.github/workflows/nope.yaml'])
  } finally {
    fs.rmSync(dirty, { recursive: true, force: true })
  }
})

test('the CLI exits 1 on findings, 0 on a clean tree, and rejects unknown flags', () => {
  const clean = writeTree(cleanTree)
  const dirty = writeTree({
    ...cleanTree,
    '.github/workflows/ok.yml': workflow({ jobs: readJob('      - uses: foo/bar@v1\n') }),
  })
  try {
    const ok = spawnSync(process.execPath, [script, '--root', clean], { encoding: 'utf8' })
    assert.equal(ok.status, 0, ok.stderr)
    const bad = spawnSync(process.execPath, [script, '--root', dirty], { encoding: 'utf8' })
    assert.equal(bad.status, 1)
    assert.match(bad.stderr, /\.github\/workflows\/ok\.yml:13/)
    const unknown = spawnSync(process.execPath, [script, '--nope'], { encoding: 'utf8' })
    assert.equal(unknown.status, 2)
  } finally {
    fs.rmSync(clean, { recursive: true, force: true })
    fs.rmSync(dirty, { recursive: true, force: true })
  }
})

test(
  'the real .github tree is pinned, read-only by default, and tracked by dependabot',
  {
    skip:
      !isPrivateSourceTree && 'public mirror: private workflows and dependabot.yml are stripped',
  },
  () => {
    const { findings, workflows, actions } = checkTree(repoRoot)
    assert.deepEqual(
      findings.map((finding) => `${finding.file}:${finding.line}: ${finding.message}`),
      [],
    )
    assert.ok(workflows > 0, 'no workflows scanned')
    assert.ok(actions > 0, 'no composite actions scanned')
  },
)

test('quoted inline write grants are still write grants', () => {
  for (const permissions of [
    "permissions: { contents: 'write' }\n",
    'permissions: { "contents": "write" }\n',
  ]) {
    const findings = checkWorkflow(
      'w.yml',
      workflow({ permissions, jobs: readJob('      - run: true\n') }),
    )
    assert.equal(findings.length, 1, permissions)
    assert.match(findings[0].message, /contents: write/)
  }
})

test('a flow-mapping step is pin-checked like a block step', () => {
  const bad = workflow({ jobs: readJob('      - { uses: actions/checkout@v6 }\n') })
  assert.equal(checkWorkflow('w.yml', bad).length, 1)
  const good = workflow({
    jobs: readJob(`      - { name: co, uses: 'actions/checkout@${SHA}' }\n`),
  })
  assert.deepEqual(checkWorkflow('w.yml', good), [])
})

test('checkTree flags an allowlist entry whose workflow no longer grants top-level write', () => {
  const writeTop = workflow({
    permissions: 'permissions:\n  contents: write\n',
    jobs: readJob('      - run: true\n'),
  })
  const root = writeTree({
    ...cleanTree,
    '.github/workflows/keep.yml': writeTop,
  })
  try {
    const live = new Map([
      ['.github/workflows/keep.yml', 'cannot be split'],
      ['.github/workflows/ok.yml', 'was split'],
    ])
    const files = checkTree(root, { allowlist: live }).findings.map((f) => f.file)
    assert.deepEqual(files, ['.github/workflows/ok.yml'])
  } finally {
    fs.rmSync(root, { recursive: true, force: true })
  }
})

test('a quoted value naming uses: cannot mask the real ref of a flow step', () => {
  const masked = workflow({
    jobs: readJob(`      - { name: 'uses: foo/bar@${SHA}', uses: actions/checkout@v6 }\n`),
  })
  assert.equal(checkWorkflow('w.yml', masked).length, 1)
})

test('the credential rule covers flow-style checkout steps', () => {
  const steps = (checkout) => `      - ${checkout}\n      - run: pnpm install\n`
  const job = (checkout) =>
    `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n${steps(checkout)}`
  const leaky = checkWorkflow('w.yml', workflow({ jobs: job(`{ uses: actions/checkout@${SHA} }`) }))
  assert.equal(leaky.length, 1)
  assert.match(leaky[0].message, /persist-credentials: false/)
  const safe = checkWorkflow(
    'w.yml',
    workflow({
      jobs: job(`{ uses: actions/checkout@${SHA}, with: { persist-credentials: false } }`),
    }),
  )
  assert.deepEqual(safe, [])
})

test('a step name mentioning persist-credentials cannot stand in for the with: setting', () => {
  const job = (checkout) =>
    `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n      - ${checkout}\n      - run: pnpm install\n`
  const decoys = [
    `{ name: 'persist-credentials: false', uses: actions/checkout@${SHA} }`,
    `{ name: "x", uses: actions/checkout@${SHA}, env: { persist-credentials: false } }`,
    `{ uses: actions/checkout@${SHA}, with: { persist-credentials: true } }`,
  ]
  for (const decoy of decoys) {
    assert.equal(checkWorkflow('w.yml', workflow({ jobs: job(decoy) })).length, 1, decoy)
  }
  const quoted = `{ uses: actions/checkout@${SHA}, "with": { "persist-credentials": "false" } }`
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs: job(quoted) })), [])
})

test('escaped quotes inside a double-quoted value cannot hide a flow step ref', () => {
  for (const name of ['"a\\" #"', '"x\\", y"', '"back\\\\slash"']) {
    const step = `      - { name: ${name}, uses: foo/bar@v1 }\n`
    assert.equal(checkWorkflow('w.yml', workflow({ jobs: readJob(step) })).length, 1, name)
  }
})

test("a doubled '' inside a single-quoted value is an escaped quote, not a closing one", () => {
  for (const name of ["'it''s, b'", "'a'' #'", "'c:\\'"]) {
    const step = `      - { name: ${name}, uses: foo/bar@v1 }\n`
    assert.equal(checkWorkflow('w.yml', workflow({ jobs: readJob(step) })).length, 1, name)
  }
})

test('an apostrophe inside a plain scalar does not open a quote', () => {
  const steps = [
    `      - { name: it's, uses: foo/bar@v1 }\n`,
    `      - name: it's pinned\n        uses: foo/bar@v1\n`,
  ]
  for (const step of steps) {
    assert.equal(checkWorkflow('w.yml', workflow({ jobs: readJob(step) })).length, 1, step)
  }
  const pinned = `      - { name: it's, uses: foo/bar@${SHA} } # it's pinned\n`
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs: readJob(pinned) })), [])
})

test('JSON-style flow pairs keep a quoted value that follows the colon directly', () => {
  const step = `      - {"name":"a, b","uses":"foo/bar@v1"}\n`
  assert.equal(checkWorkflow('w.yml', workflow({ jobs: readJob(step) })).length, 1)
})

test('quoted, escaped and spaced block keys are read like plain ones', () => {
  for (const key of ['"uses"', "'uses'", 'uses ', '"u\\x73es"', '"\\u0075ses"']) {
    const step = `      - ${key}: actions/checkout@v6\n`
    const findings = checkWorkflow('w.yml', workflow({ jobs: readJob(step) }))
    assert.equal(findings.length, 1, key)
    assert.match(findings[0].message, /not pinned/, key)
    const action = `runs:\n  using: composite\n  steps:\n    - ${key}: foo/bar@v1\n`
    assert.equal(checkActionFile('a.yml', action).length, 1, key)
  }
  const pinned = `      - "uses": 'actions/checkout@${SHA}'\n`
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs: readJob(pinned) })), [])
})

test('a quoted top-level permission scope or permissions key is still a write grant', () => {
  for (const permissions of [
    'permissions:\n  "contents": write\n',
    "permissions:\n  'contents' : 'write'\n",
    '"permissions":\n  contents: write\n',
    "'permissions': write-all\n",
  ]) {
    const findings = checkWorkflow(
      'w.yml',
      workflow({ permissions, jobs: readJob('      - run: true\n') }),
    )
    assert.equal(findings.length, 1, permissions)
    assert.match(findings[0].message, /move write grants/, permissions)
  }
})

test('a quoted jobs key still gets the credential rule', () => {
  const text = workflow({
    jobs: `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n      - uses: actions/checkout@${SHA}\n      - run: pnpm install\n`,
  }).replace('\njobs:\n', '\n"jobs":\n')
  const findings = checkWorkflow('w.yml', text)
  assert.equal(findings.length, 1)
  assert.match(findings[0].message, /persist-credentials: false/)
})

test('a block scalar opened on a step dash line does not hide the step sibling uses:', () => {
  for (const opener of ['name: >-', 'if: |', 'name: |+2']) {
    const step = `      - ${opener}\n          some text\n          uses: not/a-ref@v1\n        uses: actions/checkout@v6\n`
    const findings = checkWorkflow('w.yml', workflow({ jobs: readJob(step) }))
    assert.deepEqual(
      findings.map((finding) => finding.line),
      [13 + 3],
      opener,
    )
    const action = `runs:\n  using: composite\n  steps:\n    - ${opener}\n        text\n      uses: foo/bar@v1\n`
    assert.equal(checkActionFile('a.yml', action).length, 1, opener)
  }
})

test('only the block checkout step own with: mapping can drop credentials', () => {
  const job = (checkout) =>
    `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n${checkout}      - run: pnpm install\n`
  const findingsFor = (checkout) => checkWorkflow('w.yml', workflow({ jobs: job(checkout) }))
  const co = `actions/checkout@${SHA}`
  const decoys = [
    `      - uses: ${co}\n        env:\n          persist-credentials: 'false'\n        with:\n          persist-credentials: true\n`,
    `      - uses: ${co}\n        env: { persist-credentials: false }\n`,
    `      - uses: ${co}\n        with:\n          nested:\n            persist-credentials: false\n`,
    `      - uses: ${co}\n      - name: other\n        with:\n          persist-credentials: false\n`,
    `      - uses: ${co}\n        env:\n          with:\n            persist-credentials: false\n`,
    `      -\n        uses: ${co}\n        env:\n          persist-credentials: false\n`,
  ]
  for (const decoy of decoys) assert.equal(findingsFor(decoy).length, 1, decoy)
  const safe = [
    `      - uses: ${co}\n        with:\n          fetch-depth: 0\n          persist-credentials: false\n`,
    `      - with:\n          persist-credentials: false\n        uses: ${co}\n`,
    `      - uses: ${co}\n        with: { fetch-depth: 0, persist-credentials: false }\n`,
    `      - uses: ${co}\n        "with":\n          "persist-credentials": "false"\n`,
    `      -\n        uses: ${co}\n        with:\n          persist-credentials: false\n`,
    `      -\n        with:\n          persist-credentials: false\n        uses: ${co}\n`,
  ]
  for (const step of safe) assert.deepEqual(findingsFor(step), [], step)
})

test('the credential rule fires on any package-manager invocation in a write-granted job', () => {
  const job = (command) =>
    `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n      - uses: actions/checkout@${SHA}\n      - run: ${command}\n`
  const flagged = (command) => checkWorkflow('w.yml', workflow({ jobs: job(command) })).length
  // Shapes earlier shell-parsing versions of this rule missed, plus invocations that run
  // node_modules code without installing anything.
  for (const command of [
    'pnpm --dir services/web install',
    'npm --prefix "${{ github.workspace }}/web" install',
    'npm --fetch-retries 3 ci',
    'npm --silent>build.log ci',
    'pnpm &>log install',
    'echo >"$(npm ci; printf t)"',
    'pnpm --dir $(printf "web\\"dir") install',
    'pnpm --dir `pwd`/web install',
    '(cd web && npm ci)',
    "${{ format('npm ci') }}",
    '\\npm ci',
    "'np\\m ci'",
    "np''m ci",
    'n"p"m ci',
    '"cd web\\nnpm ci"',
    '"cd web\\tnpm ci"',
    '"cd web\\x0anpm ci"',
    '"echo ok\\x20&&\\x20npm ci"',
    '"cd web\\u000anpm ci"',
    '"cd web\\U0000000anpm ci"',
    '"cd web && \\x6epm ci"',
    '"n\\x70m ci"',
    'ruby -e \'exec"npm", "ci"\'',
    '"n\\x27\\x27pm ci"',
    '"n\\x22\\x22pm ci"',
    '"np\\x5cm ci"',
    '"n\\u005cpm ci"',
    '"sh -c \\"\\x6epm ci\\""',
    '"echo ok\\nn\\0pm ci"',
    '"echo ok\\nn\\x00pm ci"',
    '|\n          cd web\n          npm ci',
    '>-\n          cd web &&\n          pnpm install',
    './node_modules/.bin/pnpm install',
    'sh -c "npm ci"',
    'npx some-tool',
    'pnpx some-tool',
    'yarnpkg',
    'corepack enable',
    'pnpm exec semantic-release',
    'pnpm --filter install run build',
    'npm test',
    '|\n          pnpm \\\n            install',
    '>-\n          pnpm --version\n            install -d build',
    '&build |\n          pnpm \\\n            install',
    '!!str |\n          npm ci',
  ]) {
    assert.equal(flagged(command), 1, command)
  }
  for (const command of [
    'make build',
    'echo pnpm-lock.yaml changed',
    'cat .npmrc',
    'echo "$npm_config_cache"',
    '|\n          # pnpm install is not needed here\n          make',
    'go test ./... # npm ci',
    '"cd web\\x41npm ci"',
  ]) {
    assert.equal(flagged(command), 0, command)
  }
})

test('an action named for a package manager counts as running it', () => {
  const job = (step) =>
    `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n      - uses: actions/checkout@${SHA}\n${step}      - run: make\n`
  for (const step of [
    `      - uses: pnpm/action-setup@${SHA}\n        with:\n          run_install: true\n`,
    `      - { uses: 'bahmutov/npm-install@${SHA}' }\n`,
    `      - uses: borales/actions-yarn@${SHA}\n        with:\n          cmd: install\n`,
    `      - uses: owner/setup-pnpm@${SHA}\n`,
    `      - uses: owner/npx-runner@${SHA}\n`,
    `      - uses: owner/corepack-setup@${SHA}\n`,
    `      - uses: owner/pnpx-runner@${SHA}\n`,
    `      - uses: owner/yarnpkg-action@${SHA}\n`,
  ]) {
    const findings = checkWorkflow('w.yml', workflow({ jobs: job(step) }))
    assert.equal(findings.length, 1, step)
    assert.match(findings[0].message, /persist-credentials: false/, step)
  }
  const unrelated = `      - uses: actions/setup-node@${SHA}\n      - uses: owner/npmish-tool@${SHA}\n      - uses: owner/xnpm-tool@${SHA}\n`
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs: job(unrelated) })), [])
})

test('a bare dash nested inside an earlier field does not open the checkout step', () => {
  const step = `      - name: co\n        args:\n          -\n            x\n        uses: actions/checkout@${SHA}\n        with:\n          persist-credentials: false\n      - run: pnpm install\n`
  const job = `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n${step}`
  assert.deepEqual(checkWorkflow('w.yml', workflow({ jobs: job })), [])
})

test('a CRLF workflow is classified like an LF one', () => {
  const job = `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n      - uses: actions/checkout@${SHA}\n      - run: |\n          pnpm \\\n            install\n`
  const findings = checkWorkflow('w.yml', workflow({ jobs: job }).replaceAll('\n', '\r\n'))
  assert.equal(findings.length, 1)
  assert.match(findings[0].message, /persist-credentials: false/)
})

test('a multi-line flow-mapping step naming a manager on a later line counts', () => {
  const job = `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n      - uses: actions/checkout@${SHA}\n      - { name: build,\n          run: npm ci }\n`
  assert.equal(checkWorkflow('w.yml', workflow({ jobs: job })).length, 1)
})

test('an out-of-range YAML escape does not crash the gate', () => {
  const job = `  push:\n    runs-on: x\n    permissions:\n      contents: write\n    steps:\n      - uses: actions/checkout@${SHA}\n      - run: "\\U00110000npm ci"\n`
  assert.doesNotThrow(() => checkWorkflow('w.yml', workflow({ jobs: job })))
})
