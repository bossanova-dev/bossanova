// scripts/copy-public-skills.mjs — package the constructed boss-* dev skills into
// the embedded skillinstall payload so the boss binary ships them to end users
// alongside boss-repair/boss-finalize. Copies each .claude/skills/boss-<s> tree
// (minus the build inputs construct.json/SKILL.md.tmpl) into
// services/boss/internal/skillinstall/skills/boss-<s>; --check fails on drift.
// Mirrors the sync-codex-skills / vendor-toolbox drift-gate idiom. Node builtins only.
import fs from 'node:fs'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

export const PUBLIC_SKILLS = [
  'boss-plan',
  'boss-implement',
  'boss-review',
  'boss-epic',
  'boss-proof',
]
// Build inputs that must NOT ship to public consumers (mirror of sync-codex-skills.mjs's
// CONSTRUCT_BUILD_INPUTS): the constructed SKILL.md is shipped, its inputs are not.
export const BUILD_INPUTS = new Set(['construct.json', 'SKILL.md.tmpl'])
const DEST_REL = 'services/boss/internal/skillinstall/skills'

function collect(srcDir, rel, out, keepBuildInputs = false) {
  for (const entry of fs.readdirSync(srcDir, { withFileTypes: true })) {
    if (!keepBuildInputs && BUILD_INPUTS.has(entry.name)) continue
    const abs = path.join(srcDir, entry.name)
    const relPath = rel ? `${rel}/${entry.name}` : entry.name
    if (entry.isDirectory()) collect(abs, relPath, out, keepBuildInputs)
    else if (entry.isFile())
      out.set(relPath, { data: fs.readFileSync(abs), mode: fs.statSync(abs).mode & 0o777 })
  }
  return out
}

export function copyPublicSkills({ rootDir, check }) {
  // Public-mirror checkouts strip .claude/skills entirely (mirror-public.yml), so
  // --check would throw ENOENT reading each skill source and break `make test`
  // there. Skip when the skills root is absent, mirroring vendor-toolbox-check's
  // skip when its source root has been stripped. Only relevant to --check: `make
  // copy-public-skills` (write mode) runs in a full checkout where the root exists.
  if (check && !fs.existsSync(path.join(rootDir, '.claude', 'skills'))) {
    return
  }
  let drift = false
  for (const skill of PUBLIC_SKILLS) {
    const srcDir = path.join(rootDir, '.claude', 'skills', skill)
    const destDir = path.join(rootDir, DEST_REL, skill)
    const wanted = collect(srcDir, '', new Map())

    if (check) {
      // Collect the destination WITH build inputs so a stale construct.json or
      // SKILL.md.tmpl leaked into the embedded tree surfaces as an extra entry
      // (have.size > wanted.size) and trips drift — write mode never emits them,
      // so a leaked build input can only mean a hand-edited/stale embedded copy.
      const have = fs.existsSync(destDir) ? collect(destDir, '', new Map(), true) : new Map()
      const same =
        have.size === wanted.size &&
        [...wanted].every(
          ([rel, f]) =>
            have.get(rel) && have.get(rel).data.equals(f.data) && have.get(rel).mode === f.mode,
        )
      if (!same) {
        drift = true
        console.error(
          `copy-public-skills drift: ${skill} embedded copy is stale — run "make copy-public-skills"`,
        )
      }
      continue
    }

    fs.rmSync(destDir, { force: true, recursive: true })
    for (const [rel, f] of wanted) {
      const dest = path.join(destDir, rel)
      fs.mkdirSync(path.dirname(dest), { recursive: true })
      fs.writeFileSync(dest, f.data, { mode: f.mode })
      fs.chmodSync(dest, f.mode) // preserve the +x bit (e.g. toolbox/worktree-lock.sh)
    }
    console.log(`copied public skill ${skill}`)
  }
  if (check && drift) throw new Error('copy-public-skills: embedded skills are stale')
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const args = process.argv.slice(2)
  const rootIdx = args.indexOf('--root')
  copyPublicSkills({
    rootDir: rootIdx === -1 ? process.cwd() : args[rootIdx + 1],
    check: args.includes('--check'),
  })
}
