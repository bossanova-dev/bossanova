.PHONY: format test clean generate

## format: Format Go source files and run golangci-lint (catches typecheck errors locally)
format:
	gofmt -w .
	golangci-lint run ./...

## test: Run tests under -race with coverage
test:
	go test -race -timeout 300s -coverprofile=coverage.out ./...

## clean: Remove generated code and copied skills
clean:
	rm -rf gen
	rm -rf skilldata/skills

## generate: Regenerate protobuf code (run from repo root instead)
generate:
	@echo "Run 'make generate' from the repo root (uses buf)"
