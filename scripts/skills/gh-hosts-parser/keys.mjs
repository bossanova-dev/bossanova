import { isAlias, isMap, isScalar, parseDocument } from 'yaml'

/** @param {string} text @returns {string[]} */
export function hostKeys(text) {
  const document = parseDocument(text, {
    schema: 'failsafe',
    uniqueKeys: false,
    prettyErrors: false,
    logLevel: 'silent',
  })
  if (document.errors.length) throw new Error('could not parse gh host configuration')
  if (document.contents === null) return []
  if (!isMap(document.contents)) throw new Error('gh host configuration must be a mapping')
  // gh reads raw mapping keys, without resolving aliases or decoding credential values.
  // https://github.com/cli/go-gh/blob/trunk/internal/yamlmap/yaml_map.go
  return document.contents.items.map(({ key }) =>
    isScalar(key) ? String(key.value ?? '') : isAlias(key) ? key.source : '',
  )
}
