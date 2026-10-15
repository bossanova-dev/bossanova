#!/usr/bin/env node
// Published git history scanner. Matched bytes never leave scanBlobText.
import { spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import { createInterface } from 'node:readline'
import { parseArgs } from 'node:util'
import { once } from 'node:events'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { SHAPES } from '../skills-toolbox/plan-secret-scan.mjs'
import { normaliseRecord } from './bs-security-findings.mjs'

const DEFAULT_MAX_BYTES = 5 * 1024 * 1024
export const EXTRA_SHAPES = [
  { kind: 'aws-temporary-access-key-id', re: /\bASIA[0-9A-Z]{16}\b/g },
  { kind: 'google-api-key', re: /\bAIza[A-Za-z0-9_-]{35}\b/g },
  { kind: 'google-oauth-client-secret', re: /\bGOCSPX-[A-Za-z0-9_-]{20,255}/g },
  { kind: 'stripe-webhook-secret', re: /\bwhsec_[A-Za-z0-9]{16,255}\b/g },
  { kind: 'sentry-user-token', re: /\bsntryu_[A-Za-z0-9]{20,255}\b/g },
  { kind: 'terraform-cloud-token', re: /\b[A-Za-z0-9]{8,128}\.atlasv1\.[A-Za-z0-9_-]{16,255}\b/g },
  { kind: 'kubeconfig-client-key', re: /client-key-data:\s{0,8}([A-Za-z0-9+/=]{64,65536})/g },
  {
    kind: 'private-key',
    re: /-----BEGIN (?:[A-Z0-9]{1,20} ){0,3}PRIVATE KEY-----\r?\n((?:[A-Za-z0-9+/=]{1,128}\r?\n){1,512}?)-----END (?:[A-Z0-9]{1,20} ){0,3}PRIVATE KEY-----/g,
    accept: (m) => m[1].replace(/\s/g, '').length >= 64,
  },
  {
    kind: 'service-account-private-key',
    re: /"private_key"\s{0,8}:\s{0,8}"((?:[^"\\]|\\.){64,65536})"/g,
    accept: (m, text) =>
      /"type"\s{0,8}:\s{0,8}"service_account"/.test(text) && m[1].includes('PRIVATE KEY'),
  },
]

const hashValue = (value) => createHash('sha256').update(value).digest('hex').slice(0, 12)

/** Only kind, hash and line escape this function; captures are credential bytes. */
export function scanBlobText(text) {
  const hits = []
  for (const shape of [...SHAPES, ...EXTRA_SHAPES]) {
    const re = new RegExp(shape.re.source, shape.re.flags)
    for (const match of text.matchAll(re)) {
      if (shape.accept && !shape.accept(match, text)) continue
      // The shared URL shapes' first capture is metadata, their second is the credential.
      const value =
        shape.kind === 'url-password' || shape.kind === 'signed-url-query'
          ? match[2]
          : (match[1] ?? match[0])
      hits.push({
        kind: shape.kind,
        valueHash: hashValue(value),
        line: text.slice(0, match.index).split('\n').length,
      })
    }
  }
  return hits
}

// Path metadata can itself contain a credential. Do not persist those bytes either.
const safeSamplePath = (p) => (scanBlobText(p).length ? 'redacted-path' : p)

export function groupHits(hits) {
  const groups = new Map()
  for (const hit of hits) {
    const key = `${hit.kind}:${hit.valueHash}`
    const prior = groups.get(key)
    if (prior) prior.occurrences++
    else groups.set(key, { ...hit, samplePath: safeSamplePath(hit.samplePath), occurrences: 1 })
  }
  return [...groups.values()].sort(
    (a, b) => a.kind.localeCompare(b.kind) || a.valueHash.localeCompare(b.valueHash),
  )
}

export function toFindingRecords(groups) {
  return groups.map((hit) =>
    normaliseRecord(
      {
        source: 'history-scan',
        lens: 'secret-history',
        lensFamily: 'secret-history',
        severity: 'high',
        confidence: 'likely',
        // The findings core discards purely numeric title tokens; retain this hash in its stem.
        title: `${hit.kind} credential shape ${/^\d+$/.test(hit.valueHash) ? 'h' : ''}${hit.valueHash} in git history`,
        locations: [
          { path: 'git-history', line: null },
          { path: hit.samplePath, line: hit.line },
        ],
        locationStatus: 'location-verified',
        evidence: {
          kind: hit.kind,
          valueHash: hit.valueHash,
          blob: hit.blob,
          samplePath: hit.samplePath,
          occurrences: hit.occurrences,
        },
        defect: 'A credential-shaped value is reachable in published git history.',
        impact: 'If live, the credential must be treated as exposed.',
        fix: 'Rotate live credentials; allowlist the hash only when the value is a justified fixture.',
      },
      { source: 'history-scan', trustLocationStatus: true },
    ),
  )
}

