#!/usr/bin/env node
import { fileURLToPath } from 'node:url'
import ts from 'typescript'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { trackedFiles, readTrackedText, assertNonVacuousScan } from '../dev/tree-scan-lib.mjs'

/** @param {{file: string, text: string}[]} entries */
export function findUnadmittedGolangci(entries) {
  /** @type {{file: string, line: number}[]} */
  const findings = []
  for (const { file, text } of entries) {
    if (file === 'scripts/test-infra/golangci-admission.mjs') continue
    const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true)
    /** @param {ts.Node} node */
    function visit(node) {
      if (ts.isCallExpression(node)) {
        const name = ts.isIdentifier(node.expression)
          ? node.expression.text
          : ts.isPropertyAccessExpression(node.expression)
            ? node.expression.name.text
            : ''
        const command = node.arguments[0]
        const args = node.arguments[1]
        if (
          command &&
          ts.isStringLiteralLike(command) &&
          ((/^(spawn|spawnSync|execFile|execFileSync)$/.test(name) &&
            command.text === 'golangci-lint' &&
            args &&
            ts.isArrayLiteralExpression(args) &&
            args.elements.some((arg) => ts.isStringLiteralLike(arg) && arg.text === 'run')) ||
            (/^(exec|execSync)$/.test(name) && /^golangci-lint\s+run(?:\s|$)/.test(command.text)))
        )
          findings.push({
            file,
            line: source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1,
          })
      }
      ts.forEachChild(node, visit)
    }
    visit(source)
  }
  return findings
}

function main() {
  const root = fileURLToPath(new URL('../..', import.meta.url))
  const files = trackedFiles(root).filter((file) =>
    /^(scripts|skills-toolbox)\/.*\.(mjs|js)$/.test(file),
  )
  assertNonVacuousScan(files, files.length || 1, 'golangci-admission')
  const findings = findUnadmittedGolangci(
    files.map((file) => ({ file, text: readTrackedText(file, root) })),
  )
  console.log(`golangci-admission: scanned ${files.length} script files`)
  for (const finding of findings)
    console.error(
      `${finding.file}:${finding.line}: launch golangci-lint run through runGolangciSync`,
    )
  if (findings.length) process.exitCode = 1
}
if (isMainModule(import.meta.url)) main()
