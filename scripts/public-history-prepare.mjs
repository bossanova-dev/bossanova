import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { git, gitEnv, MAX_BUFFER } from './public-history-git.mjs'
import {
  DEFAULT_SPEC_DIR,
  UsageError,
  globToRegex,
  loadSpec,
  readLines,
} from './public-history-spec.mjs'

const DEFAULT_TAG_KEEP = 'refs/tags/v*'

// ---------------------------------------------------------------------------------------------
// prepare

function nearestExistingDir(target) {
  let current = target
  while (!fs.existsSync(current)) {
    const parent = path.dirname(current)
    if (parent === current) return current
    current = parent
  }
  return current
}

function realpathOrSelf(target) {
  try {
    return fs.realpathSync(target)
  } catch {
    return target
  }
}

function isWithin(child, parent) {
  const rel = path.relative(parent, child)
  return rel === '' || (!rel.startsWith('..') && !path.isAbsolute(rel))
}

// The scratch path with its existing prefix resolved through symlinks, so /tmp vs /private/tmp
// and similar aliases cannot slip past the containment checks.
function resolvedScratch(scratch) {
  const anchor = nearestExistingDir(scratch)
  return path.join(realpathOrSelf(anchor), path.relative(anchor, scratch))
}

export function scratchRefusal({ source, scratch }) {
  if (fs.existsSync(scratch)) {
    if (!fs.statSync(scratch).isDirectory())
      return `scratch ${scratch} exists and is not a directory`
    if (fs.readdirSync(scratch).length > 0) return `scratch ${scratch} exists and is not empty`
  }
  const resolved = resolvedScratch(scratch)
  if (fs.existsSync(source)) {
    const roots = []
    for (const flag of ['--show-toplevel', '--absolute-git-dir', '--git-common-dir']) {
      const out = git(source, ['rev-parse', '--path-format=absolute', flag], { allowFailure: true })
      if (out.status === 0 && out.text.trim()) roots.push(realpathOrSelf(out.text.trim()))
    }
    roots.push(realpathOrSelf(path.resolve(source)))
    const inside = roots.find((root) => isWithin(resolved, root))
    if (inside) return `scratch ${scratch} lies inside the source repository (${inside})`
  }
  const anchor = nearestExistingDir(scratch)
  const probe = git(anchor, ['rev-parse', '--is-inside-work-tree', '--is-inside-git-dir'], {
    allowFailure: true,
  })
  if (probe.status === 0 && probe.text.split('\n').includes('true')) {
    return `scratch ${scratch} lies inside a git repository or work tree (${anchor})`
  }
  return null
}

function refPatternToRegex(pattern) {
  const full = pattern.startsWith('refs/')
    ? [pattern]
    : [`refs/heads/${pattern}`, `refs/tags/${pattern}`]
  return full.map((p) => globToRegex(p))
}

export function selectKeptRefs(refs, patterns) {
  const res = patterns.flatMap(refPatternToRegex)
  return refs.filter((ref) => res.some((re) => re.test(ref)))
}

// Copies of the spec in the exact shape git-filter-repo reads: replacements without comments.
function writeFilterInputs(spec, dir) {
  const inputs = {
    paths: path.join(dir, 'paths.txt'),
    replacements: path.join(dir, 'replacements.txt'),
  }
  fs.copyFileSync(spec.files.paths, inputs.paths)
  const replacementLines = readLines(spec.files.replacements).filter(
    (line) => line !== '' && !line.startsWith('#'),
  )
  fs.writeFileSync(inputs.replacements, replacementLines.join('\n') + '\n')
  if (spec.hasMailmap) {
    inputs.mailmap = path.join(dir, 'mailmap')
    fs.copyFileSync(spec.files.mailmap, inputs.mailmap)
  }
  return inputs
}

export function filterRepoArgs(inputs) {
  const args = [
    '--force',
    '--replace-refs',
    'delete-no-add',
    '--invert-paths',
    '--paths-from-file',
    inputs.paths,
    '--replace-text',
    inputs.replacements,
    '--replace-message',
    inputs.replacements,
  ]
  if (inputs.mailmap) args.push('--mailmap', inputs.mailmap)
  return args
}

