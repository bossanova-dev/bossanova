import { execFileSync } from 'node:child_process'
import { existsSync, lstatSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import ts from 'typescript'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { BINARY_EXT, trackedFiles } from '../dev/tree-scan-lib.mjs'
import { rewritePathTokens } from '../layout/move.mjs'

const SRC = 'services/web/src/'
const LEDGER = 'scripts/codemods/web-moves.json'
const git = (root, args) =>
  execFileSync('git', args, { cwd: root, encoding: 'utf8', maxBuffer: 64 << 20 })
const generated = (file) => file.startsWith(`${SRC}gen/`)

/** Find module strings through syntax, including Vitest calls and import types. */
function moduleLiterals(file, source) {
  const ast = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  /** @type {import('typescript').StringLiteralLike[]} */
  const literals = []
  function visit(node) {
    let literal
    if (ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) literal = node.moduleSpecifier
    if (ts.isImportTypeNode(node) && ts.isLiteralTypeNode(node.argument))
      literal = node.argument.literal
    if (ts.isCallExpression(node)) {
      const expr = node.expression
      if (
        expr.kind === ts.SyntaxKind.ImportKeyword ||
        (ts.isPropertyAccessExpression(expr) &&
          ts.isIdentifier(expr.expression) &&
          expr.expression.text === 'vi' &&
          ['mock', 'doMock', 'importActual', 'importMock'].includes(expr.name.text))
      )
        literal = node.arguments[0]
    }
    if (literal && ts.isStringLiteralLike(literal)) literals.push(literal)
    ts.forEachChild(node, visit)
  }
  visit(ast)
  return { ast, literals }
}

function localTarget(from, specifier) {
  if (specifier.startsWith('~/')) return SRC + specifier.slice(2)
  if (specifier.startsWith('.'))
    return path.posix.normalize(path.posix.join(path.posix.dirname(from), specifier))
  return null
}

function resolveTarget(target, files) {
  return [
    target,
    ...(/\.[cm]?jsx?$/.test(target)
      ? [
          target
            .replace(/\.js$/, '.ts')
            .replace(/\.jsx$/, '.tsx')
            .replace(/\.mjs$/, '.mts')
            .replace(/\.cjs$/, '.cts'),
        ]
      : []),
    ...['.ts', '.tsx', '.js', '.jsx', '.mts', '.cts', '.json'].map((ext) => target + ext),
    ...['.ts', '.tsx', '.js', '.jsx'].map((ext) => `${target}/index${ext}`),
  ].find((candidate) => files.has(candidate))
}

function rewriteModules(source, from, to, moves, before, after) {
  const { ast, literals } = moduleLiterals(from, source)
  const edits = []
  for (const literal of literals) {
    const [, specifier, suffix] = literal.text.match(/^([^?#]*)(.*)$/s)
    const target = localTarget(from, specifier)
    if (target === null) continue
    const resolved = resolveTarget(target, before)
    const fail = () => {
      const { line } = ast.getLineAndCharacterOfPosition(literal.getStart(ast))
      throw new Error(`${from}:${line + 1}: unresolved module specifier ${literal.text}`)
    }
    if (!resolved) fail()
    const moved = moves.get(resolved) ?? resolved
    if (!after.has(moved)) fail()
    // An unchanged importer and target need no normalization.
    if (from === to && moved === resolved) continue
    const extension = path.posix.extname(specifier)
    let outputTarget = moved
    if (/\.[cm]?jsx?$/.test(extension) && resolved !== target)
      outputTarget = moved.replace(/\.[cm]?[jt]sx?$/, extension)
    if (!extension && /\.[cm]?[jt]sx?$/.test(moved))
      outputTarget = moved.replace(/\.[cm]?[jt]sx?$/, '')
    if (!extension && target !== resolved && /\/index\.[cm]?[jt]sx?$/.test(resolved))
      outputTarget = path.posix.dirname(outputTarget)
    let output
    if (path.posix.dirname(to) === path.posix.dirname(outputTarget))
      output = `./${path.posix.basename(outputTarget)}`
    else if (outputTarget.startsWith(SRC)) output = `~/${outputTarget.slice(SRC.length)}`
    else output = path.posix.relative(path.posix.dirname(to), outputTarget)
    if (resolveTarget(localTarget(to, output), after) !== moved) fail()
    if (output + suffix !== literal.text)
      edits.push({ start: literal.getStart(ast) + 1, end: literal.end - 1, value: output + suffix })
  }
  for (const edit of edits.sort((a, b) => b.start - a.start))
    source = source.slice(0, edit.start) + edit.value + source.slice(edit.end)
  return source
}

function expandMoves(root, requested, files) {
  if (!Array.isArray(requested)) throw new Error('Move map must be a list of [from, to] pairs')
  const moves = new Map()
  const add = (pair) => {
    if (!Array.isArray(pair) || pair.length !== 2) throw new Error('Expected a [from, to] pair')
    const [from, to] = pair
    for (const name of pair) {
      if (
        typeof name !== 'string' ||
        !name.startsWith(SRC) ||
        generated(name) ||
        name.includes('\\') ||
        path.posix.normalize(name) !== name
      )
        throw new Error(`Invalid move path: ${name}`)
    }
    if (moves.get(from) === to) return
    if (!files.has(from) || !lstatSync(path.join(root, from)).isFile())
      throw new Error(`Source is not a tracked regular file: ${from}`)
    if (
      from === to ||
      moves.has(from) ||
      existsSync(path.join(root, to)) ||
      [...moves.values()].includes(to)
    )
      throw new Error(`Conflicting move: ${from} -> ${to}`)
    moves.set(from, to)
  }
  for (const pair of requested) add(pair)
  for (const [from, to] of [...moves]) {
    if (!/\.(ts|tsx)$/.test(from) || /\.test\.[^.]+$/.test(from)) continue
    for (const ext of ['ts', 'tsx']) {
      const companion = from.replace(/\.(ts|tsx)$/, `.test.${ext}`)
      if (files.has(companion) && !moves.has(companion))
        add([companion, to.replace(/\.(ts|tsx)$/, `.test.${ext}`)])
    }
  }
  return moves
}

function sortedBaseline(file, body) {
  if (!file.endsWith('-baseline.json')) return body
  const parsed = JSON.parse(body)
  if (Array.isArray(parsed) && parsed.every((entry) => typeof entry === 'string'))
    return JSON.stringify(parsed.sort(), null, 2) + '\n'
  if (
    parsed &&
    typeof parsed === 'object' &&
    !Array.isArray(parsed) &&
    Object.keys(parsed).every((key) => key.includes('/'))
  )
    return (
      JSON.stringify(
        Object.fromEntries(Object.entries(parsed).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))),
        null,
        2,
      ) + '\n'
    )
  return body
}

/** Compute the complete edit set before any writes or git mv. */
export function planMoves({ root = process.cwd(), moves: requested }) {
  root = path.resolve(root)
  const files = trackedFiles(root)
  const before = new Set(files)
  const moves = expandMoves(root, requested, before)
  const after = new Set(files.map((file) => moves.get(file) ?? file))
  const ledgerFile = path.join(root, LEDGER)
  const ledger = existsSync(ledgerFile) ? JSON.parse(readFileSync(ledgerFile, 'utf8')) : []
  if (
    !Array.isArray(ledger) ||
    !ledger.every(
      (pair) =>
        Array.isArray(pair) && pair.length === 2 && pair.every((part) => typeof part === 'string'),
    )
  )
    throw new Error(`Invalid ledger: ${LEDGER}`)
  const shortMoves = new Map(
    [...moves].map(([from, to]) => [
      from.slice('services/web/'.length),
      to.slice('services/web/'.length),
    ]),
  )
  const changes = []
  for (const from of files) {
    if (
      from === LEDGER ||
      generated(from) ||
      BINARY_EXT.test(from) ||
      from.startsWith('.codex/skills/') ||
      from.startsWith('docs/solutions/')
    )
      continue
    const absolute = path.join(root, from)
    if (!lstatSync(absolute).isFile()) continue
    const original = readFileSync(absolute, 'utf8')
    if (original.includes('\0')) continue
    const to = moves.get(from) ?? from
    let body = original
    if (/^services\/web\/(?:src|tests|e2e)\//.test(from) && /\.[cm]?[jt]sx?$/.test(from))
      body = rewriteModules(body, from, to, moves, before, after)
    body = rewritePathTokens(body, moves)
    if (
      /^services\/web\/(?:biome\.json|knip\.json|tsconfig[^/]*\.json|vitest\.config\.ts)$/.test(
        from,
      ) ||
      from === 'scripts/guards/web-filenames-baseline.json'
    )
      body = rewritePathTokens(body, shortMoves)
    if (body !== original) changes.push({ from, to, body: sortedBaseline(from, body) })
  }
  return { moves: [...moves], changes, ledger: [...ledger, ...moves] }
}

export function applyMoves(options) {
  const root = path.resolve(options.root ?? process.cwd())
  const plan = planMoves({ ...options, root })
  if (options.dryRun) return plan
  for (const [from, to] of plan.moves) {
    mkdirSync(path.dirname(path.join(root, to)), { recursive: true })
    git(root, ['mv', '--', from, to])
  }
  for (const { to, body } of plan.changes) writeFileSync(path.join(root, to), body)
  mkdirSync(path.dirname(path.join(root, LEDGER)), { recursive: true })
  writeFileSync(path.join(root, LEDGER), JSON.stringify(plan.ledger, null, 2) + '\n')
  return plan
}

function withoutModules(file, body) {
  const { ast, literals } = moduleLiterals(file, body)
  for (const literal of literals.sort((a, b) => b.pos - a.pos))
    body = body.slice(0, literal.getStart(ast) + 1) + '<module>' + body.slice(literal.end - 1)
  return body
}

/** Check renamed files on the committed branch; hand-written new files are separate work. */
export function verifyPure({ root = process.cwd(), base }) {
  const entries = git(root, [
    'diff',
    '--name-status',
    '-z',
    '-M',
    `${base}...HEAD`,
    '--',
    SRC,
  ]).split('\0')
  const violations = []
  const renames = []
  for (let i = 0; i < entries.length && entries[i];) {
    const status = entries[i++]
    const from = entries[i++]
    if (!status.startsWith('R') && !status.startsWith('C')) continue
    const to = entries[i++]
    if (status.startsWith('R')) renames.push([from, to])
  }
  // Git's similarity heuristic misses short files. The ledger provides their identity.
  let oldLedger = []
  try {
    oldLedger = JSON.parse(git(root, ['show', `${base}:${LEDGER}`]))
  } catch {
    /* The ledger may predate the base. */
  }
  const ledger = JSON.parse(git(root, ['show', `HEAD:${LEDGER}`]))
  const identities = new Map()
  for (const [from, to] of ledger.slice(oldLedger.length)) {
    const original = identities.get(from) ?? from
    identities.delete(from)
    identities.set(to, original)
  }
  for (const [to, from] of identities) {
    if (!renames.some(([old, next]) => old === from && next === to)) renames.push([from, to])
  }
  for (const [from, to] of renames) {
    const before = git(root, ['show', `${base}:${from}`])
    const after = git(root, ['show', `HEAD:${to}`])
    if (withoutModules(from, before) !== withoutModules(to, after)) violations.push(to)
  }
  return { renames, violations }
}

export function main(args = process.argv.slice(2)) {
  const { values } = parseArgs({
    args,
    strict: true,
    options: {
      map: { type: 'string' },
      root: { type: 'string' },
      'dry-run': { type: 'boolean', default: false },
      'verify-pure': { type: 'string' },
    },
  })
  if (values['verify-pure']) {
    const result = verifyPure({ root: values.root, base: values['verify-pure'] })
    console.log(JSON.stringify(result, null, 2))
    return result.violations.length ? 1 : 0
  }
  if (!values.map) throw new Error('Expected --map <moves.json> or --verify-pure <base>')
  const result = applyMoves({
    root: values.root,
    moves: JSON.parse(readFileSync(path.resolve(values.map), 'utf8')),
    dryRun: values['dry-run'],
  })
  console.log(
    JSON.stringify(
      {
        dryRun: values['dry-run'],
        moves: result.moves,
        changedFiles: result.changes.map(({ to }) => to),
      },
      null,
      2,
    ),
  )
  return 0
}

if (isMainModule(import.meta.url)) {
  try {
    process.exitCode = main()
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}
