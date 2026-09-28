import assert from 'node:assert/strict'
import { readdir, readFile } from 'node:fs/promises'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

// BOS-1316: the static contract of the claw reader's Terraform. `terraform
// validate` checks neither default values nor IAM condition content, so this
// reads the sources: both flags default off, the module call is production-only,
// the IAM user has the IAM service-account type, every project binding is
// conditioned on one Cloud SQL instance, and no service-account key exists.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const moduleDir = path.join(repoRoot, 'infra/modules/gcp-sql')
const rootDir = path.join(repoRoot, 'infra/environments')
const keyScanDirs = [path.join(repoRoot, 'infra/modules'), rootDir]

const read = (file) => readFile(file, 'utf8')

/** Every .tf file under dir, recursively, skipping provider caches. */
async function terraformFiles(dir) {
  const found = []
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    if (entry.name === '.terraform') continue
    const absolute = path.join(dir, entry.name)
    if (entry.isDirectory()) found.push(...(await terraformFiles(absolute)))
    else if (entry.isFile() && entry.name.endsWith('.tf')) found.push(absolute)
  }
  return found.sort()
}

/**
 * The body of every block whose header matches `header` (a regex source up to,
 * not including, the opening brace). Braces inside double-quoted strings are
 * ignored, so a `${...}` interpolation does not end a block early.
 */
function blocks(source, header) {
  const found = []
  const pattern = new RegExp(`^[ \\t]*${header}\\s*\\{`, 'gm')
  for (const match of source.matchAll(pattern)) {
    const start = match.index + match[0].length
    let depth = 1
    let inString = false
    let i = start
    for (; i < source.length && depth > 0; i++) {
      const ch = source[i]
      if (inString) {
        if (ch === '\\') i++
        else if (ch === '"') inString = false
      } else if (ch === '"') inString = true
      else if (ch === '{') depth++
      else if (ch === '}') depth--
    }
    assert.equal(depth, 0, `unbalanced braces after ${match[0].trim()}`)
    found.push(source.slice(start, i - 1))
  }
  return found
}

function onlyBlock(source, header, what) {
  const found = blocks(source, header)
  assert.equal(found.length, 1, `expected exactly one ${what}, found ${found.length}`)
  return found[0]
}

/** The right-hand side of `name = ...` on one line of a block body. */
function attribute(body, name) {
  const match = body.match(new RegExp(`^[ \\t]*${name}\\s*=\\s*(.+?)\\s*$`, 'm'))
  return match?.[1]
}

/**
 * Resolve an attribute value that is a bare `local.<name>` reference against the
 * `locals` blocks of the same source, so a condition held in a local is checked
 * where it is defined.
 */
function resolveLocal(source, value) {
  const ref = value?.match(/^local\.([A-Za-z0-9_]+)$/)
  if (!ref) return value
  for (const body of blocks(source, 'locals')) {
    const resolved = attribute(body, ref[1])
    if (resolved !== undefined) return resolved
  }
  assert.fail(`${value} is not defined in any locals block`)
}

/**
 * Names of `google_project_iam_member` resources in source whose `condition`
 * expression does not scope them to one Cloud SQL instance.
 */
function unconditionedBindings(source) {
  const offenders = []
  const header = 'resource\\s+"google_project_iam_member"\\s+"([^"]+)"'
  const names = [...source.matchAll(new RegExp(header, 'g'))].map((m) => m[1])
  blocks(source, header).forEach((body, index) => {
    const conditions = blocks(body, 'condition')
    const expression =
      conditions.length === 1
        ? resolveLocal(source, attribute(conditions[0], 'expression'))
        : undefined
    const scoped =
      expression !== undefined &&
      expression.includes('sqladmin.googleapis.com/Instance') &&
      expression.includes('resource.name ==')
    if (!scoped) offenders.push(names[index])
  })
  return offenders
}

for (const [label, dir] of [
  ['gcp-sql module', moduleDir],
  ['root module', rootDir],
]) {
  test(`${label} declares both claw flags with default = false`, async () => {
    const source = await read(path.join(dir, 'variables.tf'))
    for (const flag of ['claw_reader_enabled', 'claw_reader_bindings_enabled']) {
      const body = onlyBlock(source, `variable\\s+"${flag}"`, `variable "${flag}"`)
      assert.equal(attribute(body, 'type'), 'bool', `${flag} type`)
      assert.equal(attribute(body, 'default'), 'false', `${flag} default`)
    }
  })
}

test('the module call enables the claw only in production', async () => {
  const source = await read(path.join(rootDir, 'main.tf'))
  const body = onlyBlock(source, 'module\\s+"gcp_sql"', 'module "gcp_sql"')
  for (const flag of ['claw_reader_enabled', 'claw_reader_bindings_enabled']) {
    const value = attribute(body, flag)
    assert.ok(value, `module "gcp_sql" does not set ${flag}`)
    assert.match(value, new RegExp(`\\bvar\\.${flag}\\b`), `${flag} must follow its root variable`)
    assert.match(
      value,
      /local\.env == "production"/,
      `${flag} must be gated on local.env == "production"`,
    )
  }
})

test('the claw IAM database user is a Cloud SQL IAM service-account user', async () => {
  const source = await read(path.join(moduleDir, 'main.tf'))
  const body = onlyBlock(
    source,
    'resource\\s+"google_sql_user"\\s+"claw_reader"',
    'google_sql_user.claw_reader',
  )
  assert.equal(attribute(body, 'type'), '"CLOUD_IAM_SERVICE_ACCOUNT"')
  assert.match(attribute(body, 'name') ?? '', /trimsuffix\(.*\.email, "\.gserviceaccount\.com"\)/)
})

test('every project IAM binding is conditioned on one Cloud SQL instance', async () => {
  const files = await terraformFiles(path.join(repoRoot, 'infra'))
  let bindings = 0
  for (const file of files) {
    const source = await read(file)
    bindings += blocks(source, 'resource\\s+"google_project_iam_member"\\s+"[^"]+"').length
    assert.deepEqual(
      unconditionedBindings(source),
      [],
      `${path.relative(repoRoot, file)} has an unconditioned binding`,
    )
  }
  assert.ok(
    bindings >= 1,
    'no google_project_iam_member found; the claw bindings moved or were renamed',
  )
})

test('no Terraform file declares or outputs a service-account key', async () => {
  const files = (await Promise.all(keyScanDirs.map(terraformFiles))).flat()
  assert.ok(files.length > 0, 'found no .tf files to scan')
  for (const file of files) {
    const source = await read(file)
    assert.doesNotMatch(
      source,
      /google_service_account_key/,
      `${path.relative(repoRoot, file)} names a service-account key`,
    )
  }
})

test('the binding check fires on an unconditioned or wrongly scoped binding', () => {
  const source = `
locals {
  scoped = "resource.type == \\"sqladmin.googleapis.com/Instance\\" && resource.name == \\"projects/p/instances/i\\""
}
resource "google_project_iam_member" "ok" {
  role = "roles/cloudsql.client"
  condition {
    title      = "t"
    expression = local.scoped
  }
}
resource "google_project_iam_member" "bare" {
  role   = "roles/cloudsql.client"
  member = "serviceAccount:\${x}"
}
resource "google_project_iam_member" "project_wide" {
  role = "roles/cloudsql.instanceUser"
  condition {
    title      = "t"
    expression = "resource.service == \\"sqladmin.googleapis.com\\""
  }
}
`
  assert.deepEqual(unconditionedBindings(source), ['bare', 'project_wide'])
})
