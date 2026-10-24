export const LARGEST_BLOBS = 20
const LIST_CAP = 50

function short(sha) {
  return sha ? sha.slice(0, 12) : '(none)'
}

// ---------------------------------------------------------------------------------------------
// report

function cell(value) {
  return String(value ?? '')
    .replace(/\|/g, '\\|')
    .replace(/\n/g, ' ')
}

function table(headers, rows) {
  if (rows.length === 0) return '_None._\n'
  const lines = [`| ${headers.join(' | ')} |`, `| ${headers.map(() => '---').join(' | ')} |`]
  for (const row of rows) lines.push(`| ${row.map(cell).join(' | ')} |`)
  return lines.join('\n') + '\n'
}

function code(value) {
  return value ? `\`${String(value).replace(/`/g, "'")}\`` : ''
}

export function renderReport(r) {
  const out = []
  const verdict =
    r.hardFindings.length === 0
      ? 'PASS — no hard findings'
      : `FAIL — ${r.hardFindings.length} hard finding(s)`
  out.push('# Public history audit\n')
  out.push(`- Repository: ${code(r.repo)}`)
  out.push(`- Spec: ${code(r.spec)}`)
  out.push(`- Verdict: **${verdict}**\n`)

  out.push('## Hard checks\n')
  const byCheck = (check) => r.hardFindings.filter((f) => f.check === check)
  out.push(`### Forbidden paths (paths.txt) — ${byCheck('forbidden-path').length}\n`)
  out.push(
    table(
      ['Path', 'Rule', 'Oldest commit', 'Where'],
      byCheck('forbidden-path').map((f) => [
        code(f.path),
        code(f.rule),
        code(short(f.commit)),
        f.where,
      ]),
    ),
  )
  out.push(
    `### Replacement left-hand sides (replacements.txt) — ${byCheck('replacement-lhs').length}\n`,
  )
  out.push(
    table(
      ['Rule', 'Where', 'Commit', 'Path or ref'],
      byCheck('replacement-lhs').map((f) => [
        code(f.rule),
        f.where,
        code(f.commit ? short(f.commit) : ''),
        code(f.path),
      ]),
    ),
  )

  out.push('## Report-only (human triage)\n')
  out.push(`### Secret scan — ${r.secretHits.length} hit(s), ${r.allowlisted.length} allowlisted\n`)
  out.push('Samples are redacted. Rotate anything live before publishing.\n')
  out.push(
    table(
      ['Family', 'Commit', 'Path', 'Sample', 'Occurrences'],
      r.secretHits.map((h) => [
        h.family,
        code(short(h.commit)),
        code(h.path),
        code(h.sample),
        h.count,
      ]),
    ),
  )
  out.push('#### Allowlisted (secret-allowlist.txt)\n')
  out.push(
    table(
      ['Family', 'Commit', 'Path', 'Sample', 'Allowlist entry'],
      r.allowlisted.map((h) => [
        h.family,
        code(short(h.commit)),
        code(h.path),
        code(h.sample),
        code(h.allowedBy),
      ]),
    ),
  )

  out.push('### Email addresses by domain\n')
  out.push(
    table(
      ['Domain', 'Addresses', 'Occurrences', 'Examples'],
      [...r.emails]
        .map(([domain, addrs]) => {
          const list = [...addrs.keys()].sort()
          const total = [...addrs.values()].reduce((a, b) => a + b, 0)
          const shown = list.slice(0, LIST_CAP).join(', ')
          const more = list.length > LIST_CAP ? ` … and ${list.length - LIST_CAP} more` : ''
          return [domain, list.length, total, shown + more]
        })
        .sort((a, b) => b[2] - a[2]),
    ),
  )
  out.push('### Home-directory paths by user\n')
  out.push(
    table(
      ['Home', 'Occurrences', 'First seen (commit)', 'Path'],
      [...r.homes]
        .sort((a, b) => b[1].count - a[1].count)
        .map(([home, e]) => [code(home), e.count, code(short(e.commit)), code(e.path)]),
    ),
  )
  out.push('### Cluster-internal hostnames\n')
  out.push(
    table(
      ['Hostname', 'Occurrences', 'Commit', 'Path'],
      [...r.hostnames]
        .sort((a, b) => b[1].count - a[1].count)
        .map(([host, e]) => [code(host), e.count, code(short(e.commit)), code(e.path)]),
    ),
  )

  out.push('## Size and shape\n')
  out.push(`- Commits: ${r.commitCount}`)
  out.push(`- Objects: ${r.objectCount}`)
  out.push(`- Packed size: ${(r.packedKiB / 1024).toFixed(2)} MiB\n`)
  out.push(`### Kept refs — ${r.refs.length}\n`)
  out.push(
    table(
      ['Ref', 'Object'],
      r.refs.map((line) => line.split(' ')).map(([ref, oid]) => [code(ref), code(short(oid))]),
    ),
  )
  out.push(`### ${LARGEST_BLOBS} largest blobs\n`)
  out.push(
    table(
      ['Size (bytes)', 'Path', 'Commit', 'Blob'],
      r.largestBlobs.map((b) => [b.size, code(b.path), code(short(b.commit)), code(short(b.oid))]),
    ),
  )
  out.push(`### Tip files changed by replacements — ${r.tipChanged.length}\n`)
  out.push(
    'Files in the tip (HEAD) that contain a replacement marker, so the filter changed them.\n',
  )
  out.push(
    table(
      ['Path', 'Rules'],
      r.tipChanged.map((t) => [code(t.path), t.rules.map(code).join(', ')]),
    ),
  )
  out.push(`### History-only paths — ${r.historyOnly.length}\n`)
  out.push('Paths some commit holds that the tip (HEAD) does not.\n')
  out.push('<details><summary>Show paths</summary>\n')
  out.push(
    r.historyOnly.length === 0 ? '_None._' : r.historyOnly.map((p) => `- ${code(p)}`).join('\n'),
  )
  out.push('\n</details>\n')
  return out.join('\n') + '\n'
}
