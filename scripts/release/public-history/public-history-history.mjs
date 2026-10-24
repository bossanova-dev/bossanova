import { spawn } from 'node:child_process'
import { once } from 'node:events'

import { git, gitEnv } from './public-history-git.mjs'

// ---------------------------------------------------------------------------------------------
// audit: history walk

// Every (commit, path, blob) a tree in any ref ever held. `-m --root` makes each path appear in
// the diff of the commit that introduced it against every parent, so no tree entry is missed.
export function walkHistory(repo) {
  const out = git(repo, [
    '-c',
    'core.quotePath=false',
    'log',
    '--all',
    '--no-renames',
    '-m',
    '--root',
    '--raw',
    '--no-abbrev',
    '-z',
    '--format=%x01%H',
  ]).stdout
  const pathsSeen = new Map() // path -> { commit, commits: Set }
  const blobs = new Map() // oid -> { commit, path }
  let commit = null
  let meta = null
  let start = 0
  for (let i = 0; i <= out.length; i++) {
    if (i < out.length && out[i] !== 0) continue
    let token = out.subarray(start, i).toString('utf8')
    start = i + 1
    if (token.startsWith('\n')) token = token.slice(1)
    if (token === '') continue
    if (token.startsWith('\x01')) {
      commit = token.slice(1)
      continue
    }
    if (meta === null && token.startsWith(':')) {
      meta = token
      continue
    }
    if (meta !== null) {
      const [, newMode, , newOid, status] = meta.slice(1).split(' ')
      meta = null
      if (status === 'D') continue
      const entry = pathsSeen.get(token) ?? { commit, commits: new Set() }
      entry.commit = commit // log is newest-first: the last write is the oldest commit
      entry.commits.add(commit)
      pathsSeen.set(token, entry)
      // A gitlink (submodule) path is still a path the history holds, so it stays in pathsSeen
      // for the path checks; its oid names a commit in another repository, not a blob here.
      if (newMode !== '160000') blobs.set(newOid, { commit, path: token })
    }
  }
  return { pathsSeen, blobs }
}

export function readCommits(repo) {
  const out = git(repo, ['log', '--all', '--format=%x02%H%x00%an <%ae>%x00%cn <%ce>%x00%B']).text
  return out
    .split('\x02')
    .filter(Boolean)
    .map((record) => {
      const [sha, author, committer, ...message] = record.split('\x00')
      return { sha, author, committer, message: message.join('\x00') }
    })
}

export function readTags(repo) {
  const out = git(repo, [
    'for-each-ref',
    'refs/tags',
    '--format=%02%(refname)%00%(objecttype)%00%(taggername) %(taggeremail)%00%(contents)',
  ]).text
  return out
    .split('\x02')
    .filter(Boolean)
    .map((record) => {
      const [ref, type, tagger, ...message] = record.split('\x00')
      return { ref, annotated: type === 'tag', tagger, message: message.join('\x00') }
    })
    .filter((tag) => tag.annotated)
}

class ByteQueue {
  constructor() {
    this.chunks = []
    this.length = 0
  }
  push(chunk) {
    this.chunks.push(chunk)
    this.length += chunk.length
  }
  indexOfNewline() {
    let offset = 0
    for (const chunk of this.chunks) {
      const at = chunk.indexOf(10)
      if (at !== -1) return offset + at
      offset += chunk.length
    }
    return -1
  }
  take(n) {
    const parts = []
    let need = n
    while (need > 0) {
      const head = this.chunks[0]
      if (head.length <= need) {
        parts.push(head)
        this.chunks.shift()
        need -= head.length
      } else {
        parts.push(head.subarray(0, need))
        this.chunks[0] = head.subarray(need)
        need = 0
      }
    }
    this.length -= n
    return parts.length === 1 ? parts[0] : Buffer.concat(parts, n)
  }
}

// Streams `git cat-file --batch` so a large history never has to fit in one buffer.
export async function forEachBlob(repo, oids, visit) {
  const child = spawn('git', ['cat-file', '--batch'], { cwd: repo, env: gitEnv() })
  const stderr = []
  child.stderr.on('data', (chunk) => stderr.push(chunk))
  const exited = once(child, 'close')
  const writer = (async () => {
    for (const oid of oids) {
      if (!child.stdin.write(`${oid}\n`)) await once(child.stdin, 'drain')
    }
    child.stdin.end()
  })()
  const queue = new ByteQueue()
  const unreadable = [] // an object the walk named but cat-file could not return
  let header = null
  for await (const chunk of child.stdout) {
    queue.push(chunk)
    for (;;) {
      if (header === null) {
        const nl = queue.indexOfNewline()
        if (nl === -1) break
        const line = queue
          .take(nl + 1)
          .toString('utf8')
          .trimEnd()
        const [oid, type, size] = line.split(' ')
        if (type === 'missing' || size === undefined) {
          unreadable.push(line)
          continue
        }
        header = { oid, type, size: Number(size) }
      }
      if (queue.length < header.size + 1) break
      const data = queue.take(header.size)
      queue.take(1)
      visit(header.oid, data, header.size)
      header = null
    }
  }
  await writer
  const [code] = await exited
  if (code !== 0)
    throw new Error(`git cat-file --batch failed: ${Buffer.concat(stderr).toString()}`)
  // A partial or damaged clone can list a tree entry whose blob is absent; passing it unscanned
  // would let the audit pass history it never read.
  if (unreadable.length > 0) {
    throw new Error(
      `${unreadable.length} history object(s) could not be read (missing or unparseable), ` +
        `so their content was not scanned: ${unreadable.slice(0, 5).join(', ')}`,
    )
  }
}
