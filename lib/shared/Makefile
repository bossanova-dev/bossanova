.PHONY: format lint test build clean

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
