.PHONY: format lint test build dev deploy clean

dev:
	pnpm wrangler dev

format:
	pnpm prettier --write 'src/**/*.ts'

lint:
	pnpm biome check src/

test:
	pnpm vitest run --passWithNoTests

build:
	pnpm tsc --noEmit

deploy:
	pnpm wrangler deploy

clean:
	rm -rf dist .wrangler
