// The extension contract's minimal YAML-frontmatter reader. Supports flat scalars, top-level
// literal/folded block scalars, and the single nested `x-boss-extension:` block this contract
// needs, whose keys may hold a list of flat maps (`sections:` then `- id: x` items) — no external
// YAML dep (matches the no-new-deps constraint of the scripts/ helpers).
//
// `hasFrontmatter` reports whether a delimited block was found at all. It matters because a file
// with a broken fence (an opening `---` with no closing one, say) parses to the SAME empty `data`
// as a file whose frontmatter is perfectly valid and simply declares no marker — and discovery has
// to tell a failed declaration apart from a deliberate non-extension.
export function parseFrontmatter(text) {
  const match = /^---\n([\s\S]*?)\n---\n?([\s\S]*)$/.exec(text)
  if (!match) return { data: {}, body: text, hasFrontmatter: false }
  const data = {}
  let current = null // name of the block we are collecting nested keys into
  // A nested key with no value, whose more-indented `- key: value` lines make it a list of maps.
  let list = null
  const lines = match[1].split('\n')
  for (let index = 0; index < lines.length; index += 1) {
    const raw = lines[index]
    if (!raw.trim() || raw.trim().startsWith('#')) continue
    const indent = /^ */.exec(raw)[0].length
    if (list && indent > list.indent) {
      const item = /^ *- ([\w-]+):\s*(.*)$/.exec(raw)
      const field = /^ *([\w-]+):\s*(.*)$/.exec(raw)
      const block = data[current]
      if (item) {
        if (!Array.isArray(block[list.key])) block[list.key] = []
        block[list.key].push({ [item[1]]: coerce(item[2]) })
      } else if (field) {
        // Before any item, a deeper key reads as a sibling, as it did before lists existed.
        const target = Array.isArray(block[list.key]) ? block[list.key].at(-1) : block
        target[field[1]] = coerce(field[2])
      }
      continue
    }
    list = null
    const nested = /^ {2,}([\w-]+):\s*(.*)$/.exec(raw)
    if (nested && current) {
      data[current][nested[1]] = coerce(nested[2])
      if (nested[2].trim() === '') list = { key: nested[1], indent }
      continue
    }
    const top = /^([\w-]+):\s*(.*)$/.exec(raw)
    if (!top) continue
    if (top[2] === '') {
      data[top[1]] = {}
      current = top[1]
    } else {
      const block = parseBlockHeader(top[2])
      if (block) {
        const parsed = readBlockScalar(lines, index + 1, block)
        data[top[1]] = parsed.value
        index = parsed.end - 1
      } else {
        data[top[1]] = coerce(top[2])
      }
      current = null
    }
  }
  return { data, body: match[2], hasFrontmatter: true }
}

function parseBlockHeader(value) {
  const match = /^([|>])([1-9+-]{0,2})(?:[ \t]+#.*)?$/.exec(value.trim())
  if (!match) return null
  const modifiers = match[2]
  const indentation = /[1-9]/.exec(modifiers)?.[0]
  const chomping = /[+-]/.exec(modifiers)?.[0] ?? ''
  if (modifiers.replace(/[1-9]/, '').replace(/[+-]/, '') !== '') return null
  if ((modifiers.match(/[1-9]/g)?.length ?? 0) > 1) return null
  if ((modifiers.match(/[+-]/g)?.length ?? 0) > 1) return null
  return {
    style: match[1],
    indentation: indentation ? Number(indentation) : null,
    chomping,
  }
}

function readBlockScalar(lines, start, block) {
  let indentation = block.indentation
  let firstContent = start
  while (firstContent < lines.length && lines[firstContent].trim() === '') firstContent += 1
  if (indentation === null && firstContent < lines.length) {
    const leading = /^ */.exec(lines[firstContent])[0].length
    if (leading > 0) indentation = leading
  }

  let end = start
  while (end < lines.length) {
    const raw = lines[end]
    if (raw.trim() === '') {
      end += 1
      continue
    }
    const leading = /^ */.exec(raw)[0].length
    if (indentation === null || leading < indentation) break
    end += 1
  }

  const contentIndentation = indentation ?? 0
  const content = lines
    .slice(start, end)
    .map((raw) =>
      raw.trim() === ''
        ? raw.slice(Math.min(contentIndentation, raw.length))
        : raw.slice(contentIndentation),
    )
  const rendered =
    block.style === '|'
      ? content.length === 0
        ? ''
        : `${content.join('\n')}\n`
      : foldBlockLines(content)
  return { value: chompBlockScalar(rendered, block.chomping), end }
}

function foldBlockLines(lines) {
  if (lines.length === 0) return ''
  let rendered = ''
  let sawNonEmpty = false
  for (let index = 0; index < lines.length - 1; index += 1) {
    const current = lines[index]
    const next = lines[index + 1]
    rendered += current
    if (current === '') {
      if (next === '' || !sawNonEmpty) rendered += '\n'
    } else {
      sawNonEmpty = true
      rendered += next !== '' && !/^\s/.test(current) && !/^\s/.test(next) ? ' ' : '\n'
    }
  }
  return `${rendered}${lines.at(-1)}\n`
}

function chompBlockScalar(value, chomping) {
  if (chomping === '+') return value
  const stripped = value.replace(/\n+$/, '')
  if (chomping === '-' || stripped === '') return stripped
  return `${stripped}\n`
}

// A quoted YAML scalar is a string by construction, so the quotes must be honoured
// BEFORE the numeric test rather than stripped ahead of it: `tagPrefix: "42"` has to stay
// the string "42", and stripping first would turn it into Number 42, which extensionMarker's
// string guards then drop silently. Only a bare, unquoted integer coerces.
function coerce(value) {
  const v = value.trim()
  const quoted = /^(["'])([\s\S]*)\1$/.exec(v)
  if (quoted) return quoted[2]
  if (/^-?\d+$/.test(v)) return Number(v)
  return v
}