export function applyAllowlist(groups, entries) {
  const allowed = new Set(entries.map((e) => `${e.kind}:${e.valueHash}`))
  const kept = groups.filter((h) => !allowed.has(`${h.kind}:${h.valueHash}`))
  return { groups: kept, suppressed: groups.length - kept.length }
}

function readAllowlist(file) {
  if (!file) return []
  let text
  try {
    text = fs.readFileSync(file, 'utf8')
  } catch (err) {
    if (err.code === 'ENOENT') {
      process.stderr.write('secret-scan: allowlist missing; using an empty allowlist\n')
      return []
    }
    throw new Error('secret-scan: cannot read allowlist')
  }
  let data
  try {
    data = JSON.parse(text)
  } catch {
    throw new Error('secret-scan: malformed allowlist')
  }
  if (
    !data ||
    !Array.isArray(data.entries) ||
    data.entries.some(
      (e) =>
        !e ||
        typeof e.valueHash !== 'string' ||
        !/^[a-f0-9]{12}$/.test(e.valueHash) ||
        typeof e.kind !== 'string' ||
        !e.kind ||
        typeof e.samplePath !== 'string' ||
        typeof e.reason !== 'string',
    )
  ) {
    throw new Error('secret-scan: malformed allowlist')
  }
  return data.entries
}

/** Discard Git stderr: error messages may quote filenames or object contents. */
function gitProcess(repo, args) {
  const child = spawn('git', ['-C', repo, ...args], { stdio: ['pipe', 'pipe', 'ignore'] })
  const done = new Promise((resolve, reject) => {
    child.once('error', () => reject(new Error('secret-scan: git unavailable')))
    child.once('close', (code) =>
      code === 0 ? resolve() : reject(new Error('secret-scan: git operation failed')),
    )
  })
  // Attach immediately so a failure cannot become an unhandled rejection before a reader ends.
  done.catch(() => {})
  child.stdin.on('error', () => {})
  return { child, done }
}

async function publishedObjects(repo) {
  const { child, done } = gitProcess(repo, ['rev-list', '--objects', '--remotes=origin', '--tags'])
  child.stdin.end()
  const objects = new Map()
  for await (const line of createInterface({ input: child.stdout, crlfDelay: Infinity })) {
    const match = line.match(/^([a-f0-9]{40,64})(?: (.*))?$/)
    if (!match) throw new Error('secret-scan: invalid git object metadata')
    objects.set(match[1], match[2] ?? 'git-history')
  }
  await done
  return objects
}

async function objectMetadata(repo, refs, paths) {
  const args =
    refs === 'all-objects'
      ? ['cat-file', '--batch-all-objects', '--batch-check']
      : ['cat-file', '--batch-check']
  const { child, done } = gitProcess(repo, args)
  const write = async () => {
    if (paths)
      for (const oid of paths.keys()) {
        if (!child.stdin.write(oid + '\n')) await once(child.stdin, 'drain')
      }
    child.stdin.end()
  }
  const writing = write()
  writing.catch(() => {})
  const objects = []
  try {
    for await (const line of createInterface({ input: child.stdout, crlfDelay: Infinity })) {
      const m = line.match(/^([a-f0-9]{40,64}) (blob|commit|tree|tag) (\d+)$/)
      if (!m) throw new Error('secret-scan: invalid git object metadata')
      if (m[2] === 'blob' || m[2] === 'commit')
        objects.push({
          oid: m[1],
          type: m[2],
          size: Number(m[3]),
          samplePath: paths?.get(m[1]) ?? 'git-history',
        })
    }
    await writing
    await done
    return objects.sort((a, b) => a.oid.localeCompare(b.oid))
  } finally {
    child.kill()
  }
}

/** Read byte-framed cat-file output without decoding payloads or retaining the history. */
function batchReader(stream) {
  const iterator = stream[Symbol.asyncIterator]()
  let pending = Buffer.alloc(0)
  async function fill() {
    const next = await iterator.next()
    if (next.done) throw new Error('secret-scan: incomplete git object stream')
    pending = pending.length ? Buffer.concat([pending, next.value]) : next.value
  }
  return {
    async line() {
      while (!pending.includes(10)) await fill()
      const end = pending.indexOf(10)
      const line = pending.subarray(0, end).toString('ascii')
      pending = pending.subarray(end + 1)
      return line
    },
    async bytes(size) {
      const parts = []
      let remaining = size
      while (remaining) {
        if (!pending.length) await fill()
        const n = Math.min(remaining, pending.length)
        parts.push(pending.subarray(0, n))
        pending = pending.subarray(n)
        remaining -= n
      }
      return Buffer.concat(parts, size)
    },
  }
}