export function prepare({
  source,
  scratch,
  spec: specDir = DEFAULT_SPEC_DIR,
  keepRefs = [],
  filterRepo = 'git-filter-repo',
  log = (line) => process.stderr.write(`${line}\n`),
}) {
  if (!source) throw new UsageError('prepare needs --source')
  if (!scratch) throw new UsageError('prepare needs --scratch')
  const spec = loadSpec(specDir)
  const scratchDir = path.resolve(scratch)
  const refusal = scratchRefusal({ source, scratch: scratchDir })
  if (refusal) throw new UsageError(`refusing to prepare: ${refusal}`)

  fs.mkdirSync(scratchDir, { recursive: true })
  const cloneSource = fs.existsSync(source) ? path.resolve(source) : source
  git(path.dirname(scratchDir), [
    'clone',
    '--quiet',
    '--mirror',
    '--no-local',
    '--',
    cloneSource,
    scratchDir,
  ])

  const head = git(scratchDir, ['symbolic-ref', '-q', 'HEAD'], { allowFailure: true }).text.trim()
  const patterns = keepRefs.length > 0 ? keepRefs : [head, DEFAULT_TAG_KEEP].filter(Boolean)
  const refs = git(scratchDir, ['for-each-ref', '--format=%(refname)'])
    .text.split('\n')
    .filter(Boolean)
  const kept = selectKeptRefs(refs, patterns)
  if (kept.length === 0) {
    throw new UsageError(`no ref in ${cloneSource} matches --keep-ref ${patterns.join(', ')}`)
  }
  const dropped = refs.filter((ref) => !kept.includes(ref))
  if (dropped.length > 0) {
    git(scratchDir, ['update-ref', '--stdin'], {
      input: dropped.map((ref) => `delete ${ref}\n`).join(''),
    })
  }
  const keptBranches = kept.filter((ref) => ref.startsWith('refs/heads/'))
  if (!kept.includes(head) && keptBranches.length > 0) {
    git(scratchDir, ['symbolic-ref', 'HEAD', keptBranches[0]])
  }

  // Remove the remote configuration directly: `git remote remove` on a mirror clone also walks
  // its `+refs/*:refs/*` refspec, and the refs were already pruned above.
  for (const remote of git(scratchDir, ['remote']).text.split('\n').filter(Boolean)) {
    git(scratchDir, ['config', '--remove-section', `remote.${remote}`])
  }
  if (git(scratchDir, ['remote']).text.trim() !== '') {
    throw new Error(`remotes remain in ${scratchDir}`)
  }

  const inputDir = fs.mkdtempSync(path.join(os.tmpdir(), 'public-history-spec-'))
  try {
    const inputs = writeFilterInputs(spec, inputDir)
    const args = filterRepoArgs(inputs)
    log(`running ${filterRepo} in ${scratchDir}`)
    const run = spawnSync(filterRepo, args, {
      cwd: scratchDir,
      env: gitEnv(),
      encoding: 'utf8',
      maxBuffer: MAX_BUFFER,
    })
    for (const line of `${run.stdout ?? ''}${run.stderr ?? ''}`.split('\n').filter(Boolean)) {
      log(`  ${line}`)
    }
    if (run.error) {
      const hint =
        /** @type {NodeJS.ErrnoException} */ (run.error).code === 'ENOENT'
          ? ' (install git-filter-repo or pass --filter-repo)'
          : ''
      throw new Error(`cannot run ${filterRepo}: ${run.error.message}${hint}`)
    }
    if (run.status !== 0) throw new Error(`${filterRepo} exited ${run.status}`)
  } finally {
    fs.rmSync(inputDir, { recursive: true, force: true })
  }

  const finalRefs = git(scratchDir, ['for-each-ref', '--format=%(refname)'])
    .text.split('\n')
    .filter(Boolean)
  const remotes = git(scratchDir, ['remote']).text.split('\n').filter(Boolean)
  if (remotes.length > 0)
    throw new Error(`remotes reappeared in ${scratchDir}: ${remotes.join(', ')}`)
  log(`prepared ${scratchDir}: kept ${finalRefs.join(', ')}; dropped ${dropped.length} refs`)
  return { scratch: scratchDir, keptRefs: finalRefs, droppedRefs: dropped }
}
