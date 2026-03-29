You are analyzing surviving mutants from Go mutation testing.
You MUST use your tools (Read, Edit, Write, Bash) to modify files directly.
Do NOT just describe changes — actually edit the test files on disk.

Each line of STDIN describes a surviving mutant:
[module] file:line MUTATION_TYPE

For each surviving mutant:

1. Use the Read tool to read the source file at the given line
2. Understand what mutation would survive (see types below)
3. Use the Read tool to read existing tests for that package
4. Use the Edit or Write tool to add a test that catches the boundary

Follow existing test conventions in this repo:

- Standard library `testing` package (no testify)
- Table-driven tests with `[]struct{ name string; ... }` + `t.Run`
- `t.Helper()` on helper functions
- Hand-written mocks (no code generation)

MUTATION TYPES:

- CONDITIONALS_BOUNDARY: `<` changed to `<=`, `>` to `>=`, etc.
- CONDITIONALS_NEGATION: `==` changed to `!=`, `<` to `>=`, etc.
- ARITHMETIC_BASE: `+` changed to `-`, `*` to `/`, etc.
- INCREMENT_DECREMENT: `++` changed to `--`, etc.
- INVERT_NEGATIVES: `-x` changed to `x`
- INVERT_LOGICAL: `&&` changed to `||`, etc.

Prioritize business logic over utility code. Group tests by file.

After writing tests for all surviving mutants:

1. Run `go test ./...` in each affected module to confirm all tests pass
2. If any test fails, fix it and re-run until green
3. Commit the changes with: `git add -A && git commit -m "test(mutate): add tests to kill surviving mutants"`
