.PHONY: format lint test build dev clean

dev:
	pnpm tsx src/index.ts

format:
	pnpm prettier --write 'src/**/*.ts'

lint:
	pnpm biome check src/

test:
	pnpm vitest run --passWithNoTests

build:
	pnpm tsc --noEmit

clean:
	rm -rf dist
