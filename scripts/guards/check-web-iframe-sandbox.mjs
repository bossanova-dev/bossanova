#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import ts from 'typescript'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import record from './registry/web-iframe-sandbox.mjs'

// The sandbox is the boundary that keeps framed content (a proof page) out of
// the app; CSP frame-src only limits which URLs may load. Browsers match
// sandbox tokens ASCII case-insensitively, so tokens are lowercased first.
const ALLOWED_SANDBOX_TOKENS = new Set(['allow-scripts'])
// <object>, <embed> and <frame> take no sandbox, so they cannot frame safely.
const FRAMING_TAGS = new Set(['iframe', 'object', 'embed', 'frame'])
const FRAMING_MARKUP = /<(iframe|object|embed|frame)/i
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

function literalText(initializer) {
  if (!initializer) return null
  if (ts.isStringLiteral(initializer)) return initializer.text
  const expression = ts.isJsxExpression(initializer) ? initializer.expression : null
  return expression && ts.isStringLiteralLike(expression) ? expression.text : null
}

function attribute(element, name) {
  return element.attributes.properties.find(
    (property) => ts.isJsxAttribute(property) && property.name.getText() === name,
  )
}

function iframeProblem(element) {
  if (element.attributes.properties.some(ts.isJsxSpreadAttribute)) {
    return 'spreads props, so its sandbox cannot be checked'
  }
  const sandbox = attribute(element, 'sandbox')
  if (!sandbox) return 'has no sandbox attribute'
  const value = literalText(sandbox.initializer)
  if (value === null) return 'sets sandbox to a non-literal value'
  const granted = value
    .split(/[\t\n\f\r ]+/)
    .filter((token) => token && !ALLOWED_SANDBOX_TOKENS.has(token.toLowerCase()))
  if (granted.length > 0) return `sandbox grants ${granted.join(', ')}`
  return literalText(attribute(element, 'allow')?.initializer) === ''
    ? null
    : 'must set allow="" to delegate no permissions'
}

// A non-literal __html cannot be checked, so it fails like a non-literal sandbox.
function innerHtmlProblem(initializer) {
  const value = initializer && ts.isJsxExpression(initializer) ? initializer.expression : null
  const html =
    value && ts.isObjectLiteralExpression(value)
      ? value.properties
          .filter(ts.isPropertyAssignment)
          .find((property) => property.name.getText() === '__html')?.initializer
      : null
  if (!html || !ts.isStringLiteralLike(html)) {
    return 'dangerouslySetInnerHTML is not a literal string, so its markup cannot be checked'
  }
  return FRAMING_MARKUP.test(html.text) ? 'dangerouslySetInnerHTML injects framing markup' : null
}

function elementProblem(element, tag) {
  if (tag === 'iframe') return iframeProblem(element)
  if (FRAMING_TAGS.has(tag)) return 'cannot be sandboxed; frame pages with <iframe>'
  const innerHtml = attribute(element, 'dangerouslySetInnerHTML')
  return innerHtml ? innerHtmlProblem(innerHtml.initializer) : null
}

function createdFramingTag(call) {
  const callee = call.expression
  const name = ts.isPropertyAccessExpression(callee) ? callee.name.text : callee.getText()
  const [first] = call.arguments
  const tag = first !== undefined && ts.isStringLiteralLike(first) ? first.text.toLowerCase() : null
  return name === 'createElement' && FRAMING_TAGS.has(tag) ? tag : null
}

/** Each element in `source` that frames content unsafely, as `file:line: <tag> problem`. */
export function iframeViolations(file, source) {
  const kind = file.endsWith('.tsx') ? ts.ScriptKind.TSX : ts.ScriptKind.TS
  const sourceFile = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, kind)
  const problems = []
  const report = (node, tag, problem) => {
    const { line } = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile))
    problems.push(`${file}:${line + 1}: <${tag}> ${problem}`)
  }
  const visit = (node) => {
    if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
      const tag = node.tagName.getText()
      const problem = elementProblem(node, tag)
      if (problem) report(node, tag, problem)
    } else if (ts.isCallExpression(node)) {
      const tag = createdFramingTag(node)
      if (tag) report(node, tag, 'is created outside JSX, so it cannot be checked')
    }
    ts.forEachChild(node, visit)
  }
  visit(sourceFile)
  return problems
}

export function checkWebIframeSandbox(root = repoRoot) {
  return scannedFiles(root, record.scans).flatMap((file) =>
    iframeViolations(file, fs.readFileSync(path.join(root, file), 'utf8')),
  )
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({ options: { root: { type: 'string' } }, strict: true })
  const failures = checkWebIframeSandbox(values.root ?? repoRoot)
  if (failures.length) {
    console.error(failures.join('\n'))
    console.error(
      'Frame untrusted pages only with a JSX <iframe sandbox="allow-scripts" allow="">, at most.',
    )
    process.exitCode = 1
  } else {
    console.log(
      'OK: every web frame is an <iframe> sandboxed to allow-scripts at most, with allow=""',
    )
  }
}
