#!/usr/bin/env node

import { execFileSync, spawnSync } from 'node:child_process'
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

/**
 * Paths the release workflows commit to staging/production and never to main. An `image-pins`
 * file is exempt only in the image-tag lines perform-*-release.yml rewrite; any other edit to it
 * is drift like any other file.
 */
/** @type {ReadonlyArray<{kind: 'file' | 'dir' | 'image-pins', path: string}>} */
export const RELEASE_OWNED = Object.freeze([
  { kind: 'file', path: 'CHANGELOG.md' },
  { kind: 'file', path: '.VERSION' },
  { kind: 'dir', path: 'services/marketing/src/content/changelog/' },
  { kind: 'image-pins', path: 'infra/kustomize/overlays/staging/kustomization.yml' },
  { kind: 'image-pins', path: 'infra/kustomize/overlays/production/kustomization.yml' },
])

// The sed in perform-*-release.yml matches `newTag: ['"][^'"]*['"] *# bosso` (and `# mcp-gateway`)
// and rewrites the quoted tag and the spaces after it to `'<version>'  `. Only that span is blanked,
// so a tab separator or any other edit on the line stays visible. The comment must end the line:
// sed would also rewrite `# bosso-canary`, but the overlays have no such line, so it reads as drift.
const IMAGE_PIN_LINE = /^([ \t]*newTag: )['"][^'"]*['"] *(# (?:bosso|mcp-gateway)[ \t]*)$/gm

export function stripImagePins(text) {
  return text.replace(IMAGE_PIN_LINE, '$1<pinned>  $2')
}

/** @returns {'file' | 'dir' | 'image-pins' | null} How the release workflows own `file`. */
export function releaseOwnership(file) {
  const owned = RELEASE_OWNED.find((entry) =>
    entry.kind === 'dir' ? file.startsWith(entry.path) : file === entry.path,
  )
  return owned ? owned.kind : null
}

function readBlob(treeish, file, cwd) {
  const shown = spawnSync('git', ['show', `${treeish}:${file}`], { cwd, encoding: 'utf8' })
  return shown.status === 0 ? shown.stdout : null
}

function onlyPinsDiffer({ head, merge, file, cwd }) {
  const before = readBlob(head, file, cwd)
  const after = readBlob(merge, file, cwd)
  return before !== null && after !== null && stripImagePins(before) === stripImagePins(after)
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
  return out.split('\0').filter((file) => {
    if (file === '') return false
    const ownership = releaseOwnership(file)
    if (ownership === 'image-pins') return !onlyPinsDiffer({ head, merge, file, cwd })
    return ownership === null
  })
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
      'The two kustomization.yml files are exempt only in their pinned newTag lines.',
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
