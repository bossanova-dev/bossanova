import { test } from 'node:test'
import assert from 'node:assert/strict'
import { AREAS, detectAreas, dependencyBumps } from './verify-areas.mjs'

const conventions = {
  migrations:
    'migrations/001.ts db/migrate/001.rb queries/find.sql schema.prisma db/migrations/0001.test.ts',
  'api-contract':
    'api.proto schema.graphql schema.graphqls schema.gql docs/openapi.yaml openapi-v1.yml openapi.json swagger.yaml swagger-v2.yml swagger.json',
  infra:
    'main.tf values.tfvars terragrunt.hcl k8s/deploy.yaml kubernetes/deploy.yaml helm/values.yaml charts/Chart.yaml kustomization.yaml kustomization.yml .github/workflows/ci.yml .github/actions/run/action.yml .gitlab-ci.yml .circleci/config.yml',
  'agent-rules':
    'AGENTS.md CLAUDE.md GEMINI.md skills/x/SKILL.md .claude/settings.json .codex/config.toml .cursor/rules/test.mdc .cursorrules .github/copilot-instructions.md .boss-skills.json nested/.boss-project.json .mcp.json .claude/skills/x/SKILL.md',
  dependencies:
    'package.json go.mod Cargo.toml pyproject.toml requirements-dev.txt Gemfile composer.json pom.xml build.gradle build.gradle.kts package-lock.json pnpm-lock.yaml yarn.lock bun.lockb go.sum go.work.sum Cargo.lock poetry.lock uv.lock Gemfile.lock composer.lock',
  housekeeping:
    'x.test.ts x.spec.ts x_test.go test_x.py x_test.py __tests__/x.ts __snapshots__/x.ts testdata/x.go test/x.ts tests/x.ts README.md guide.mdx guide.rst docs/image.png LICENSE-MIT CHANGELOG-v1 gen/x.go generated/x.ts __generated__/x.ts x.generated.ts x.gen.ts x_gen.go zz_generated.go x.pb.go x_pb2.py x_pb2_grpc.py x.snap src/components/Button.test.tsx',
  ui: 'A.tsx A.jsx A.vue A.svelte A.astro x.css x.scss x.sass x.less index.html components/x.ts pages/x.ts layouts/x.ts styles/x.ts',
  other: 'src/main.go services/api/internal/migration/seeds/hospitals.go',
}

for (const [area, paths] of Object.entries(conventions)) {
  for (const path of paths.split(' ')) {
    test(`convention ${path} → ${area}`, () => {
      assert.equal(detectAreas([path])[0].area, area)
      // Every **/ family must work both at the root and below a package.
      if (!path.startsWith('.') && !path.startsWith('docs/')) {
        assert.equal(detectAreas([`packages/app/${path}`])[0].area, area)
      }
    })
  }
}

const freshClaimFiles = [
  'lib/fresh-claim-graphql/core/gql.ts',
  'lib/fresh-claim-graphql/core/graphql.ts',
  'lib/fresh-claim-queries/Hospital.ts',
  'services/admin/src/components/domain/hospitals/EditHospitalContactModal.test.tsx',
  'services/admin/src/components/domain/hospitals/EditHospitalContactModal.tsx',
  'services/admin/src/components/domain/hospitals/HospitalTable.test.tsx',
  'services/admin/src/components/domain/hospitals/HospitalTable.tsx',
  'services/admin/src/components/domain/hospitals/index.ts',
  'services/admin/src/layouts/Application.tsx',
  'services/admin/src/pages/[organizationId]/settings/hospitals.tsx',
  'services/admin/src/services/Routes.ts',
  'services/api/api/graph/generated/generated.go',
  'services/api/api/graph/policy/hospital_policy.go',
  'services/api/api/graph/policy/hospital_policy_test.go',
  'services/api/api/graph/resolver/hospital.resolvers.go',
  'services/api/api/graph/resolver/hospital_resolvers_test.go',
  'services/api/api/graph/resolver/schema.resolvers.go',
  'services/api/api/graph/schema/hospital.graphqls',
  'services/api/api/graph/schema/schema.graphqls',
  'services/api/entity/hospital.go',
  'services/api/internal/migration/seeds/hospitals.go',
  'services/api/internal/migration/seeds/hospitals_db_test.go',
  'services/api/internal/service/claim_patient_email.go',
  'services/api/internal/service/email_address_normalize.go',
  'services/api/internal/service/hospital_update_claim_number_request_email.go',
  'services/api/internal/service/hospital_update_claim_number_request_email_test.go',
  'services/api/internal/storage/hospital.go',
]

