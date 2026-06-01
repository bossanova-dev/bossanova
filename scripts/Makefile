.PHONY: all format lint test

## all: Format, lint, and test scripts
all: format lint test

## format: Format script files with Prettier
format:
	cd .. && pnpm exec prettier --write 'scripts/*.{cjs,mjs}'

## lint: Check script syntax
lint:
	@for file in *.cjs *.mjs; do \
		[ -e "$$file" ] || continue; \
		echo "==> Checking scripts/$$file"; \
		node --check "$$file"; \
	done

## test: Run script tests
test:
	node --test *.test.mjs
