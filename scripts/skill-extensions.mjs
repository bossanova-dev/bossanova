#!/usr/bin/env node
// Compatibility entrypoint for callers that still use the repository path.
// The shared toolbox owns discovery, validation, schemas, and CLI behavior.
import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { main as toolboxMain } from '../skills-toolbox/skill-extensions.mjs'

export function main(argv) {
  return toolboxMain(argv)
}

export {
  DEFAULT_EXTENSION_ROOTS,
  ROLE_SCHEMAS,
  discoverExtensions,
  resolveExtensionRoots,
  validateResult,
} from '../skills-toolbox/skill-extensions.mjs'

if (isMainModule(import.meta.url)) {
  process.exit(main(process.argv.slice(2)))
}
