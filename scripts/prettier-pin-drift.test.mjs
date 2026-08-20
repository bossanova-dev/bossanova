// Guard: the repo root and the standalone `services/docs` Docusaurus package must
// resolve the SAME prettier version. Both packages' markdown is prettier-checked by
// `make lint` — `$(MAKE) -C services/docs lint` uses the docs-local binary while
// `scripts/lint-docs-affected.mjs` prefers the ROOT binary — and neither package
// ships its own `.prettierrc`, so both resolve the repo-root config. Two engines
// reading one config is how a file passes one leg and fails the other (BOS-795).
//
// `services/docs` is installed with `--ignore-workspace` and owns a separate
// lockfile, so pnpm's workspace resolution cannot deduplicate the two ranges for
// us. This assertion is the honest mechanism keeping them on one engine.
//
// Two layers, because the declared range alone is not enough: `^3.9.6` in both
// manifests is still satisfied by root 3.9.7 and docs 3.9.6, so a `pnpm update
// prettier` run in only one tree restores the exact skew this gate exists to close
// while every declared range still agrees. The lockfiles record what each tree
// ACTUALLY installs, so they are the layer that binds.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

// Repo root is the parent of scripts/.
const repoRoot = fileURLToPath(new URL('..', import.meta.url))

export const ROOT_MANIFEST = 'package.json'
export const DOCS_MANIFEST = 'services/docs/package.json'
export const ROOT_LOCKFILE = 'pnpm-lock.yaml'
export const DOCS_LOCKFILE = 'services/docs/pnpm-lock.yaml'

/** The prettier range a parsed package.json declares, or null when it declares none. */
function prettierRange(pkg) {
  const range = pkg?.devDependencies?.prettier ?? pkg?.dependencies?.prettier
  return typeof range === 'string' && range.trim() !== '' ? range.trim() : null
}

/**
 * Pure comparison over two parsed package.json payloads. Returns
 * `{ agree, rootRange, docsRange, message }`; the message names BOTH declared
 * values so a failure reads as "which two disagree", not just "they disagree".
 * A manifest that declares no prettier at all is drift too — silently dropping
 * the dependency would otherwise satisfy an equality-only check.
 */
export function comparePrettierPins(rootPkg, docsPkg, labels = {}) {
  const rootLabel = labels.root ?? ROOT_MANIFEST
  const docsLabel = labels.docs ?? DOCS_MANIFEST
  const rootRange = prettierRange(rootPkg)
  const docsRange = prettierRange(docsPkg)
  const shown = (range) => range ?? '<no prettier dependency>'
  const agree = rootRange !== null && docsRange !== null && rootRange === docsRange
  const message = agree
    ? `${rootLabel} and ${docsLabel} both declare prettier ${rootRange}`
    : `prettier pin drift: ${rootLabel} declares ${shown(rootRange)} but ${docsLabel} declares ` +
      `${shown(docsRange)} — both trees are prettier-checked by \`make lint\` against the same ` +
      `root .prettierrc, so they must declare one range (see docs/build-and-ci.md)`
  return { agree, rootRange, docsRange, message }
}

/**
 * The body of a pnpm lockfile's top-level `importers:` mapping, or null when the
 * lockfile has none. Scoping to that block matters: `packages:` and `snapshots:`
 * mention `prettier` many times over (peer dependencies of the Astro/Volar
 * language services), and none of those are what either tree installs as its
 * formatter.
 */
function importersBlock(lockText) {
  const lines = lockText.split('\n')
  const start = lines.findIndex((line) => line === 'importers:')
  if (start === -1) return null
  let end = lines.length
  for (let i = start + 1; i < lines.length; i += 1) {
    if (/^\S/.test(lines[i])) {
      end = i
      break
    }
  }
  return lines.slice(start + 1, end).join('\n')
}

// An importer's direct dependency entry, e.g.
//       prettier:
//         specifier: ^3.9.6
//         version: 3.9.6
const IMPORTER_PRETTIER =
  /^[ \t]+prettier:\n[ \t]+specifier:[ \t]*(\S+)\n[ \t]+version:[ \t]*(\S+)[ \t]*$/gm

/**
 * The prettier version a pnpm lockfile resolves for its importers. Returns
 * `{ version, reason }` — `version` is null whenever no single answer exists, and
 * `reason` then says why, so the failure message can name the actual defect
 * (missing section, dropped dependency, importers that disagree with each other).
 */
export function resolvedPrettierVersion(lockText) {
  const block = importersBlock(lockText)
  if (block === null) return { version: null, reason: 'no `importers:` section' }
  const matches = [...block.matchAll(IMPORTER_PRETTIER)]
  if (matches.length === 0) return { version: null, reason: 'no importer resolves prettier' }
  const versions = [...new Set(matches.map((match) => match[2]))]
  if (versions.length > 1) {
    return { version: null, reason: `importers disagree (${versions.join(', ')})` }
  }
  return { version: versions[0], reason: null }
}

/**
 * Pure comparison over two pnpm lockfile texts. This is the layer that actually
 * binds the two trees to one engine: matching caret ranges still permit different
 * resolved versions, and the resolved version is what each `prettier --check` leg
 * runs. The message names BOTH resolved values.
 */