const bossanovaFiles = [
  'docs/features/README.md',
  'docs/features/accounts.md',
  'docs/features/coverage.md',
  'lib/bossalib/gen/bossanova/v1/bossanovav1connect/orchestrator.connect.go',
  'lib/bossalib/gen/bossanova/v1/models.pb.go',
  'lib/bossalib/gen/bossanova/v1/orchestrator.pb.go',
  'lib/bossalib/gen/bossanova/v1/stream.pb.go',
  'proto/bossanova/v1/models.proto',
  'proto/bossanova/v1/orchestrator.proto',
  'proto/bossanova/v1/stream.proto',
  'services/bossd/api/daemon/account.go',
  'services/bossd/api/daemon/account_ports.go',
  'services/bossd/api/daemon/account_sync_test.go',
  'services/bossd/api/daemon/convert.go',
  'services/bossd/api/daemon/credential_devices.go',
  'services/bossd/api/daemon/credential_devices_test.go',
  'services/bossd/api/daemon/server.go',
  'services/bossd/api/upstream/BUILD.bazel',
  'services/bossd/api/upstream/account_commands.go',
  'services/bossd/api/upstream/account_sync.go',
  'services/bossd/api/upstream/adapters.go',
  'services/bossd/api/upstream/command_dispatcher.go',
  'services/bossd/api/upstream/credential_commands.go',
  'services/bossd/api/upstream/credential_seal_cache.go',
  'services/bossd/api/upstream/credential_seal_cache_test.go',
  'services/bossd/api/upstream/credential_sync.go',
  'services/bossd/api/upstream/credential_trust.go',
  'services/bossd/api/upstream/credential_trust_test.go',
  'services/bossd/api/upstream/trigger_gate.go',
  'services/bossd/cmd/BUILD.bazel',
  'services/bossd/cmd/credential_devices.go',
  'services/bossd/cmd/credential_refresh_test.go',
  'services/bossd/cmd/credential_sync.go',
  'services/bossd/cmd/wire_accounts.go',
  'services/bossd/cmd/wire_background_workers.go',
  'services/bossd/cmd/wire_daemon.go',
  'services/bossd/cmd/wire_upstream.go',
  'services/bossd/cmd/wiring.go',
  'services/bossd/internal/accountwiring/BUILD.bazel',
  'services/bossd/internal/accountwiring/adapters.go',
  'services/bossd/internal/accountwiring/refresh_broker.go',
  'services/bossd/internal/accountwiring/refresh_broker_test.go',
  'services/bossd/internal/accountwiring/smoke.go',
  'services/bossd/internal/adapter/accountcred/accountcred.go',
  'services/bossd/internal/adapter/accountcred/accountcred_test.go',
  'services/bossd/internal/adapter/credmaterialize/BUILD.bazel',
  'services/bossd/internal/adapter/credmaterialize/credmaterialize.go',
  'services/bossd/internal/adapter/credmaterialize/persist_back.go',
  'services/bossd/internal/adapter/credmaterialize/refresh_broker.go',
  'services/bossd/internal/adapter/credmaterialize/refresh_broker_test.go',
  'services/bossd/internal/credsync/BUILD.bazel',
  'services/bossd/internal/credsync/contracts.go',
  'services/bossd/internal/credsync/crypto.go',
  'services/bossd/internal/credsync/crypto_test.go',
  'services/bossd/internal/credsync/devices.go',
  'services/bossd/internal/credsync/engine.go',
  'services/bossd/internal/credsync/engine_test.go',
  'services/bossd/internal/credsync/key_seal.go',
  'services/bossd/internal/credsync/key_seal_test.go',
  'services/bossd/internal/credsync/recipient.go',
  'services/bossd/internal/credsync/rotation.go',
  'services/bossd/internal/db/BUILD.bazel',
  'services/bossd/internal/db/account_store.go',
  'services/bossd/internal/db/account_sync.go',
  'services/bossd/internal/db/credential_version_test.go',
  'services/bossd/internal/db/queries/agent_accounts.sql',
  'services/bossd/internal/db/sqlcgen/agent_accounts.sql.go',
  'services/bossd/internal/db/sqlcgen/models.go',
  'services/bossd/internal/domain/agent_account.go',
  'services/bossd/migrations/20261029120000_agent_accounts_credential_version.sql',
  'services/bossd/migrations/BUILD.bazel',
  'services/bosso/api/orchestrator/agentaccounts/BUILD.bazel',
  'services/bosso/api/orchestrator/agentaccounts/credentials.go',
  'services/bosso/api/orchestrator/agentaccounts/credentials_test.go',
  'services/bosso/api/orchestrator/agentaccounts/devices.go',
  'services/bosso/api/orchestrator/agentaccounts/devices_test.go',
  'services/bosso/api/orchestrator/agentaccounts/service.go',
  'services/bosso/api/orchestrator/agentaccounts/sync.go',
  'services/bosso/api/orchestrator/authz_interceptor_test.go',
  'services/bosso/api/orchestrator/credential_devices_cross_tenant_test.go',
  'services/bosso/api/orchestrator/cross_tenant_denial_test.go',
  'services/bosso/api/orchestrator/fixtures_test.go',
  'services/bosso/api/orchestrator/implementation_catalog.go',
  'services/bosso/api/orchestrator/routing_catalog.go',
  'services/bosso/api/orchestrator/telemetry_actions.go',
  'services/bosso/api/orchestrator/telemetry_actions_test.go',
  'services/bosso/api/stream/agent_accounts.go',
  'services/bosso/cmd/main.go',
  'services/bosso/internal/clawreader/reader.sql',
  'services/bosso/internal/clawreader/verify.sql',
  'services/bosso/internal/daemonproxy/machine_owner_test.go',
  'services/bosso/internal/db/BUILD.bazel',
  'services/bosso/internal/db/agent_credential_store.go',
  'services/bosso/internal/db/agent_credential_store_test.go',
  'services/bosso/internal/db/queries/agent_credentials.sql',
  'services/bosso/internal/db/queries/synced_agent_accounts.sql',
  'services/bosso/internal/db/sqlcgen/BUILD.bazel',
  'services/bosso/internal/db/sqlcgen/agent_credentials.sql.go',
  'services/bosso/internal/db/sqlcgen/models.go',
  'services/bosso/internal/db/sqlcgen/synced_agent_accounts.sql.go',
  'services/bosso/internal/db/synced_agent_account_store.go',
  'services/bosso/internal/domain/BUILD.bazel',
  'services/bosso/internal/domain/agent_credential.go',
  'services/bosso/internal/domain/agentaccountsync/BUILD.bazel',
  'services/bosso/internal/domain/agentaccountsync/credential_test.go',
  'services/bosso/internal/domain/agentaccountsync/reconcile.go',
  'services/bosso/internal/domain/agentaccountsync/sync.go',
  'services/bosso/internal/domain/synced_agent_account.go',
  'services/bosso/migrations_postgres/20261029000100_add_agent_credential_distribution.sql',
  'services/bosso/migrations_postgres/20261029000200_add_agent_credential_refresh_claim.sql',
  'services/bosso/migrations_postgres/BUILD.bazel',
  'services/bosso/migrations_postgres/withheld_columns.json',
  'services/bosso/migrations_postgres/withheld_columns_test.go',
  'services/docs/openapi/bossanova.v1.openapi.yaml',
  'services/web/e2e/fakes/api.ts',
  'services/web/src/gen/bossanova/v1/models_pb.ts',
  'services/web/src/gen/bossanova/v1/orchestrator_pb.ts',
  'services/web/src/gen/bossanova/v1/stream_pb.ts',
]