export async function scanRepository({ repo, refs = 'origin', maxBlobBytes = DEFAULT_MAX_BYTES }) {
  const paths = refs === 'origin' ? await publishedObjects(repo) : null
  const objects = await objectMetadata(repo, refs, paths)
  const { child, done } = gitProcess(repo, ['cat-file', '--batch'])
  const reader = batchReader(child.stdout)
  const hits = []
  let blobsScanned = 0
  let commitsScanned = 0
  try {
    for (const object of objects) {
      // Bound commits too: maliciously large messages must not defeat the streaming memory limit.
      if (object.size > maxBlobBytes) continue
      child.stdin.write(object.oid + '\n')
      const header = await reader.line()
      if (header !== `${object.oid} ${object.type} ${object.size}`)
        throw new Error('secret-scan: unexpected git object framing')
      const bytes = await reader.bytes(object.size)
      if ((await reader.bytes(1))[0] !== 10)
        throw new Error('secret-scan: unexpected git object framing')
      if (bytes.subarray(0, 8192).includes(0)) continue
      let text = bytes.toString('utf8')
      const samplePath = object.type === 'commit' ? 'commit-message' : object.samplePath
      if (object.type === 'commit') {
        // gate-region-ok: Git commit protocol separates metadata from message bytes, not prose sections.
        text = text.slice(text.indexOf('\n\n') + 2)
        commitsScanned++
      } else blobsScanned++
      for (const hit of scanBlobText(text)) hits.push({ ...hit, blob: object.oid, samplePath })
    }
    child.stdin.end()
    // Consume EOF so the child can exit even if it has buffered output.
    await done
  } finally {
    child.kill()
  }
  return { groups: groupHits(hits), blobsScanned, commitsScanned, hits: hits.length }
}

export async function main(args = process.argv.slice(2)) {
  try {
    const { values, positionals } = parseArgs({
      args,
      strict: true,
      allowPositionals: true,
      options: {
        repo: { type: 'string' },
        out: { type: 'string' },
        allowlist: { type: 'string' },
        refs: { type: 'string', default: 'origin' },
        'max-blob-bytes': { type: 'string' },
      },
    })
    const command = positionals[0]
    const maxBlobBytes =
      values['max-blob-bytes'] === undefined ? DEFAULT_MAX_BYTES : Number(values['max-blob-bytes'])
    if (
      positionals.length !== 1 ||
      !['scan', 'baseline'].includes(command) ||
      !values.repo ||
      !values.out ||
      !['origin', 'all-objects'].includes(values.refs) ||
      !Number.isSafeInteger(maxBlobBytes) ||
      maxBlobBytes <= 0 ||
      (command === 'baseline' &&
        (values.allowlist !== undefined || values['max-blob-bytes'] !== undefined))
    ) {
      throw new Error('secret-scan: invalid arguments')
    }
    const entries = command === 'scan' ? readAllowlist(values.allowlist) : []
    const result = await scanRepository({ repo: values.repo, refs: values.refs, maxBlobBytes })
    const { groups, suppressed } = applyAllowlist(result.groups, entries)
    const output =
      command === 'baseline'
        ? {
            entries: groups.map((h) => ({
              kind: h.kind,
              valueHash: h.valueHash,
              samplePath: h.samplePath,
              reason: '',
            })),
          }
        : toFindingRecords(groups)
    fs.mkdirSync(path.dirname(path.resolve(values.out)), { recursive: true })
    fs.writeFileSync(values.out, JSON.stringify(output, null, 2) + '\n')
    process.stdout.write(
      JSON.stringify({
        blobsScanned: result.blobsScanned,
        commitsScanned: result.commitsScanned,
        hits: result.hits,
        suppressed,
        records: groups.length,
      }) + '\n',
    )
    return 0
  } catch {
    // Never forward an exception: parseArgs, fs and Git errors can contain secret-bearing input.
    process.stderr.write(
      'secret-scan: invalid arguments, allowlist, repository or output; scan not completed\n',
    )
    return 2
  }
}

if (isMainModule(import.meta.url)) process.exitCode = await main()
