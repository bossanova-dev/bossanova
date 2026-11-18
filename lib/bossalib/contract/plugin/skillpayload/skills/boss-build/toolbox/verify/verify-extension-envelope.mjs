import { readFileSync, writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { validateVerifyContext } from './verify-contract.mjs'

/**
 * Repo-local extensions execute frozen base code, so keep their context contract unchanged.
 * Built-ins ship with the core and can consume its exact UI classification.
 * @param {{builtin?: boolean} | null | undefined} extension
 * @param {unknown} context
 * @param {unknown} [uiFiles]
 * @returns {unknown}
 */
export function extensionEnvelopeFor(extension, context, uiFiles) {
  if (context === null || typeof context !== 'object' || Array.isArray(context)) return context
  const { uiFiles: _uiFiles, ...legacy } = /** @type {Record<string, unknown>} */ (context)
  const files = uiFiles ?? _uiFiles
  return extension?.builtin === true && Array.isArray(files)
    ? { ...legacy, uiFiles: files }
    : legacy
}

/** Write one dispatch context, optionally retaining the intent-enriched common context. */
export function main(argv = process.argv.slice(2)) {
  const { values, positionals } = parseArgs({
    args: argv,
    strict: true,
    options: {
      judge: { type: 'string' },
      context: { type: 'string' },
      out: { type: 'string' },
      builtin: { type: 'boolean', default: false },
    },
  })
  if (positionals.length || !values.judge || !values.out)
    throw new Error(
      'usage: verify-extension-envelope.mjs --judge <file> [--context <file>] [--builtin] --out <file>',
    )
  const judge = JSON.parse(readFileSync(values.judge, 'utf8'))
  const context = values.context
    ? JSON.parse(readFileSync(values.context, 'utf8'))
    : judge.extensionEnvelope
  const envelope = extensionEnvelopeFor(
    { builtin: values.builtin },
    context,
    judge.extensionUiFiles,
  )
  const valid = validateVerifyContext(envelope)
  if (!valid.ok) throw new Error(`invalid extension context: ${valid.errors.join('; ')}`)
  writeFileSync(values.out, `${JSON.stringify(envelope)}\n`)
  return values.out
}

if (isMainModule(import.meta.url)) {
  try {
    console.log(main())
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 2
  }
}
