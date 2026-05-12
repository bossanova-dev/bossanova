.PHONY: all deps format lint test dev clean build typecheck

## all: Clean, format, and test
all: clean format test

## deps: Install dependencies (docs is standalone, not part of the pnpm workspace)
deps:
	pnpm install --ignore-workspace

## format: Format markdown, TypeScript, and CSS via prettier
format:
	pnpm run format

## test: Typecheck and build (build is the strictest test for static docs)
test: typecheck
	pnpm run build

## lint: Typecheck and prettier format check
lint: typecheck
	pnpm run lint

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
