// scripts/proof-brief.d.mts — minimal TS declaration for the ONE proof-brief.mjs
// export the web agent needs (BOS-140 P3b/P3c): normalizeScenes. scripts/
// proof-brief.mjs remains the single source of truth for scene normalization;
// this file exists only so strict `tsc -b` (services/web/tsconfig.e2e.json,
// moduleResolution: bundler, no allowJs) can type-check the cross-package
// import from services/web/tests/e2e/agent/{tools,proof.spec}.ts — the same
// `.d.mts`-beside-`.mjs` pairing scripts/proof-caption-spec.d.mts set up for
// the shared caption spec. Extend this file if a future TS consumer needs
// another export; do not duplicate normalizeScenes' logic here.

export declare function normalizeScenes(brief: {
  scenes?: Array<{
    id?: string
    title?: string
    stepsHints?: string[]
    expectedEvidence?: string[]
  }>
  title?: string
  stepsHints?: string[]
  expectedEvidence?: string[]
}): Array<{
  id: string
  title: string
  stepsHints: string[]
  expectedEvidence: string[]
}>
