// BOS-1317: pin the shape of the dbt warehouse's Terraform. `terraform validate`
// proves the configuration parses; it cannot prove the warehouse login sits on the
// EXISTING shared instance (Bossanova must never create or alter that instance), nor
// that the Kestra secret output stays sensitive and base64-encoded, which is the
// strict form madverts-core's Kestra secret validator accepts.

import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'

const moduleURL = new URL('../infra/modules/gcp-sql/main.tf', import.meta.url)
const moduleOutputsURL = new URL('../infra/modules/gcp-sql/outputs.tf', import.meta.url)
const rootOutputsURL = new URL('../infra/environments/outputs.tf', import.meta.url)
const rootMainURL = new URL('../infra/environments/main.tf', import.meta.url)

const SECRET_KEYS = ['HOST', 'DATABASE', 'USERNAME', 'PASSWORD'].map(
  (suffix) => `SECRET_BOSSANOVA_WAREHOUSE_${suffix}`,
)

// Returns the body of the first top-level `<kind> "<a>" ["<b>"] {` block, matched by
// brace depth so nested blocks stay inside it.
export function terraformBlock(source, header) {
  const start = source.indexOf(`${header} {`)
  assert.notEqual(start, -1, `block not found: ${header}`)
  let depth = 0
  for (let i = source.indexOf('{', start); i < source.length; i++) {
    if (source[i] === '{') depth++
    if (source[i] === '}' && --depth === 0) return source.slice(start, i + 1)
  }
  assert.fail(`unterminated block: ${header}`)
}

test('the warehouse user is created on the existing instance data source', async () => {
  const source = await readFile(moduleURL, 'utf8')
  const user = terraformBlock(source, 'resource "google_sql_user" "warehouse"')
  assert.match(user, /instance\s*=\s*data\.google_sql_database_instance\.bosso_existing\[0\]\.name/)
  assert.match(user, /name\s*=\s*var\.warehouse_database_user/)
  assert.match(user, /count\s*=\s*var\.enabled \? 1 : 0/)
  assert.doesNotMatch(source, /resource\s+"google_sql_database_instance"/)
})

test('the warehouse password has no special characters', async () => {
  const source = await readFile(moduleURL, 'utf8')
  const password = terraformBlock(source, 'resource "random_password" "warehouse"')
  assert.match(password, /special\s*=\s*false/)
  assert.match(password, /length\s*=\s*40/)
})

test('the module exposes the warehouse password only as a sensitive output', async () => {
  const source = await readFile(moduleOutputsURL, 'utf8')
  const output = terraformBlock(source, 'output "warehouse_database_password"')
  assert.match(output, /sensitive\s*=\s*true/)
})

test('the Kestra secret output is sensitive and base64-encodes each warehouse key once', async () => {
  const source = await readFile(rootOutputsURL, 'utf8')
  const output = terraformBlock(source, 'output "kestra_secret_bossanova_warehouse"')
  assert.match(output, /sensitive\s*=\s*true/)
  assert.match(output, /local\.gcp_sql_enabled \?/)
  for (const key of SECRET_KEYS) {
    const lines = output.split('\n').filter((line) => line.includes(`${key}=`))
    assert.equal(lines.length, 1, `${key} must appear exactly once`)
    assert.match(lines[0], new RegExp(`"${key}=\\$\\{base64encode\\(`), `${key} must be base64`)
  }
  // No fifth key may slip in unencoded: every assignment is one of the four above.
  assert.equal(output.match(/SECRET_BOSSANOVA_WAREHOUSE_[A-Z_]+=/g).length, SECRET_KEYS.length)
})

// BOS-1319: the PostHog batch-export user. It must sit on the existing instance,
// exist only in production (an internet-reachable password user nothing uses would
// be pure exposure), and never leak its host or password through a plain output.

test('the PostHog export user is created on the existing instance, gated by its flag', async () => {
  const source = await readFile(moduleURL, 'utf8')
  const user = terraformBlock(source, 'resource "google_sql_user" "posthog_export"')
  assert.match(user, /instance\s*=\s*data\.google_sql_database_instance\.bosso_existing\[0\]\.name/)
  assert.match(user, /name\s*=\s*var\.posthog_export_database_user/)
  assert.match(user, /count\s*=\s*var\.enabled && var\.posthog_export_enabled \? 1 : 0/)
  const password = terraformBlock(source, 'resource "random_password" "posthog_export"')
  assert.match(password, /count\s*=\s*var\.enabled && var\.posthog_export_enabled \? 1 : 0/)
  assert.match(password, /special\s*=\s*false/)
  assert.match(password, /length\s*=\s*40/)
})

test('the PostHog export is enabled only in production', async () => {
  const source = await readFile(rootMainURL, 'utf8')
  const lines = source.split('\n').filter((line) => /^\s*posthog_export_enabled\s*=/.test(line))
  // One local definition plus the module argument that passes it on.
  assert.equal(lines.length, 2, lines.join('\n'))
  assert.match(lines[0], /=\s*local\.gcp_sql_enabled && local\.env == "production"$/)
  assert.match(lines[1], /=\s*local\.posthog_export_enabled$/)
})

test('the module exposes the PostHog export password and the public IP only as sensitive outputs', async () => {
  const source = await readFile(moduleOutputsURL, 'utf8')
  for (const name of ['posthog_export_database_password', 'database_public_ip']) {
    assert.match(terraformBlock(source, `output "${name}"`), /sensitive\s*=\s*true/, name)
  }
})

test('the PostHog batch-export host and password outputs are sensitive', async () => {
  const source = await readFile(rootOutputsURL, 'utf8')
  for (const name of ['posthog_batch_export_host', 'posthog_batch_export_password']) {
    const output = terraformBlock(source, `output "${name}"`)
    assert.match(output, /sensitive\s*=\s*true/, name)
    assert.match(output, /local\.posthog_export_enabled \?/, name)
  }
})

test('the PostHog batch-export configuration names the raw_posthog landing tables', async () => {
  const source = await readFile(rootOutputsURL, 'utf8')
  const output = terraformBlock(source, 'output "posthog_batch_export_configuration"')
  assert.match(output, /schema\s*=\s*"raw_posthog"/)
  assert.match(output, /events_table\s*=\s*"events"/)
  assert.match(output, /persons_table\s*=\s*"persons"/)
  assert.match(output, /user\s*=\s*module\.gcp_sql\.posthog_export_database_user/)
  assert.match(output, /local\.posthog_export_enabled \?[\s\S]*: null/)
  // Non-secret by design, so no password or host may ride along in it.
  assert.doesNotMatch(output, /password|public_ip/)
})

test('terraformBlock rejects a missing block so a rename cannot pass vacuously', () => {
  assert.throws(() => terraformBlock('output "other" {}', 'output "absent"'), /block not found/)
})
