import { execFileSync } from 'node:child_process'
import { existsSync, lstatSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { matchesGlob } from 'node:path'
import ts from 'typescript'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { BINARY_EXT, trackedFiles } from '../dev/tree-scan-lib.mjs'

const LEDGER = 'scripts/layout/moves.json'
const GENERATED = [
  '.codex/skills/',
  'docs/testing/test-command-manifest.md',
  'docs/philosophy/rules.md',
]
const FIXTURE = /(?:^|\/)(?:fixtures|testdata|assets)\/|(?:^|\/)skill-parity\/baseline\//

function excluded(file) {
  return (
    FIXTURE.test(file) ||
    GENERATED.some((prefix) => file.startsWith(prefix)) ||
    /^lib\/bossalib\/contract\/plugin\/skillpayload\/skills\/[^/]+\/toolbox\//.test(file)
  )
}

function relative(from, to) {
  const result = path.posix.relative(path.posix.dirname(from), to)
  return result.startsWith('.') ? result : `./${result}`
}

function checkedPath(file) {
  if (
    typeof file !== 'string' ||
    !/^(?:scripts|skills-toolbox)\//.test(file) ||
    path.posix.normalize(file) !== file ||
    file.includes('\\') ||
    (excluded(file) && !/(?:^|\/)skill-parity\/baseline\/[^/]+\.json$/.test(file))
  ) {
    throw new Error(`Invalid move path: ${file}`)
  }
}

function expandMoves(root, requested, tracked) {
  if (!Array.isArray(requested)) throw new Error('Move map must be a JSON list of [from, to] pairs')
  const pairs = new Map()
  function add(pair) {
    if (!Array.isArray(pair) || pair.length !== 2) throw new Error('Expected a [from, to] pair')
    const [from, to] = pair
    checkedPath(from)
    checkedPath(to)
    if (from === to || path.posix.basename(from) !== path.posix.basename(to)) {
      throw new Error(`Moves must keep the basename: ${from} -> ${to}`)
    }
    if (pairs.has(from) && pairs.get(from) !== to) throw new Error(`Conflicting moves for ${from}`)
    if (!tracked.has(from) || !existsSync(path.join(root, from)))
      throw new Error(`Source is not a tracked file: ${from}`)
    if (!lstatSync(path.join(root, from)).isFile())
      throw new Error(`Source is not a regular file: ${from}`)
    if (existsSync(path.join(root, to)) || [...pairs.values()].includes(to)) {
      if (pairs.get(from) === to) return
      throw new Error(`Destination already exists: ${to}`)
    }
    pairs.set(from, to)
  }
  for (const pair of requested) add(pair)
  for (const [from, to] of [...pairs]) {
    if (!from.endsWith('.mjs') || from.endsWith('.test.mjs')) continue
    for (const suffix of ['.test.mjs', '-baseline.json']) {
      const companion = from.replace(/\.mjs$/, suffix)
      if (tracked.has(companion)) add([companion, to.replace(/\.mjs$/, suffix)])
    }
  }
  for (const record of tracked) {
    if (!/^scripts\/guards\/registry\/.*\.mjs$/.test(record)) continue
    const ast = ts.createSourceFile(
      record,
      readFileSync(path.join(root, record), 'utf8'),
      ts.ScriptTarget.Latest,
      true,
      ts.ScriptKind.JS,
    )
    const fields = new Map()
    function visit(node) {
      if (ts.isPropertyAssignment(node) && ts.isStringLiteralLike(node.initializer)) {
        fields.set(
          node.name.getText(ast).replaceAll("'", '').replaceAll('"', ''),
          node.initializer.text,
        )
      }
      ts.forEachChild(node, visit)
    }
    visit(ast)
    const baseline = fields.get('baseline')
    if (!baseline || pairs.has(baseline) || !tracked.has(baseline)) continue
    for (const [from, to] of [...pairs]) {
      const command = fields.get('command') ?? ''
      if (
        command.split(/\s+/).some((token) => token.replace(/^['"]|['"]$/g, '') === from) &&
        path.posix.dirname(from) === path.posix.dirname(baseline)
      ) {
        add([baseline, path.posix.join(path.posix.dirname(to), path.posix.basename(baseline))])
      }
    }
  }
  return pairs
}

function rewriteJavaScript(source, from, to, moves) {
  const ast = ts.createSourceFile(from, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.JS)
  const edits = []
  const classified = new Set()
  /** @param {import('typescript').Node | undefined} node
   * @returns {node is import('typescript').StringLiteralLike} */
  const isLiteral = (node) => !!node && ts.isStringLiteralLike(node)
  const name = (node) => (ts.isPropertyAccessExpression(node) ? node.name.text : node.getText(ast))
  const isMeta = (node, member) =>
    ts.isPropertyAccessExpression(node) &&
    node.expression.kind === ts.SyntaxKind.MetaProperty &&
    node.name.text === member
  const editLiteral = (node, value) => {
    if (value !== node.text) edits.push({ start: node.getStart(ast) + 1, end: node.end - 1, value })
  }
  const fail = (node) => {
    const { line } = ast.getLineAndCharacterOfPosition(node.getStart(ast))
    throw new Error(`${from}:${line + 1}: unclassifiable import.meta-relative path expression`)
  }
  function mark(node) {
    if (ts.isMetaProperty(node)) classified.add(node)
    ts.forEachChild(node, mark)
  }
  const aliases = new Map()
  function collectAliases(node) {
    if (
      ts.isVariableDeclaration(node) &&
      ts.isIdentifier(node.name) &&
      node.initializer &&
      ts.isVariableDeclarationList(node.parent) &&
      node.parent.flags & ts.NodeFlags.Const
    )
      aliases.set(node.name.text, node.initializer)
    ts.forEachChild(node, collectAliases)
  }
  collectAliases(ast)
  function unalias(node) {
    const seen = new Set()
    while (ts.isIdentifier(node) && aliases.has(node.text) && !seen.has(node.text)) {
      seen.add(node.text)
      node = aliases.get(node.text)
    }
    return node
  }
  function dirnameBase(node) {
    node = unalias(node)
    if (isMeta(node, 'dirname')) return true
    if (
      !ts.isCallExpression(node) ||
      name(node.expression) !== 'dirname' ||
      node.arguments.length !== 1
    )
      return false
    const inner = unalias(node.arguments[0])
    if (
      ts.isCallExpression(inner) &&
      name(inner.expression) === 'fileURLToPath' &&
      inner.arguments.length === 1 &&
      isMeta(inner.arguments[0], 'url')
    )
      return true
    // path.dirname(new URL(import.meta.url).pathname)
    return (
      ts.isPropertyAccessExpression(inner) &&
      inner.name.text === 'pathname' &&
      ts.isNewExpression(inner.expression) &&
      name(inner.expression.expression) === 'URL' &&
      inner.expression.arguments?.length === 1 &&
      isMeta(inner.expression.arguments[0], 'url')
    )
  }
  function visit(node) {
    // Module specifiers are resolved from the old location, then re-relativized from the new one.
    let specifier
    if (ts.isImportDeclaration(node) || ts.isExportDeclaration(node))
      specifier = node.moduleSpecifier
    if (ts.isCallExpression(node) && node.expression.kind === ts.SyntaxKind.ImportKeyword)
      specifier = node.arguments[0]
    if (isLiteral(specifier) && specifier.text.startsWith('.')) {
      const [filename, suffix = ''] = specifier.text.match(/^([^?#]*)(.*)$/s).slice(1)
      const target = path.posix.normalize(path.posix.join(path.posix.dirname(from), filename))
      editLiteral(specifier, relative(to, moves.get(target) ?? target) + suffix)
    }
    if (
      ts.isNewExpression(node) &&
      name(node.expression) === 'URL' &&
      node.arguments?.length === 2 &&
      isMeta(node.arguments[1], 'url')
    ) {
      const literal = node.arguments[0]
      if (!isLiteral(literal)) {
        if (from !== to) fail(node)
      } else {
        if (!/^[a-z][a-z\d+.-]*:/i.test(literal.text) && !literal.text.startsWith('/')) {
          const target = path.posix.normalize(
            path.posix.join(path.posix.dirname(from), literal.text),
          )
          const rewritten = relative(to, moves.get(target) ?? target)
          editLiteral(
            literal,
            rewritten + (literal.text.endsWith('/') && !rewritten.endsWith('/') ? '/' : ''),
          )
        }
        mark(node.arguments[1])
      }
    }
    if (
      ts.isCallExpression(node) &&
      ['resolve', 'join'].includes(name(node.expression)) &&
      node.arguments.length &&
      dirnameBase(node.arguments[0])
    ) {
      if (!node.arguments.slice(1).every(isLiteral)) {
        if (from !== to) fail(node)
      } else {
        const target = path.posix
          .resolve(
            '/',
            path.posix.dirname(from),
            ...node.arguments.slice(1).map((arg) => (isLiteral(arg) ? arg.text : '')),
          )
          .slice(1)
        const adjusted =
          path.posix.relative(path.posix.dirname(to), moves.get(target) ?? target) || '.'
        if (from !== to || moves.has(target)) {
          if (node.arguments.length > 1) {
            edits.push({
              start: node.arguments[1].getStart(ast),
              end: node.arguments.at(-1).end,
              value: `'${adjusted.replaceAll("'", "\\'")}'`,
            })
          } else {
            edits.push({
              start: node.arguments[0].end,
              end: node.arguments[0].end,
              value: `, '${adjusted}'`,
            })
          }
        }
        mark(unalias(node.arguments[0]))
      }
    }
    // A module's identity (including self-reads and upward root discovery) moves with it.
    if (
      ts.isCallExpression(node) &&
      name(node.expression) === 'fileURLToPath' &&
      node.arguments.length === 1 &&
      isMeta(node.arguments[0], 'url')
    )
      mark(node.arguments[0])
    // A filename used as module identity follows the module without a path rewrite.
    if (isMeta(node, 'filename')) mark(node)
    // Entry detection uses the current module URL and needs no depth rewrite.
    if (
      ts.isCallExpression(node) &&
      name(node.expression) === 'isMainModule' &&
      node.arguments.length &&
      isMeta(node.arguments[0], 'url')
    )
      mark(node.arguments[0])
    ts.forEachChild(node, visit)
  }
  visit(ast)
  if (from !== to) {
    function check(node) {
      if (ts.isMetaProperty(node) && !classified.has(node)) fail(node)
      ts.forEachChild(node, check)
    }
    check(ast)
  }
  for (const edit of edits.sort((a, b) => b.start - a.start)) {
    source = source.slice(0, edit.start) + edit.value + source.slice(edit.end)
  }
  return source
}

/** Rewrite complete path tokens without corrupting longer filenames or replacement chains. */
export function rewritePathTokens(source, moves) {
  const entries = [...moves].sort(([a], [b]) => b.length - a.length)
  if (!entries.length) return source
  const escaped = entries.map(([from]) => from.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')
  const lookup = new Map(entries)
  source = source.replace(
    new RegExp(`(?<![\\w./-])((?:\\.{1,2}/)*)(?:${escaped})(?![\\w/-]|\\.[\\w-])`, 'g'),
    (token, prefix) => prefix + lookup.get(token.slice(prefix.length)),
  )
  for (const [from, to] of entries) {
    if (!from.startsWith('skills-toolbox/')) continue
    const oldSuffix = from.slice('skills-toolbox/'.length)
    const newSuffix = to.slice('skills-toolbox/'.length)
    const escapedSuffix = oldSuffix.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
    source = source.replace(
      new RegExp(`toolbox/${escapedSuffix}(?![\\w/-]|\\.[\\w-])`, 'g'),
      `toolbox/${newSuffix}`,
    )
    source = source.replace(
      new RegExp(
        `(\\$\\{?(?:process\\.env\\.)?BOSS_[A-Z_]*TOOLBOX\\}?/)${escapedSuffix}(?![\\w/-]|\\.[\\w-])`,
        'g',
      ),
      (_, prefix) => prefix + newSuffix,
    )
  }
  return source
}

// Narrow globs whose entire tracked selection moves to one folder can move with it. Broad
// globs retaining old subjects remain valid and acquire explicit moved subjects as needed.
function rewriteScans(source, file, moves, tracked) {
  if (!/^scripts\/guards\/registry\/.*\.mjs$/.test(file)) return source
  const ast = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.JS)
  const edits = []
  function visit(node) {
    if (
      ts.isPropertyAssignment(node) &&
      node.name.getText(ast).replaceAll("'", '').replaceAll('"', '') === 'scans' &&
      ts.isArrayLiteralExpression(node.initializer)
    ) {
      for (const item of node.initializer.elements) {
        if (!ts.isStringLiteralLike(item) || !/[?*[]/.test(item.text)) continue
        const matches = tracked.filter((candidate) => matchesGlob(candidate, item.text))
        const moved = matches.filter(
          (candidate) => moves.has(candidate) && !matchesGlob(moves.get(candidate), item.text),
        )
        if (!moved.length) continue
        const oldDir = path.posix.dirname(item.text)
        const newDirs = new Set(moved.map((candidate) => path.posix.dirname(moves.get(candidate))))
        const sameOldDir = moved.every((candidate) => path.posix.dirname(candidate) === oldDir)
        let replacement
        if (matches.length === moved.length && newDirs.size === 1 && sameOldDir) {
          replacement = [path.posix.join([...newDirs][0], path.posix.basename(item.text))]
        } else {
          replacement = [item.text, ...moved.map((candidate) => moves.get(candidate))]
        }
        edits.push({
          start: item.getStart(ast),
          end: item.end,
          value: replacement.map((value) => JSON.stringify(value)).join(', '),
        })
      }
    }
    ts.forEachChild(node, visit)
  }
  visit(ast)
  for (const edit of edits.sort((a, b) => b.start - a.start))
    source = source.slice(0, edit.start) + edit.value + source.slice(edit.end)
  return source
}

function rewriteVendorMap(source, file, moves) {
  if (
    !['scripts/skills/vendor-toolbox.mjs', 'scripts/skills/vendor-toolbox.test.mjs'].includes(file)
  )
    return source
  const ast = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.JS)
  const edits = []
  function visit(node) {
    if (ts.isStringLiteralLike(node)) {
      const moved = moves.get(`skills-toolbox/${node.text}`)
      if (moved)
        edits.push({
          start: node.getStart(ast) + 1,
          end: node.end - 1,
          value: moved.slice('skills-toolbox/'.length),
        })
    }
    ts.forEachChild(node, visit)
  }
  visit(ast)
  for (const edit of edits.sort((a, b) => b.start - a.start))
    source = source.slice(0, edit.start) + edit.value + source.slice(edit.end)
  return source
}

function rewriteWritableText(source, file, moves, tracked) {
  source = rewriteVendorMap(source, file, moves)
  source = rewriteScans(source, file, moves, tracked)
  // AGENTS.md combines authored guidance with a generated rules region.
  return source
    .split(/(<!-- BEGIN GENERATED guard-registry:[\s\S]*?<!-- END GENERATED guard-registry -->)/g)
    .map((part) =>
      part.startsWith('<!-- BEGIN GENERATED guard-registry:')
        ? part
        : rewritePathTokens(part, moves),
    )
    .join('')
}

/** Compute every edit before running git mv or writing, including failures in unmoved importers. */
export function planMoves({ root = process.cwd(), moves }) {
  root = path.resolve(root)
  const files = trackedFiles(root)
  const mapping = expandMoves(root, moves, new Set(files))
  const ledgerPath = path.join(root, LEDGER)
  const ledger = existsSync(ledgerPath) ? JSON.parse(readFileSync(ledgerPath, 'utf8')) : []
  if (!Array.isArray(ledger) || !ledger.every((pair) => Array.isArray(pair) && pair.length === 2))
    throw new Error(`Invalid move ledger: ${LEDGER}`)
  const changes = []
  for (const from of files) {
    if (from === LEDGER || excluded(from) || BINARY_EXT.test(from)) continue
    const absolute = path.join(root, from)
    if (!lstatSync(absolute).isFile()) continue
    const original = readFileSync(absolute, 'utf8')
    if (original.includes('\0')) continue
    const to = mapping.get(from) ?? from
    let body = /\.(?:mjs|cjs|js)$/.test(from)
      ? rewriteJavaScript(original, from, to, mapping)
      : original
    body = rewriteWritableText(body, from, mapping, files)
    if (body !== original && from.endsWith('-baseline.json')) {
      const baseline = JSON.parse(body)
      if (Array.isArray(baseline) && baseline.every((entry) => typeof entry === 'string'))
        body = JSON.stringify(baseline.sort(), null, 2) + '\n'
      else if (
        baseline &&
        typeof baseline === 'object' &&
        !Array.isArray(baseline) &&
        Object.keys(baseline).every((key) => key.includes('/'))
      )
        body =
          JSON.stringify(
            Object.fromEntries(
              Object.entries(baseline).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)),
            ),
            null,
            2,
          ) + '\n'
    }
    if (body !== original) changes.push({ from, to, body })
  }
  return {
    moves: [...mapping],
    changes,
    ledger: [...ledger, ...mapping],
    regenerate: [
      'make codex-skills',
      'make vendor-toolbox',
      'make test-manifest-update',
      'node scripts/guards/rules-table.mjs --write',
    ],
  }
}

export function applyMoves(options) {
  const root = path.resolve(options.root ?? process.cwd())
  const plan = planMoves({ ...options, root })
  if (options.dryRun) return plan
  for (const [from, to] of plan.moves) {
    mkdirSync(path.dirname(path.join(root, to)), { recursive: true })
    execFileSync('git', ['mv', '--', from, to], { cwd: root })
  }
  for (const change of plan.changes) writeFileSync(path.join(root, change.to), change.body)
  mkdirSync(path.join(root, 'scripts/layout'), { recursive: true })
  writeFileSync(path.join(root, LEDGER), JSON.stringify(plan.ledger, null, 2) + '\n')
  return plan
}

export function main(args = process.argv.slice(2)) {
  const { values } = parseArgs({
    args,
    strict: true,
    options: {
      map: { type: 'string' },
      root: { type: 'string' },
      'dry-run': { type: 'boolean', default: false },
    },
  })
  if (!values.map)
    throw new Error(
      'Usage: node scripts/layout/move.mjs --map <moves.json> [--root <repo>] [--dry-run]',
    )
  const moves = JSON.parse(readFileSync(path.resolve(values.map), 'utf8'))
  const result = applyMoves({ root: values.root, moves, dryRun: values['dry-run'] })
  console.log(
    JSON.stringify(
      {
        dryRun: values['dry-run'],
        moves: result.moves,
        changedFiles: result.changes.map(({ to }) => to),
        regenerate: result.regenerate,
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
