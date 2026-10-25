#!/usr/bin/env node
// Read-only tenant-access inventory. Tokenization keeps comments and quoted code out of checks.
import fs from 'node:fs'
import path from 'node:path'
import { createHash } from 'node:crypto'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { normaliseRecord } from './bs-security-findings.mjs'

/** @typedef {{path:string,text:string}} SourceFile */
/** @typedef {{value:string,start:number,end:number,line:number,quoted:boolean}} Token */
/** @typedef {{path:string,line:number,function?:string,reason?:string,reasonKind?:string,count?:number,symbol?:string,method?:string,kind?:string,authz?:string,entitlement?:string,class?:string}} Site */
/** @typedef {{schemaVersion:number,systemAccess:Array<{path:string,function:string,reason:string,count:number,review:string}>,rawOrganizationID:Array<{path:string,symbol:string,review:string}>,crossTenantExceptions:Array<{method:string,review:string}>}} Allowlist */

/** @param {string} text @returns {Token[]} */
function tokens(text) {
  const out = []
  const re =
    /\/\/[^\n]*|\/\*[\s\S]*?\*\/|"(?:\\[\s\S]|[^"\\])*"|`[^`]*`|'(?:\\[\s\S]|[^'\\])*'|[A-Za-z_][A-Za-z_0-9]*|\S/g
  let line = 1
  let previous = 0
  for (const m of text.matchAll(re)) {
    line += text.slice(previous, m.index).split('\n').length - 1
    previous = m.index
    if (m[0].startsWith('//') || m[0].startsWith('/*')) continue
    out.push({
      value: m[0],
      start: m.index,
      end: m.index + m[0].length,
      line,
      quoted: /^["`']/.test(m[0]),
    })
  }
  return out
}
/** @param {Token[]} ts @param {number} start */
function closing(ts, start) {
  const pairs = { '(': ')', '{': '}', '[': ']' }
  const end = pairs[ts[start]?.value]
  if (!end) throw new Error('missing opening delimiter')
  let depth = 0
  for (let i = start; i < ts.length; i++) {
    if (ts[i].value === ts[start].value) depth++
    if (ts[i].value === end && --depth === 0) return i
  }
  throw new Error('unclosed delimiter')
}
/** @param {Token[]} ts */
function functions(ts) {
  const out = []
  let depth = 0
  for (let i = 0; i < ts.length; i++) {
    if (ts[i].value === 'func' && depth === 0) {
      let at = i + 1
      let receiver = ''
      if (ts[at]?.value === '(') {
        const end = closing(ts, at)
        receiver =
          ts
            .slice(at + 1, end)
            .filter((t) => /^[A-Za-z_]\w*$/.test(t.value))
            .at(-1)?.value ?? ''
        at = end + 1
      }
      if (ts[at + 1]?.value !== '(') continue
      const name = ts[at].value
      const paramsStart = at + 1
      const paramsEnd = closing(ts, paramsStart)
      let body = paramsEnd + 1
      while (body < ts.length && ts[body].value !== '{' && ts[body].value !== 'func') body++
      if (ts[body]?.value !== '{') continue
      const end = closing(ts, body)
      out.push({
        name: receiver ? `${receiver}.${name}` : name,
        receiver,
        params: ts.slice(paramsStart + 1, paramsEnd),
        start: i,
        end,
        line: ts[i].line,
      })
      i = end
      continue
    }
    if (ts[i].value === '{') depth++
    if (ts[i].value === '}') depth--
  }
  return out
}
/** @param {string} value */
function literal(value) {
  if (value.startsWith('`')) return value.slice(1, -1).replace(/\r/g, '')
  try {
    return JSON.parse(value)
  } catch {
    return value.slice(1, -1)
  }
}
/** @param {SourceFile[]} files @returns {Site[]} */
export function scanSystemAccess(files) {
  const sites = new Map()
  for (const file of files) {
    const ts = tokens(file.text)
    const funcs = functions(ts)
    for (let i = 0; i < ts.length; i++) {
      if (ts[i].value !== 'SystemAccess' || ts[i + 1]?.value !== '(' || ts[i - 1]?.value === 'func')
        continue
      const end = closing(ts, i + 1)
      const args = ts.slice(i + 2, end)
      if (args.at(-1)?.value === ',') args.pop()
      const reasonKind = args.length === 1 && /^["`]/.test(args[0].value) ? 'literal' : 'expression'
      const reason =
        reasonKind === 'literal' ? literal(args[0].value) : args.map((t) => t.value).join(' ')
      const fn = funcs.find((f) => f.start < i && f.end > i)?.name ?? '<package>'
      const key = JSON.stringify([file.path, fn, reason])
      const prior = sites.get(key)
      if (prior) prior.count++
      else
        sites.set(key, {
          path: file.path,
          line: ts[i].line,
          function: fn,
          reason,
          reasonKind,
          count: 1,
        })
    }
  }
  return [...sites.values()]
}
/** @param {SourceFile[]} files */
export function scanRawOrganizationIDs(files) {
  /** @type {Site[]} */ const methods = []
  /** @type {Site[]} */ const bypasses = []
  let storeFiles = 0
  for (const file of files) {
    if (!file.path.startsWith('services/bosso/')) continue
    const ts = tokens(file.text)
    if (file.path.startsWith('services/bosso/internal/db/')) {
      if (file.path.includes('/sqlcgen/')) continue
      storeFiles++
      for (const fn of functions(ts)) {
        if (!fn.receiver || !/\.[A-Z]/.test(fn.name)) continue
        const params = fn.params.map((t) => t.value).join(' ')
        if (/\b\w+\s*\.\s*(?:OrgAccess|System)\b/.test(params)) continue
        if (
          /\b(?:orgIDs?|organizationIDs?|\w+OrganizationIDs?)\b(?:\s*,\s*\w+)*\s+(?:\[\s*\]\s*)?string\b/.test(
            params,
          )
        )
          methods.push({ path: file.path, line: fn.line, symbol: fn.name })
      }
      continue
    }
    // Imports are declarations, not arbitrary string references to generated queries.
    for (let i = 0; i < ts.length; i++) {
      if (ts[i].value !== 'import') continue
      let end = i + 2
      if (ts[i + 1]?.value === '(') end = closing(ts, i + 1)
      else if (!ts[i + 1]?.quoted) end = i + 3
      for (const t of ts.slice(i + 1, end))
        if (t.quoted && literal(t.value) === 'github.com/recurser/bosso/internal/db/sqlcgen')
          bypasses.push({ path: file.path, line: t.line, kind: 'sqlcgen-import' })
    }
    for (const t of ts)
      if (t.quoted && /^["`]/.test(t.value)) {
        const value = literal(t.value)
        if (
          /\borganization_id\b/i.test(value) &&
          /\b(?:select|insert\s+into|update|delete\s+from)\b/i.test(value) &&
          /\b(?:from|set|where|into|values)\b/i.test(value)
        )
          bypasses.push({ path: file.path, line: t.line, kind: 'raw-tenant-sql' })
      }
  }
  return { methods, bypasses, storeFiles }
}
/** @param {SourceFile} proto @param {SourceFile[]} tests */
export function scanDeclarations(proto, tests) {
  const ts = tokens(proto.text)
  const service = ts.findIndex(
    (t, i) => t.value === 'service' && ts[i + 1]?.value === 'OrchestratorService',
  )
  if (service < 0 || ts[service + 2]?.value !== '{') throw new Error('OrchestratorService missing')
  const serviceEnd = closing(ts, service + 2)
  /** @type {Site[]} */ const declarations = []
  for (let i = service + 3; i < serviceEnd; i++) {
    if (ts[i].value !== 'rpc') continue
    const method = ts[i + 1].value
    let end = i + 2
    while (end < serviceEnd && ![';', '{'].includes(ts[end].value)) end++
    const options = ts[end]?.value === '{' ? ts.slice(end + 1, closing(ts, end)) : []
    const text = options.map((t) => t.value).join(' ')
    declarations.push({
      path: proto.path,
      line: ts[i].line,
      method,
      authz: text.match(/bossanova\s*\.\s*v1\s*\.\s*authz_level\s*\)\s*=\s*(\w+)/)?.[1] ?? '',
      entitlement: text.match(/bossanova\s*\.\s*v1\s*\.\s*entitlement\s*\)\s*=\s*(\w+)/)?.[1] ?? '',
    })
    i = ts[end]?.value === '{' ? closing(ts, end) : end
  }
  const exceptionFiles = tests.filter((f) =>
    /\bvar\s+crossTenantExceptions\s*=/.test(
      tokens(f.text)
        .map((t) => t.value)
        .join(' '),
    ),
  )
  if (exceptionFiles.length !== 1)
    throw new Error('crossTenantExceptions inventory missing or ambiguous')
  const file = exceptionFiles[0]
  const ets = tokens(file.text)
  const start = ets.findIndex(
    (t, i) => t.value === 'crossTenantExceptions' && ets[i - 1]?.value === 'var',
  )
  let body = start + 1
  while (body < ets.length && ets[body].value !== '{') body++
  if (body === ets.length) throw new Error('crossTenantExceptions map missing')
  const end = closing(ets, body)
  /** @type {Site[]} */ const exceptions = []
  for (let i = body + 1; i < end; i++)
    if (ets[i].quoted && ets[i + 1]?.value === ':')
      exceptions.push({ path: file.path, line: ets[i].line, method: literal(ets[i].value) })
  const suitePresent = tests.some((f) =>
    functions(tokens(f.text)).some(
      (fn) => fn.name === 'TestCrossTenantDenial_EveryOrchestratorMethod',
    ),
  )
  return { declarations, exceptions, suitePresent, suitePath: file.path }
}
/** @param {SourceFile[]} files @param {SourceFile} doc */
export function scanUpstreamCommands(files, doc) {
  /** @type {Site[]} */ const arms = []
  for (const file of files.filter((f) => f.path.startsWith('services/bossd/'))) {
    const ts = tokens(file.text)
    for (let i = 0; i < ts.length; i++)
      if (ts[i].value === 'case') {
        let end = i + 1
        while (end < ts.length && ts[end].value !== ':') end++
        for (let j = i + 1; j < end; j++)
          if (
            ts[j].value === '*' &&
            ts[j + 2]?.value === '.' &&
            ts[j + 3]?.value.startsWith('OrchestratorCommand_')
          )
            arms.push({
              path: file.path,
              line: ts[i].line,
              kind: ts[j + 3].value.slice('OrchestratorCommand_'.length),
            })
      }
  }
  const rows = []
  let inInventory = false
  let command = -1
  let classification = -1
  for (const line of doc.text.split('\n')) {
    if (/^## Command inventory\s*$/.test(line)) {
      inInventory = true
      continue
    }
    if (inInventory && /^## /.test(line)) break
    if (!inInventory || !line.startsWith('|')) continue
    const cells = line
      .split('|')
      .slice(1, -1)
      .map((c) => c.trim().replace(/`/g, ''))
    if (cells.includes('Command') && cells.includes('Class')) {
      command = cells.indexOf('Command')
      classification = cells.indexOf('Class')
      continue
    }
    if (command < 0 || /^[\s:-]+$/.test(cells[command] ?? '') || !cells[command]) continue
    const kind = cells[command]
      .split('_')
      .map((p) => p.charAt(0).toUpperCase() + p.slice(1))
      .join('')
    rows.push({ kind, class: cells[classification] ?? '' })
  }
  const byKind = new Map(rows.map((r) => [r.kind, r.class]))
  return {
    arms: arms.map((a) => ({ ...a, class: byKind.get(a.kind) ?? '' })),
    rows,
    staleCommandRows: rows.filter((r) => !arms.some((a) => a.kind === r.kind)).length,
  }
}

/** @param {unknown} data @returns {Allowlist} */
export function validateAllowlist(data) {
  const fail = () => {
    throw new Error('invalid tenant allowlist')
  }
  if (!data || typeof data !== 'object' || Array.isArray(data)) return fail()
  const obj = /** @type {Record<string,unknown>} */ (data)
  const keys = ['schemaVersion', 'systemAccess', 'rawOrganizationID', 'crossTenantExceptions']
  if (Object.keys(obj).some((k) => !keys.includes(k)) || obj.schemaVersion !== 1) return fail()
  const shapes = {
    systemAccess: ['path', 'function', 'reason', 'count', 'review'],
    rawOrganizationID: ['path', 'symbol', 'review'],
    crossTenantExceptions: ['method', 'review'],
  }
  for (const [category, fields] of Object.entries(shapes)) {
    if (!Array.isArray(obj[category])) return fail()
    const seen = new Set()
    for (const entry of obj[category]) {
      if (
        !entry ||
        typeof entry !== 'object' ||
        Array.isArray(entry) ||
        Object.keys(entry).some((k) => !fields.includes(k))
      )
        return fail()
      if (
        fields.some((k) =>
          k === 'count'
            ? !Number.isSafeInteger(entry[k]) || entry[k] < 1
            : typeof entry[k] !== 'string' || !entry[k].trim(),
        )
      )
        return fail()
      const key = allowKey(category, entry)
      if (seen.has(key)) return fail()
      seen.add(key)
    }
  }
  return /** @type {Allowlist} */ (data)
}
/** @param {string} category @param {Site} site */
function allowKey(category, site) {
  return JSON.stringify(
    category === 'systemAccess'
      ? [site.path, site.function, site.reason]
      : category === 'rawOrganizationID'
        ? [site.path, site.symbol]
        : [site.method],
  )
}
/** @param {{systemAccess:Site[],rawOrganizationID:Site[],crossTenantExceptions:Site[]}} inventory @param {Allowlist} allowlist */
export function applyAllowlist(inventory, allowlist) {
  /** @type {Site[]} */ const findings = []
  let staleAllowlistEntries = 0
  for (const category of ['systemAccess', 'rawOrganizationID', 'crossTenantExceptions']) {
    const allowed = new Map(allowlist[category].map((e) => [allowKey(category, e), e]))
    for (const site of inventory[category]) {
      const entry = allowed.get(allowKey(category, site))
      if (!entry || (category === 'systemAccess' && site.count > entry.count))
        findings.push({ ...site, kind: category })
    }
    for (const entry of allowed.values())
      if (!inventory[category].some((s) => allowKey(category, s) === allowKey(category, entry)))
        staleAllowlistEntries++
  }
  return { findings, staleAllowlistEntries }
}
/** @param {Site[]} sites */
export function buildRecords(sites) {
  return sites.map((site) => {
    // A compact identity token precedes prose: the core fingerprints only eight title tokens.
    const identity = createHash('sha256')
      .update(
        JSON.stringify([
          site.path,
          site.function,
          site.reason,
          site.symbol,
          site.method,
          site.kind,
        ]),
      )
      .digest('hex')
      .slice(0, 12)
    const title =
      site.kind === 'systemAccess'
        ? `${site.function} grant-${identity}: ${site.reason}`
        : `${site.symbol ?? site.method ?? site.kind} tenant access ${identity}`
    return normaliseRecord(
      {
        source: 'tenant-scan',
        lens: 'tenant-access',
        lensFamily: 'tenant-access',
        title,
        severity: [
          'rawOrganizationID',
          'sqlcgen-import',
          'raw-tenant-sql',
          'missing-authz',
          'missing-entitlement',
          'missing-suite',
        ].includes(site.kind)
          ? 'high'
          : 'medium',
        confidence: 'confirmed-by-reading',
        locations: [{ path: site.path, line: site.line }],
        evidence: site,
        defect: `Unreviewed tenant access: ${site.kind}`,
        impact: 'Tenant isolation or authorization may be bypassed.',
        fix: 'Review the call or declaration and restore the tenant-access contract.',
      },
      { source: 'tenant-scan' },
    )
  })
}
/** @param {string} root @param {string} [relative] @returns {SourceFile[]} */
function readGoFiles(root, relative = 'services') {
  const dir = path.join(root, relative)
  if (!fs.existsSync(dir)) return []
  return fs
    .readdirSync(dir, { withFileTypes: true })
    .sort((a, b) => a.name.localeCompare(b.name))
    .flatMap((e) => {
      const rel = `${relative}/${e.name}`
      if (e.isDirectory())
        return ['gen', 'vendor', 'node_modules', '.git'].includes(e.name)
          ? []
          : readGoFiles(root, rel)
      if (!e.isFile() || !e.name.endsWith('.go')) return []
      const text = fs.readFileSync(path.join(root, rel), 'utf8')
      return /^\/\/ Code generated .* DO NOT EDIT\./m.test(text) ? [] : [{ path: rel, text }]
    })
}
/** @param {{repo:string,allowlist:Allowlist}} options */
export function scanRepository({ repo, allowlist }) {
  allowlist = validateAllowlist(allowlist)
  const all = readGoFiles(repo)
  const files = all.filter((f) => !f.path.endsWith('_test.go'))
  const systemAccess = scanSystemAccess(files)
  const raw = scanRawOrganizationIDs(files)
  const protoPath = 'proto/bossanova/v1/orchestrator.proto'
  const declarations = scanDeclarations(
    { path: protoPath, text: fs.readFileSync(path.join(repo, protoPath), 'utf8') },
    all.filter((f) => f.path.startsWith('services/bosso/') && f.path.endsWith('_test.go')),
  )
  const docPath = 'docs/security/daemon-commands.md'
  const commands = scanUpstreamCommands(files, {
    path: docPath,
    text: fs.readFileSync(path.join(repo, docPath), 'utf8'),
  })
  if (
    !systemAccess.length ||
    !raw.storeFiles ||
    !declarations.declarations.length ||
    !commands.arms.length ||
    !commands.rows.length
  )
    throw new Error('empty required tenant inventory')
  const inventory = {
    systemAccess,
    rawOrganizationID: raw.methods,
    crossTenantExceptions: declarations.exceptions,
    declarations: declarations.declarations,
    upstreamCommands: commands.arms,
  }
  const allowed = applyAllowlist(inventory, allowlist)
  const findings = [...allowed.findings, ...raw.bypasses]
  for (const rpc of declarations.declarations) {
    if (!rpc.authz || rpc.authz === 'AUTHZ_LEVEL_UNSPECIFIED')
      findings.push({ ...rpc, kind: 'missing-authz' })
    if (!rpc.entitlement || rpc.entitlement === 'ENTITLEMENT_UNSPECIFIED')
      findings.push({ ...rpc, kind: 'missing-entitlement' })
  }
  if (!declarations.suitePresent)
    findings.push({ path: declarations.suitePath, line: 1, kind: 'missing-suite' })
  for (const arm of commands.arms)
    if (!['Justified', 'Needs daemon check', 'Gap'].includes(arm.class))
      findings.push({ ...arm, kind: 'unclassified-command', symbol: arm.kind })
  const records = buildRecords(findings)
  return {
    schemaVersion: 1,
    counts: {
      systemAccessCalls: systemAccess.reduce((n, s) => n + s.count, 0),
      systemAccessKeys: systemAccess.length,
      storeFiles: raw.storeFiles,
      rawOrganizationIDMethods: raw.methods.length,
      rpcs: declarations.declarations.length,
      crossTenantExceptions: declarations.exceptions.length,
      dispatcherArms: commands.arms.length,
      commandRows: commands.rows.length,
      staleCommandRows: commands.staleCommandRows,
      staleAllowlistEntries: allowed.staleAllowlistEntries,
      records: records.length,
    },
    inventory,
    records,
  }
}

export function main(args = process.argv.slice(2)) {
  try {
    const { values, positionals } = parseArgs({
      args,
      strict: true,
      allowPositionals: true,
      options: { repo: { type: 'string' }, out: { type: 'string' }, allowlist: { type: 'string' } },
    })
    if (
      positionals.length !== 1 ||
      positionals[0] !== 'scan' ||
      !values.repo ||
      !values.out ||
      !values.allowlist
    )
      throw new Error('invalid arguments')
    const allowlist = validateAllowlist(JSON.parse(fs.readFileSync(values.allowlist, 'utf8')))
    const result = scanRepository({ repo: values.repo, allowlist })
    fs.mkdirSync(path.dirname(path.resolve(values.out)), { recursive: true })
    fs.writeFileSync(values.out, JSON.stringify(result, null, 2) + '\n')
    process.stdout.write(JSON.stringify(result.counts) + '\n')
    return 0
  } catch {
    process.stderr.write(
      'tenant-scan: invalid arguments, allowlist, repository or output; scan incomplete\n',
    )
    return 2
  }
}
if (isMainModule(import.meta.url)) process.exitCode = main()
