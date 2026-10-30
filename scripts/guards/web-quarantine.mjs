import { createRequire } from 'node:module'

const require = createRequire(import.meta.url)
/** @type {typeof import('typescript') | undefined} */
let compiler

function loadCompiler() {
  compiler ??= require('typescript')
  return compiler
}

function propertyKey(name) {
  const ts = loadCompiler()
  if (!ts.isComputedPropertyName(name)) return name.text
  const expression = name.expression
  return ts.isStringLiteral(expression) || ts.isNoSubstitutionTemplateLiteral(expression)
    ? expression.text
    : null
}

/** Discover literal test identities and retry markers without matching comments or callback text. */
export function parseWebTests(file, text, runner) {
  const ts = loadCompiler()
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  const tests = []
  const problems = []
  const literal = (node) =>
    node && (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node))
      ? node.text
      : null
  const properties = (args, key) =>
    args.flatMap((arg) =>
      ts.isObjectLiteralExpression(arg)
        ? arg.properties.filter(
            (prop) =>
              (ts.isPropertyAssignment(prop) || ts.isShorthandPropertyAssignment(prop)) &&
              propertyKey(prop.name) === key,
          )
        : [],
    )
  const tagValue = (node) => {
    if (literal(node) !== null) return literal(node).includes('@quarantine')
    return ts.isArrayLiteralExpression(node) && node.elements.some(tagValue)
  }
  function isPlaywrightAnnotation(node, name) {
    if (!/^test\.(?:skip|fixme|fail)$/.test(name)) return false
    const args = node.arguments
    return (
      args.length <= 1 ||
      ts.isArrowFunction(args[0]) ||
      ts.isFunctionExpression(args[0]) ||
      (args.length === 2 &&
        (ts.isStringLiteral(args[1]) ||
          ts.isNoSubstitutionTemplateLiteral(args[1]) ||
          ts.isTemplateExpression(args[1])))
    )
  }
  function visit(node, titles = [], inherited = false) {
    if (ts.isCallExpression(node)) {
      const name = node.expression.getText(source)
      const suite =
        /^(?:test\.)?describe(?:\.(?:only|skip|serial|parallel|fixme|concurrent|sequential|shuffle|todo))*$/.test(
          name,
        )
      const test =
        runner === 'playwright'
          ? !isPlaywrightAnnotation(node, name) &&
            /^(?:test|it)(?:\.(?:only|skip|fixme|todo|concurrent|sequential|fails|fail))*$/.test(
              name,
            )
          : /^(?:test|it)(?:\.(?:only|skip|fixme|todo|concurrent|sequential|fails))*$/.test(name)
      const callee = node.expression
      const factory =
        (ts.isCallExpression(callee) || ts.isTaggedTemplateExpression(callee)) &&
        /^(?:test|it|describe)(?:\.|\()/.test(
          ts.isTaggedTemplateExpression(callee) ? callee.tag.getText(source) : name,
        )
      const title = literal(node.arguments[0])
      const tags = properties([...node.arguments], 'tag')
      if (
        runner === 'playwright' &&
        (suite || test || factory) &&
        tags.some((prop) => ts.isShorthandPropertyAssignment(prop))
      ) {
        problems.push(`${file}: shorthand tag requires a literal tag value to verify quarantine`)
      }
      const tagged =
        runner === 'playwright'
          ? tags.some((prop) => ts.isPropertyAssignment(prop) && tagValue(prop.initializer)) ||
            (title ?? '').includes('@quarantine')
          : properties([...node.arguments], 'retry').length > 0
      if (factory && (tagged || inherited)) {
        problems.push(
          `${file}: unsupported test factory carries a retry marker; use a literal test declaration`,
        )
      }
      if (suite || test) {
        if (title === null && (tagged || inherited)) {
          problems.push(`${file}: retry marker requires a literal test title`)
        }
        if (test && title !== null) {
          tests.push({ id: [...titles, title].join(' > '), marked: tagged || inherited })
          return
        }
        if (suite) {
          for (const arg of node.arguments) {
            if (ts.isArrowFunction(arg) || ts.isFunctionExpression(arg)) {
              visit(arg.body, title === null ? titles : [...titles, title], inherited || tagged)
            }
          }
          return
        }
      }
    }
    ts.forEachChild(node, (child) => visit(child, titles, inherited))
  }
  visit(source)
  return { tests, problems }
}

/** Any global retry property is forbidden, even a zero-valued default. */
export function hasGlobalRetry(text) {
  const ts = loadCompiler()
  const source = ts.createSourceFile('vitest.config.ts', text, ts.ScriptTarget.Latest, true)
  let found = false
  function visit(node) {
    if (
      (ts.isPropertyAssignment(node) || ts.isShorthandPropertyAssignment(node)) &&
      propertyKey(node.name) === 'retry'
    )
      found = true
    ts.forEachChild(node, visit)
  }
  visit(source)
  return found
}

export function findWebQuarantineProblems(entries, sources, vitestConfig) {
  const problems = []
  const listed = new Set()
  const discovered = new Map()
  for (const [file, text] of sources) {
    const runner = /\.spec\.[cm]?[jt]sx?$/.test(file) ? 'playwright' : 'vitest'
    const parsed = parseWebTests(file, text, runner)
    discovered.set(file, parsed.tests)
    problems.push(...parsed.problems)
  }
  for (const entry of entries) {
    if (!['playwright', 'vitest'].includes(entry?.runner)) continue
    const where = `${entry.runner} ${entry.file ?? 'no file'}`
    const validFile =
      typeof entry.file === 'string' &&
      entry.file.startsWith('services/web/') &&
      !entry.file.split('/').includes('..') &&
      (entry.runner === 'playwright'
        ? /\.spec\.tsx?$/.test(entry.file)
        : /\.test\.tsx?$/.test(entry.file))
    if (!validFile) {
      problems.push(`${where}: file must be a repo-relative web ${entry.runner} test path`)
      continue
    }
    if (entry.label !== undefined)
      problems.push(`${where}: web entries must not carry a Bazel label`)
    if (!sources.has(entry.file))
      problems.push(`${where}: file does not exist in scanned web tests`)
    for (const id of Array.isArray(entry.tests) ? entry.tests : []) {
      const key = `${entry.file}:${id}`
      if (listed.has(key)) problems.push(`${where}: duplicate test ${id}; merge the entries`)
      listed.add(key)
      const matches = (discovered.get(entry.file) ?? []).filter((test) => test.id === id)
      if (matches.length !== 1)
        problems.push(`${where}: test ${id} not found uniquely (stale test id)`)
      else if (!matches[0].marked)
        problems.push(
          `${where}: test ${id} lacks ${entry.runner === 'playwright' ? '@quarantine' : 'retry'} marker`,
        )
    }
  }
  for (const [file, tests] of discovered) {
    for (const test of tests) {
      if (test.marked && !listed.has(`${file}:${test.id}`)) {
        problems.push(`${file}: test ${test.id} has a retry marker but no quarantine entry`)
      }
    }
  }
  if (hasGlobalRetry(vitestConfig))
    problems.push(
      'services/web/vitest.config.ts: global retry is forbidden; quarantine individual tests',
    )
  return problems
}
