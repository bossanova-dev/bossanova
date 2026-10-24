.PHONY: format format-affected format-all

## format-affected: Format only files changed vs origin/main (+ staged/untracked). Fast local/agent loop.
format-affected:
	@node scripts/lint/format-affected.mjs

## format: Fast affected format (default) — format only the files changed on this
## branch (format-affected: gofmt/goimports on changed .go; prettier/biome on changed
## web/markdown; full Biome packages after workspace dependency changes; syncpack when a
## package.json changed). Fast is the default (BOS-371);
## the whole-tree format is `make format-all`.
format:
	@node scripts/lint/format-affected.mjs

## format-all: Exhaustive whole-tree format — Go (gofmt), web (biome), package.json
## (syncpack), and ALL markdown (prettier). Slow (was `make format`); CI/release and
## BOS-372's sweep are the hard gate.
format-all:
# `set -e` on the multi-command recipe lines (the house form, cf. readme-gifs): a shell
# `for`/`if` block exits with its LAST command's status, so without it a module whose
# gofmt/prettier fails is skipped and `make format-all` still exits 0. bs-sweep-prettify's
# Phase 1 keys its "broken toolchain -> open no PR" abort on this target's exit status, so a
# swallowed failure would have it commit and open a PR for a partially-formatted tree while
# claiming the toolchain was healthy. Structural, not positional: a command added to either
# block later is covered without anyone remembering to append `|| exit 1`. The single-command
# blocks below need nothing — their one command is already the block's exit status.
	@set -e; \
	if command -v pnpm >/dev/null 2>&1 && [ -f package.json ]; then \
		pnpm syncpack format; \
		pnpm syncpack fix; \
	fi
	@set -e; \
	for mod in $(MODULES); do \
		echo "==> Formatting $$mod"; \
		$(MAKE) -C $$mod format; \
	done
	@if [ -d services/web ]; then \
		$(MAKE) -C services/web format; \
	fi
	@if [ -d services/docs ]; then \
		$(MAKE) -C services/docs format; \
	fi
	$(MAKE) -C scripts format
	@if command -v pnpm >/dev/null 2>&1 && [ -f package.json ]; then \
		pnpm run format:docs; \
	fi
