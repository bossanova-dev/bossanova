.PHONY: architecture-metrics

## architecture-metrics: Measure architecture and print its summary (JSON and markdown in TMPDIR)
architecture-metrics:
	@node scripts/architecture/metrics.mjs --json "$${TMPDIR:-/tmp}/architecture-metrics.json" --markdown "$${TMPDIR:-/tmp}/architecture-metrics.md"
	@cat "$${TMPDIR:-/tmp}/architecture-metrics.md"
