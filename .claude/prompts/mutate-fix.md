You are analyzing surviving mutants from Go mutation testing.

Each line of STDIN describes a surviving mutant:
[module] file:line MUTATION_TYPE

For each surviving mutant:

1. Read the source file at the given line
2. Understand what mutation would survive (see types below)
3. Read existing tests for that package
4. Write a test (or extend an existing one) that catches the boundary

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
Run `go test ./...` in the relevant module after writing each test to verify it passes.
