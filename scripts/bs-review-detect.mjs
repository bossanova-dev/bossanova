// Pure helpers for the bs-review skill: classify a changed-file list and pick
// the cross-agent "second voice". Node built-ins only (cron worktrees are
// dependency-free).

/**
 * Classify changed files into review lenses.
 * - go:  any *.go file changed                -> golang-pro lens
 * - tui: any file under services/boss/        -> tui-design lens (Bubbletea TUI)
 * - web: any file under services/web/         -> impeccable lens (React/web UI)
 * @param {string[]} files
 * @returns {{go: boolean, tui: boolean, web: boolean}}
 */
export function detectChangeTypes(files) {
  const list = Array.isArray(files) ? files : []
  return {
    go: list.some((f) => f.endsWith('.go')),
    tui: list.some((f) => f.startsWith('services/boss/')),
    web: list.some((f) => f.startsWith('services/web/')),
  }
}

/**
 * Pick the opposite agent for the second-opinion round.
 * Anything that is not "codex" yields "codex" (claude is the common host).
 * @param {string} currentAgent
 * @returns {"codex"|"claude"}
 */
export function secondVoiceAgent(currentAgent) {
  return currentAgent === 'codex' ? 'claude' : 'codex'
}

// Thin CLI: `node scripts/bs-review-detect.mjs <newline-separated-files-on-stdin>`
// prints `{ "go":..., "tui":..., "web":... }` for shell consumption. Optional
// flag `--second-voice <agent>` prints the opposite agent name instead.
if (import.meta.url === `file://${process.argv[1]}`) {
  const argv = process.argv.slice(2)
  const svIdx = argv.indexOf('--second-voice')
  if (svIdx !== -1) {
    process.stdout.write(secondVoiceAgent(argv[svIdx + 1] ?? '') + '\n')
  } else if (process.stdin.isTTY) {
    // No piped file list and no --second-voice flag: an interactive TTY would
    // otherwise block forever waiting for stdin 'end'. Treat it as an empty
    // change set so the caller gets a deterministic result instead of a hang.
    process.stdout.write(JSON.stringify(detectChangeTypes([])) + '\n')
  } else {
    const chunks = []
    process.stdin.on('data', (c) => chunks.push(c))
    process.stdin.on('end', () => {
      const files = chunks
        .join('')
        .split('\n')
        .map((s) => s.trim())
        .filter(Boolean)
      process.stdout.write(JSON.stringify(detectChangeTypes(files)) + '\n')
    })
  }
}
