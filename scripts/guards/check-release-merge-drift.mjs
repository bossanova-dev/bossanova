#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

// A release PR merges a candidate head (main's tip, or a fix cherry-picked onto it) into staging or
// production. That merge must not keep any release-branch content the head lacks, outside the
// files the release workflows themselves write. When it does, the release branch has drifted from
// main: services/bossd/internal/plugin/host_service_test.go kept a hunk main had reverted, because
// release PR #3095 carried an unsquashed commit and the later merge base predated it.
//
// The check reads the merge result, not the route to it. A cherry-picked release fix passes as
// long as the merge adds nothing; an ancestry or patch-id check cannot tell that flow from the one
// that caused the drift.

/** Paths the release workflows commit to staging/production and never to main. */
/** @type {ReadonlyArray<{kind: 'file' | 'dir', path: string}>} */
export const RELEASE_OWNED = Object.freeze([
  { kind: 'file', path: 'CHANGELOG.md' },
  { kind: 'file', path: '.VERSION' },
  { kind: 'dir', path: 'services/marketing/src/content/changelog/' },
])

/** @returns {'file' | 'dir' | null} How the release workflows own `file`. */
export function releaseOwnership(file) {
  const owned = RELEASE_OWNED.find((entry) =>
    entry.kind === 'dir' ? file.startsWith(entry.path) : file === entry.path,
  )
  return owned ? owned.kind : null
}

/**
 * Files whose merge result differs from the release head, minus what the release workflows write.
 *
 * @param {{head: string, merge: string, cwd?: string}} refs Any tree-ish for `merge`.
 * @returns {string[]}
 */
export function findReleaseDrift({ head, merge, cwd = process.cwd() }) {
  const out = execFileSync('git', ['diff', '--no-renames', '--name-only', '-z', head, merge], {
    cwd,
    encoding: 'utf8',
  })
  return out.split('\0').filter((file) => file !== '' && releaseOwnership(file) === null)
}

export function renderVerdict({ head, drift }) {
  if (drift.length === 0) {
    return {
      ok: true,
      text: `Release merge keeps nothing the head ${head} lacks outside release-owned files.`,
    }
  }
  return {
    ok: false,
    text: [
      `::error::The release merge keeps content the release head lacks in ${drift.length} file(s). The release branch has drifted from main.`,
      ...drift.map((file) => `  - ${file}`),
      '',
      `Inspect each with: git diff ${head} HEAD -- <file>`,
      'If the release-branch copy is right, land it on main, then cut a new release or cherry-pick',
      'the main commit onto the release candidate branch. If main is right, open a PR into the',
      'release branch that makes the file match main. Either way the merge must add nothing.',
      `Release-owned files are exempt: ${RELEASE_OWNED.map((owned) => owned.path).join(', ')}.`,
    ].join('\n'),
  }
}

function main() {
  const { values } = parseArgs({
    options: {
      head: { type: 'string' },
      merge: { type: 'string', default: 'HEAD' },
    },
    strict: true,
    allowPositionals: false,
  })
  if (!values.head) {
    console.error(
      'usage: check-release-merge-drift.mjs --head <release-head-sha> [--merge <tree-ish>]',
    )
    process.exit(2)
  }
  const verdict = renderVerdict({
    head: values.head,
    drift: findReleaseDrift({ head: values.head, merge: values.merge }),
  })
  if (!verdict.ok) {
    console.error(verdict.text)
    process.exit(1)
  }
  console.log(verdict.text)
}

if (isMainModule(import.meta.url)) main()
