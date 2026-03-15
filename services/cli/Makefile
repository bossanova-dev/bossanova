.PHONY: format lint test build dev clean

dev:
	pnpm tsx src/cli.tsx

format:
	pnpm prettier --write 'src/**/*.{ts,tsx}'

lint:
	pnpm biome check src/

test:
	pnpm vitest run --passWithNoTests

build:
	pnpm tsc --noEmit

clean:
	rm -rf dist
