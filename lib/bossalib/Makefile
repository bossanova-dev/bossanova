.PHONY: format test clean generate

## format: Format Go source files
format:
	gofmt -w .

## test: Run tests
test:
	go test ./...

## clean: Remove generated code
clean:
	rm -rf gen

## generate: Regenerate protobuf code (run from repo root instead)
generate:
	@echo "Run 'make generate' from the repo root (uses buf)"