test('fresh-claim #11996 includes contracts, UI and housekeeping; singular migration is other', () => {
  const result = detectAreas(freshClaimFiles)
  assert.deepEqual(
    result.filter((x) => x.area !== 'other').map((x) => x.area),
    ['api-contract', 'housekeeping', 'ui'],
  )
  assert.ok(
    result
      .find((x) => x.area === 'other')
      .paths.includes('services/api/internal/migration/seeds/hospitals.go'),
  )
})

test('bossanova #3752 full 118-file fixture includes migrations, contracts and housekeeping', () => {
  assert.equal(bossanovaFiles.length, 118)
  assert.deepEqual(
    detectAreas(bossanovaFiles)
      .filter((x) => x.area !== 'other')
      .map((x) => x.area),
    ['migrations', 'api-contract', 'housekeeping'],
  )
})

test('catalogue is frozen, ordered and carries the public vocabulary', () => {
  assert.ok(Object.isFrozen(AREAS))
  assert.deepEqual(
    AREAS.map((x) => x.area),
    [
      'migrations',
      'api-contract',
      'infra',
      'agent-rules',
      'dependencies',
      'housekeeping',
      'ui',
      'other',
    ],
  )
  for (const entry of AREAS) {
    assert.ok(Object.isFrozen(entry))
    assert.ok(Object.isFrozen(entry.globs))
    assert.ok(['always', 'unless-proven', 'never'].includes(entry.defaultAsk))
    assert.equal(typeof entry.label, 'string')
  }
})

