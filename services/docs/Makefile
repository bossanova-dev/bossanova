.PHONY: all deps format lint lint-prose test dev clean build typecheck

## all: Clean, format, and test
all: clean format test

## deps: Install dependencies (docs is its own workspace root, not part of the
## root pnpm workspace). pnpm-workspace.yaml here stops pnpm's upward search at
## services/docs and carries the overrides/minimumReleaseAge/allowBuilds this
## install needs; --ignore-workspace would discard it.
deps:
	pnpm install

## format: Format markdown, TypeScript, and CSS via prettier
format:
	pnpm run format

## test: Typecheck, run unit tests, build static docs, then check the build's
## inline scripts against the static/_headers CSP (scripts/headers.test.mjs)
test: typecheck
	pnpm run test:unit
	pnpm run build
	pnpm run test:headers

## lint: Typecheck and prettier format check
lint: typecheck
	pnpm run lint

## lint-prose: Check Vale rules and the em-dash density ratchet
lint-prose:
	pnpm run lint:prose

## typecheck: Run TypeScript without emitting
typecheck:
	pnpm run typecheck

## dev: Start the Docusaurus dev server on :3001
dev:
	pnpm run dev

## build: Production build
build:
	pnpm run build

## clean: Remove build artifacts
clean:
	rm -rf build .docusaurus
