.PHONY: format test clean generate

## format: Format Go source files and run golangci-lint (catches typecheck errors locally)
format:
	gofmt -w .
	golangci-lint run ./...

# Race detector is opt-in locally (slow). CI sets RACE=1; locally use `RACE=1 make test`.
RACE_FLAG := $(if $(RACE),-race,)

## test: Run tests with coverage (set RACE=1 to enable race detector)
test:
	go test $(RACE_FLAG) -timeout 300s -coverprofile=coverage.out ./...

## clean: Remove generated code and copied skills
clean:
	rm -rf gen
	rm -rf skilldata/skills

## generate: Regenerate protobuf code (run from repo root instead)
generate:
	@echo "Run 'make generate' from the repo root (uses buf)"