test('partition is sorted, deduplicated, deterministic and accepts immutable inputs', () => {
  const files = Object.freeze([
    'z.go',
    'a.go',
    'z.go',
    'docs/a.md',
    Object.freeze({
      path: 'archive/x.ts',
      previousPath: 'migrations/x.ts',
      status: 'renamed',
      changes: 4,
    }),
    Object.freeze({
      path: 'src/A.tsx',
      previousPath: 'archive/x.ts',
      status: 'renamed',
      changes: 4,
    }),
    'src/A.tsx',
  ])
  const result = detectAreas(files)
  assert.deepEqual(result, detectAreas([...files].reverse()))
  const paths = result.flatMap((x) => x.paths)
  assert.equal(paths.length, new Set(paths).size)
  assert.deepEqual(
    [...paths].sort(),
    ['z.go', 'a.go', 'docs/a.md', 'archive/x.ts', 'migrations/x.ts', 'src/A.tsx'].sort(),
  )
  for (const entry of result) assert.deepEqual(entry.paths, [...entry.paths].sort())
  assert.deepEqual(
    result.map((x) => x.area),
    ['migrations', 'housekeeping', 'other'],
  )
  assert.deepEqual(detectAreas([]), [])
})

for (const [path, previousPath, changes, area] of [
  ['archive/a.sql', 'migrations/a.sql', 2, 'migrations'],
  ['src/ui/A.tsx', 'src/components/A.tsx', 0, 'housekeeping'],
  ['src/ui/A.tsx', 'src/components/A.tsx', 4, 'ui'],
  ['src/new.go', 'src/old.go', 0, 'housekeeping'],
  ['archive/ci.yml', '.github/workflows/ci.yml', 0, 'infra'],
  ['archive/readme.md', 'schema.proto', 4, 'api-contract'],
]) {
  test(`rename ${previousPath} → ${path}, changes ${changes}`, () => {
    const result = detectAreas([{ path, previousPath, status: 'renamed', changes }])
    assert.equal(result[0].area, area)
    assert.deepEqual(result[0].paths, [path, previousPath].sort())
  })
}

