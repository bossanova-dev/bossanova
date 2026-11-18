// Whole-package Go facts from git objects, without invoking a Go toolchain.
import path from 'node:path'

import { isMainModule } from '../main-module.mjs'
import {
  blockFact,
  describerMain,
  envelopeDescriber,
  item,
  listed,
  MAX_BLOCKS,
  MAX_TEXT_CHARS,
  runCli,
} from './describer-kit.mjs'
import { goRoutes, goTokens, routeChanges, shortenGoText } from './verify-go-routes.mjs'
import { NAME_CHARS } from './verify-describe-blocks.mjs'

/** @param {string} source */
export function isGeneratedGo(source) {
  const beforePackage = source.split(/^\s*package\s/m)[0]
  return /^\/\/ Code generated .* DO NOT EDIT\.\r?$/m.test(beforePackage)
}

/** Nearest ancestor module directory, including the repository root.
 * @param {string} file @param {string[]} goModDirs
 */
export function goModuleFor(file, goModDirs) {
  return goModDirs
    .filter((dir) => dir === '' || file.startsWith(`${dir}/`))
    .sort((a, b) => b.length - a.length)[0]
}

/** @param {string} name */
const exported = (name) => /^\p{Lu}/u.test(name)

/** Top-level declarations only: fields, local variables and literal contents cannot be exports.
 * @param {string} source
 */
export function exportedIdentifiers(source) {
  const tokens = goTokens(source)
  const packageName = tokens[tokens.findIndex((t) => t.text === 'package') + 1]?.text ?? ''
  const names = new Set()
  let braces = 0,
    signatureDepth = 0
  /** @param {number} index @param {string} kind */
  const declaration = (index, kind) => {
    const first = tokens[index]?.text ?? ''
    if (exported(first)) names.add(first)
    if (kind !== 'type') {
      while (tokens[index + 1]?.text === ',') {
        index += 2
        if (exported(tokens[index]?.text ?? '')) names.add(tokens[index].text)
      }
    }
  }
  for (let i = 0; i < tokens.length; i++) {
    const token = tokens[i].text
    if (token === '{') braces++
    if (token === '}') braces--
    if (braces !== 0) continue
    if (['(', '['].includes(token)) signatureDepth++
    if ([')', ']'].includes(token)) signatureDepth--
    const previous = tokens[i - 1]
    const declarationStart = !previous || previous.text === ';' || previous.line < tokens[i].line
    if (token === 'func' && signatureDepth === 0 && declarationStart) {
      if (tokens[i + 1]?.text !== '(') {
        const name = tokens[i + 1]?.text ?? ''
        if (exported(name)) names.add(name)
      } else {
        let end = i + 2
        while (end < tokens.length && tokens[end].text !== ')') end++
        const receiver = tokens.slice(i + 2, end)
        // A named receiver starts with its variable followed by a type or pointer.
        const typeIndex =
          receiver[0]?.text === '*'
            ? 1
            : receiver[1]?.text === '*'
              ? 2
              : /^[\p{L}_]/u.test(receiver[1]?.text ?? '')
                ? 1
                : 0
        const type = receiver[typeIndex]?.text ?? ''
        const name = tokens[end + 1]?.text ?? ''
        if (exported(type) && exported(name)) names.add(`${type}.${name}`)
      }
    }
    if (!['const', 'var', 'type'].includes(token)) continue
    if (tokens[i + 1]?.text !== '(') {
      declaration(i + 1, token)
      continue
    }
    let depth = 0
    let spec = true
    for (i += 2; i < tokens.length; i++) {
      const t = tokens[i]
      if (depth === 0 && t.text === ')') break
      if (
        depth === 0 &&
        (spec || (tokens[i - 1]?.line < t.line && ![',', '=', '.'].includes(tokens[i - 1]?.text)))
      ) {
        declaration(i, token)
        spec = false
      }
      if (['(', '[', '{'].includes(t.text)) depth++
      if ([')', ']', '}'].includes(t.text)) depth--
      if (depth === 0 && t.text === ';') spec = true
    }
  }
  return { package: packageName, names }
}

