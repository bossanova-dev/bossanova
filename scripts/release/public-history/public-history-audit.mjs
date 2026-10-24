import fs from 'node:fs'
import path from 'node:path'

import { git } from './public-history-git.mjs'
import {
  DEFAULT_SPEC_DIR,
  UsageError,
  loadSpec,
  matchingPathRule,
  replacementMarker,
  replacementRuleMatches,
} from './public-history-spec.mjs'
import { forEachBlob, readCommits, readTags, walkHistory } from './public-history-history.mjs'
import {
  bump,
  countEmails,
  isBinary,
  newReportScans,
  scanReportOnly,
} from './public-history-scan.mjs'
import { LARGEST_BLOBS, renderReport } from './public-history-report.mjs'

export async function audit({ repo, spec: specDir = DEFAULT_SPEC_DIR, report }) {
  if (!repo) throw new UsageError('audit needs --repo')
  if (!report) throw new UsageError('audit needs --report')
  const spec = loadSpec(specDir)
  const repoDir = path.resolve(repo)
  if (git(repoDir, ['rev-parse', '--git-dir'], { allowFailure: true }).status !== 0) {
    throw new UsageError(`${repoDir} is not a git repository`)
  }

  const hard = []
  const { pathsSeen, blobs } = walkHistory(repoDir)

  // Hard: forbidden paths in any commit of any ref.
  for (const [pathname, entry] of [...pathsSeen].sort(([a], [b]) => a.localeCompare(b))) {
    const rule = matchingPathRule(spec.pathRules, pathname)
    if (rule) {
      hard.push({
        check: 'forbidden-path',
        rule: rule.text,
        commit: entry.commit,
        path: pathname,
        where: `path (in ${entry.commits.size} commit(s))`,
      })
    }
  }

  // Hard: replacement left-hand sides in paths, messages, identities.
  const lhsHit = (where, text, commit, pathname) => {
    for (const rule of spec.replacements) {
      if (replacementRuleMatches(rule, text)) {
        hard.push({ check: 'replacement-lhs', rule: rule.text, commit, path: pathname, where })
      }
    }
  }
  for (const [pathname, entry] of pathsSeen) lhsHit('path', pathname, entry.commit, pathname)
  const commits = readCommits(repoDir)
  for (const c of commits) {
    lhsHit('commit message', c.message, c.sha, '')
    lhsHit('author identity', c.author, c.sha, '')
    lhsHit('committer identity', c.committer, c.sha, '')
  }
  const tags = readTags(repoDir)
  for (const tag of tags) {
    lhsHit('tag message', tag.message, '', tag.ref)
    lhsHit('tagger identity', tag.tagger, '', tag.ref)
  }

  // Tip: HEAD's tree, for history-only paths and replacement markers.
  const tipTree = git(repoDir, ['-c', 'core.quotePath=false', 'ls-tree', '-r', '-z', 'HEAD'], {
    allowFailure: true,
  })
  const tipBlobs = new Map() // oid -> [paths]
  const tipPaths = new Set()
  if (tipTree.status === 0) {
    for (const record of tipTree.text.split('\0').filter(Boolean)) {
      const tab = record.indexOf('\t')
      const [, type, oid] = record.slice(0, tab).split(' ')
      const pathname = record.slice(tab + 1)
      tipPaths.add(pathname)
      if (type === 'blob') bump(tipBlobs, oid, () => []).push(pathname)
    }
  }
  const markers = spec.replacements
    .map((rule) => ({ rule, marker: replacementMarker(rule) }))
    .filter((m) => m.marker)

  // Blob pass: hard LHS check, report-only scans, sizes, tip markers.
  const scans = newReportScans()
  const { secretHits, allowlisted, emails, homes, hostnames } = scans
  const sizes = []
  const tipChanged = new Map() // path -> Set(rule)
  await forEachBlob(repoDir, [...blobs.keys()], (oid, data, size) => {
    const origin = blobs.get(oid)
    sizes.push({ oid, size, path: origin.path, commit: origin.commit })
    const binary = isBinary(data)
    const text = data.toString(binary ? 'latin1' : 'utf8')
    for (const rule of spec.replacements) {
      if (replacementRuleMatches(rule, text)) {
        hard.push({
          check: 'replacement-lhs',
          rule: rule.text,
          commit: origin.commit,
          path: origin.path,
          where: binary ? 'blob (binary; filter-repo skips binary blobs)' : 'blob',
        })
      }
    }
    const tipAt = tipBlobs.get(oid)
    if (tipAt && !binary) {
      for (const { rule, marker } of markers) {
        if (text.includes(marker)) {
          for (const p of tipAt) bump(tipChanged, p, () => new Set()).add(rule.text)
        }
      }
    }
    if (!binary) scanReportOnly(scans, spec.allowlist, text, origin)
  })
  for (const c of commits) {
    countEmails(emails, `${c.author}\n${c.committer}`)
  }

  const refs = git(repoDir, ['for-each-ref', '--format=%(refname) %(objectname)'])
    .text.split('\n')
    .filter(Boolean)
  const counts = Object.fromEntries(
    git(repoDir, ['count-objects', '-v'])
      .text.split('\n')
      .filter(Boolean)
      .map((line) => line.split(': ')),
  )
  const commitCount = Number(git(repoDir, ['rev-list', '--all', '--count']).text.trim() || 0)
  const historyOnly = [...pathsSeen.keys()].filter((p) => !tipPaths.has(p)).sort()
  sizes.sort((a, b) => b.size - a.size)

  const result = {
    repo: repoDir,
    spec: spec.dir,
    hardFindings: hard,
    secretHits: [...secretHits.values()],
    allowlisted: [...allowlisted.values()],
    emails,
    homes,
    hostnames,
    refs,
    commitCount,
    objectCount: Number(counts.count ?? 0) + Number(counts['in-pack'] ?? 0),
    packedKiB: Number(counts['size-pack'] ?? 0),
    largestBlobs: sizes.slice(0, LARGEST_BLOBS),
    historyOnly,
    tipChanged: [...tipChanged]
      .map(([p, rules]) => ({ path: p, rules: [...rules] }))
      .sort((a, b) => a.path.localeCompare(b.path)),
  }
  fs.mkdirSync(path.dirname(path.resolve(report)), { recursive: true })
  fs.writeFileSync(report, renderReport(result))
  return result
}