const patch = (before, after) => `@@ -1 +1 @@\n-${before}\n+${after}`
for (const [path, before, after, name, from, to] of [
  ['package.json', '"react": "^18.3.1",', '"react": "^19.0.0",', 'react', '^18.3.1', '^19.0.0'],
  [
    'go.mod',
    'example.com/lib v1.9.0',
    'example.com/lib/v2 v2.0.0',
    'example.com/lib',
    'v1.9.0',
    'v2.0.0',
  ],
  ['requirements.txt', 'requests>=1.9.0', 'requests>=2.0.0', 'requests', '>=1.9.0', '>=2.0.0'],
  ['pyproject.toml', 'httpx = "^1.9"', 'httpx = "^2.0"', 'httpx', '^1.9', '^2.0'],
  ['pyproject.toml', '"httpx>=1.9",', '"httpx>=2.0",', 'httpx', '>=1.9', '>=2.0'],
  ['Cargo.toml', 'serde = "1.9"', 'serde = "2.0"', 'serde', '1.9', '2.0'],
  [
    'Cargo.toml',
    'serde = { version = "1.9", features = [] }',
    'serde = { version = "2.0", features = [] }',
    'serde',
    '1.9',
    '2.0',
  ],
]) {
  test(`${path} pairs ${name} major bump`, () => {
    const inputPatch = patch(before, after)
    const bump = { name, from, to }
    assert.deepEqual(dependencyBumps(path, inputPatch), { readable: true, bumps: [bump] })
    assert.deepEqual(detectAreas([{ path, patch: inputPatch }]), [
      {
        area: 'dependencies',
        label: 'Dependencies',
        paths: [path],
        defaultAsk: 'always',
        bumps: [{ path, ...bump }],
      },
    ])
  })
}

test('dependencies use numeric major and zero-major minor changes only', () => {
  for (const [from, to, expected] of [
    ['^18.2.0', '^18.3.1', false],
    ['0.4.1', '0.5.0', true],
    ['0.4.1', '0.4.9', false],
    ['2.0', '1.0', false],
    ['workspace:*', 'latest', false],
    ['git+https://example.com/a', '2.0', false],
    ['~1.0', ' >=v2.0', true],
  ]) {
    const result = dependencyBumps('package.json', patch(`"x": "${from}"`, `"x": "${to}"`))
    assert.equal(result.bumps.length, expected ? 1 : 0)
    assert.equal(
      detectAreas([{ path: 'package.json', patch: patch(`"x": "${from}"`, `"x": "${to}"`) }])[0]
        .defaultAsk,
      expected ? 'always' : 'never',
    )
  }
})

test('ignores package identity, diff headers, context and one-sided dependencies', () => {
  assert.deepEqual(
    dependencyBumps(
      'package.json',
      `${patch('"version": "1.0"', '"version": "2.0"')}\n${patch('"name": "1.0"', '"name": "2.0"')}\n+"new": "2.0"\n-"removed": "1.0"\n "context": "2.0"\n--- a/package.json\n+++ b/package.json`,
    ),
    { readable: true, bumps: [] },
  )
})

test('lockfiles never ask, absent or unsupported manifests always ask', () => {
  assert.deepEqual(dependencyBumps('pnpm-lock.yaml', undefined), { readable: true, bumps: [] })
  assert.equal(detectAreas(['pnpm-lock.yaml'])[0].defaultAsk, 'never')
  for (const path of [
    'package.json',
    'go.mod',
    'Cargo.toml',
    'pyproject.toml',
    'requirements.txt',
    'Gemfile',
    'composer.json',
    'pom.xml',
    'build.gradle',
    'build.gradle.kts',
  ]) {
    assert.deepEqual(dependencyBumps(path, undefined), { readable: false, bumps: [] })
    assert.equal(detectAreas([path])[0].defaultAsk, 'always')
  }
  for (const path of ['Gemfile', 'composer.json', 'pom.xml', 'build.gradle', 'build.gradle.kts']) {
    assert.deepEqual(dependencyBumps(path, '+anything'), { readable: false, bumps: [] })
    assert.equal(detectAreas([{ path, patch: '+anything' }])[0].defaultAsk, 'always')
  }
})

test('collects sorted unique bumps, independent of file order', () => {
  const files = [
    { path: 'z/package.json', patch: patch('"z": "1"', '"z": "2"') },
    {
      path: 'a/package.json',
      patch: patch('"b": "1"', '"b": "2"') + '\n' + patch('"a": "1"', '"a": "2"'),
    },
  ]
  const result = detectAreas(files)
  assert.deepEqual(result, detectAreas([...files].reverse()))
  assert.deepEqual(result, detectAreas([...files, ...files]))
  assert.deepEqual(
    result[0].bumps.map((x) => [x.path, x.name]),
    [
      ['a/package.json', 'a'],
      ['a/package.json', 'b'],
      ['z/package.json', 'z'],
    ],
  )
})