/** @param {string} source */
export function testFunctions(source) {
  const tokens = goTokens(source)
  const names = []
  let depth = 0
  for (let i = 0; i < tokens.length; i++) {
    if (tokens[i].text === '{') depth++
    if (tokens[i].text === '}') depth--
    const name = tokens[i + 1]?.text ?? ''
    if (depth === 0 && tokens[i].text === 'func' && /^(?:Test|Benchmark|Fuzz|Example)/.test(name))
      names.push(name)
  }
  return names
}

/** @type {import('./describer-kit.mjs').CollectItems} */
export function collectGoFacts({ git, base, head, allChanges, changes }) {
  const changedGo = allChanges
    .flatMap((change) =>
      change.status === 'R' &&
      (!change.path.endsWith('_test.go') ||
        path.posix.dirname(change.previousPath) !== path.posix.dirname(change.path))
        ? [
            { path: change.previousPath, status: /** @type {const} */ ('D') },
            { path: change.path, status: /** @type {const} */ ('A') },
          ]
        : [change],
    )
    .filter((change) => change.path.endsWith('.go'))
  if (!changedGo.length) return []
  const files = { base: git.filesAt(base, ['.']), head: git.filesAt(head, ['.']) }
  /** @param {string[]} paths */
  const moduleDirs = (paths) =>
    paths
      .filter((p) => path.posix.basename(p) === 'go.mod')
      .map((p) => (path.posix.dirname(p) === '.' ? '' : path.posix.dirname(p)))
  const dirs = { base: moduleDirs(files.base), head: moduleDirs(files.head) }
  if (!dirs.base.length && !dirs.head.length) return []
  /** @type {Map<string, {dir:string, module:string, code:string[], tests:import('./describer-kit.mjs').Change[]}>} */
  const packages = new Map()
  /** @type {import('./verify-go-routes.mjs').Route[]} */
  const baseRoutes = [],
    headRoutes = []
  /** @type {Map<string,string>} */
  const moduleNames = new Map()
  for (const change of changedGo) {
    const ref = change.status === 'D' ? base : head
    const file = change.status === 'D' ? (change.previousPath ?? change.path) : change.path
    const modDir = goModuleFor(file, change.status === 'D' ? dirs.base : dirs.head)
    if (modDir === undefined) continue
    const source = git.fileAt(ref, file)
    if (isGeneratedGo(source)) continue
    const dir = path.posix.dirname(file)
    const modFile = modDir ? `${modDir}/go.mod` : 'go.mod'
    const key = `${ref}:${modFile}`
    if (!moduleNames.has(key))
      moduleNames.set(
        key,
        git
          .fileAt(ref, modFile)
          .match(/^\s*module\s+(?:"([^"]+)"|(\S+))/m)
          ?.slice(1)
          .find(Boolean) ?? '',
      )
    const mod = moduleNames.get(key)
    if (!mod) continue
    const relative = path.posix.relative(modDir || '.', dir)
    const pkg = packages.get(dir) ?? {
      dir,
      module: relative ? `${mod}/${relative}` : mod,
      code: [],
      tests: [],
    }
    if (file.endsWith('_test.go')) pkg.tests.push(change)
    else {
      pkg.code.push(change.path)
      for (const [target, refName, paths, moduleRoots] of /** @type {const} */ ([
        [baseRoutes, base, files.base, dirs.base],
        [headRoutes, head, files.head, dirs.head],
      ])) {
        const oldFile = refName === base ? (change.previousPath ?? change.path) : change.path
        const namespace = goModuleFor(oldFile, moduleRoots)
        if (!paths.includes(oldFile) || namespace === undefined) continue
        const text = git.fileAt(refName, oldFile)
        if (!isGeneratedGo(text))
          target.push(
            ...goRoutes(text).map((route) => ({ ...route, file: change.path, namespace })),
          )
      }
    }
    packages.set(dir, pkg)
  }
  /** @type {import('./describer-kit.mjs').Fact[]} */
  const facts = []
  const inArea = new Set(changes.flatMap((c) => [c.path, c.previousPath].filter(Boolean)))
  const routes = routeChanges(baseRoutes, headRoutes).filter(
    (route) => route.file && inArea.has(route.file),
  )
  if (routes.length) {
    const counts = ['added', 'changed', 'removed']
      .map((kind) => `${routes.filter((r) => r.change === kind).length} ${kind}`)
      .join(', ')
    const prefix = `Routes: ${counts}: `
    const suffix =
      routes.length > MAX_BLOCKS ? `; ${routes.length - MAX_BLOCKS} not shown as blocks` : ''
    const names = shortenGoText(
      listed(routes.map((r) => `${r.method} ${r.path}`)),
      MAX_TEXT_CHARS - prefix.length - suffix.length,
    )
    facts.push(
      item(
        'other',
        `${prefix}${names}${suffix}`,
        routes.map((r) => r.file),
      ),
    )
    for (const route of routes)
      facts.push(
        blockFact(
          'other',
          {
            kind: 'route',
            method: route.method,
            path: shortenGoText(route.path, NAME_CHARS),
            auth: route.auth,
            change: route.change,
            note: shortenGoText(route.handler || 'handler not detected', MAX_TEXT_CHARS),
          },
          [route.file],
        ),
      )
  }
  /** @type {import('./describer-kit.mjs').Fact[]} */
  const testFacts = []
  for (const pkg of packages.values()) {
    if (pkg.code.length) {
      /** @param {string} ref @param {string[]} tracked */
      const namesAt = (ref, tracked) => {
        const paths = tracked.filter(
          (file) =>
            path.posix.dirname(file) === pkg.dir &&
            file.endsWith('.go') &&
            !file.endsWith('_test.go'),
        )
        const names = new Set()
        for (const source of git.readFiles(ref, paths).values()) {
          if (isGeneratedGo(source)) continue
          const parsed = exportedIdentifiers(source)
          if (parsed.package !== 'main') for (const name of parsed.names) names.add(name)
        }
        return names
      }
      const before = namesAt(base, files.base),
        after = namesAt(head, files.head)
      const added = [...after].filter((n) => !before.has(n)).sort(),
        removed = [...before].filter((n) => !after.has(n)).sort()
      const text = [
        added.length ? `added ${listed(added)}` : '',
        removed.length ? `removed ${listed(removed)}` : '',
      ]
        .filter(Boolean)
        .join('; ')
      if (text) facts.push(item('other', `${pkg.module}: exported ${text}`, pkg.code))
    }
    if (pkg.tests.length) {
      const newTests = new Set()
      for (const change of pkg.tests) {
        const before = new Set(testFunctions(git.fileAt(base, change.previousPath ?? change.path)))
        for (const name of testFunctions(git.fileAt(head, change.path)))
          if (!before.has(name)) newTests.add(name)
      }
      const files = pkg.tests.map(
        (c) =>
          `${path.posix.basename(c.path)}${c.status === 'A' ? ' (added)' : c.status === 'D' ? ' (removed)' : ''}`,
      )
      testFacts.push(
        item(
          'other',
          `${pkg.module} tests: ${listed(files)}${newTests.size ? `; new ${listed([...newTests].sort())}` : ''}`,
          pkg.code.length ? pkg.code : pkg.tests.map((c) => c.path),
        ),
      )
    }
  }
  return [...facts, ...testFacts]
}

export const describeGo = envelopeDescriber(collectGoFacts)
export const main = () => describerMain(describeGo)
if (isMainModule(import.meta.url)) {
  if (process.argv.length > 2) runCli(import.meta.url, 'boss-verify-go', collectGoFacts)
  else await main()
}