export function compareResolvedPrettier(rootLockText, docsLockText, labels = {}) {
  const rootLabel = labels.root ?? ROOT_LOCKFILE
  const docsLabel = labels.docs ?? DOCS_LOCKFILE
  const root = resolvedPrettierVersion(rootLockText)
  const docs = resolvedPrettierVersion(docsLockText)
  const shown = (side) => side.version ?? `<unresolved: ${side.reason}>`
  const agree = root.version !== null && docs.version !== null && root.version === docs.version
  const message = agree
    ? `${rootLabel} and ${docsLabel} both resolve prettier ${root.version}`
    : `prettier resolved-version drift: ${rootLabel} resolves ${shown(root)} but ${docsLabel} ` +
      `resolves ${shown(docs)} — matching caret ranges are not enough, since a \`pnpm update ` +
      `prettier\` in one tree alone moves that tree's engine while both manifests still agree. ` +
      `Re-run \`cd services/docs && pnpm install --ignore-workspace\` after bumping the root ` +
      `(see docs/build-and-ci.md)`
  return { agree, rootVersion: root.version, docsVersion: docs.version, message }
}

function readRepoFile(relativePath) {
  return fs.readFileSync(path.join(repoRoot, relativePath), 'utf8')
}

function readManifest(relativePath) {
  return JSON.parse(readRepoFile(relativePath))
}

function lockWithPrettier(version) {
  return [
    "lockfileVersion: '9.0'",
    '',
    'importers:',
    '',
    '  .:',
    '    devDependencies:',
    '      prettier:',
    `        specifier: ^3.9.6`,
    `        version: ${version}`,
    '',
    'packages:',
    '',
    '  prettier@3.0.0:',
    '    peerDependencies:',
    '      prettier: ^3.0.0',
    '',
    // Shaped exactly like an importer entry, at a different version. Deleting the
    // `importers:` scoping makes the parser see two versions here and collapse to
    // null, so this block is what gives that scoping its coverage.
    'snapshots:',
    '',
    '  eslint-config-decoy@1.0.0:',
    '    dependencies:',
    '      prettier:',
    '        specifier: ^3.0.0',
    '        version: 3.0.0',
    '',
  ].join('\n')
}

test('(a) comparePrettierPins reds on disagreeing synthetic payloads and names both values', () => {
  // Synthetic payload 1: the exact skew BOS-795 closed (root 3.9.6 vs docs 3.8.3).
  const skew = comparePrettierPins(
    { devDependencies: { prettier: '^3.9.6' } },
    { devDependencies: { prettier: '^3.8.3' } },
  )
  assert.equal(skew.agree, false, 'a differing pair must not be reported as agreeing')
  assert.match(skew.message, /\^3\.9\.6/, 'message must name the root range')
  assert.match(skew.message, /\^3\.8\.3/, 'message must name the services/docs range')

  // Synthetic payload 2: a dropped dependency is drift, not a vacuous pass.
  const missing = comparePrettierPins(
    { devDependencies: { prettier: '^3.9.6' } },
    { devDependencies: {} },
  )
  assert.equal(
    missing.agree,
    false,
    'a missing prettier dependency must not be reported as agreeing',
  )
  assert.match(missing.message, /\^3\.9\.6/, 'message must name the range that IS declared')
  assert.match(missing.message, /<no prettier dependency>/, 'message must name the absent side')
})

test('(b) comparePrettierPins passes on an agreeing synthetic payload', () => {
  const same = comparePrettierPins(
    { devDependencies: { prettier: '^3.9.6' } },
    { devDependencies: { prettier: '^3.9.6' } },
  )
  assert.equal(same.agree, true)
  assert.match(same.message, /\^3\.9\.6/)
})

test('(c) the real root and services/docs manifests declare the same prettier range', () => {
  const result = comparePrettierPins(readManifest(ROOT_MANIFEST), readManifest(DOCS_MANIFEST))
  assert.ok(result.rootRange, `${ROOT_MANIFEST} must declare a prettier dependency`)
  assert.ok(result.docsRange, `${DOCS_MANIFEST} must declare a prettier dependency`)
  assert.ok(result.agree, result.message)
})

test('(d) compareResolvedPrettier reds when matching ranges resolve to different versions', () => {
  // The skew the range check alone cannot see: both manifests still say ^3.9.6.
  const drifted = compareResolvedPrettier(lockWithPrettier('3.9.7'), lockWithPrettier('3.9.6'))
  assert.equal(drifted.agree, false, 'differing resolved versions must not be reported as agreeing')
  assert.match(drifted.message, /3\.9\.7/, 'message must name the root resolved version')
  assert.match(drifted.message, /3\.9\.6/, 'message must name the services/docs resolved version')

  // A lockfile whose importers stopped resolving prettier is drift, not a pass.
  const dropped = compareResolvedPrettier(lockWithPrettier('3.9.6'), "lockfileVersion: '9.0'\n")
  assert.equal(dropped.agree, false, 'an unresolvable side must not be reported as agreeing')
  assert.match(dropped.message, /<unresolved: /, 'message must say why the side is unresolvable')

  // Only importer entries count. The fixture plants a decoy under `snapshots:` that is
  // byte-shaped like an importer entry but at 3.0.0, so this reds if the `importers:`
  // scoping is ever dropped — the guarantee is tested, not just asserted in a comment.
  assert.equal(resolvedPrettierVersion(lockWithPrettier('3.9.6')).version, '3.9.6')
})

test('(e) the real root and services/docs lockfiles resolve the same prettier version', () => {
  const result = compareResolvedPrettier(readRepoFile(ROOT_LOCKFILE), readRepoFile(DOCS_LOCKFILE))
  assert.ok(result.rootVersion, `${ROOT_LOCKFILE} must resolve prettier`)
  assert.ok(result.docsVersion, `${DOCS_LOCKFILE} must resolve prettier`)
  assert.ok(result.agree, result.message)
})
