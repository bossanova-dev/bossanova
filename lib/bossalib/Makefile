.PHONY: format test clean generate

## format: Format Go source files (gofmt).
format:
	gofmt -w .

GO_TEST_MAKEFILE := $(lastword $(MAKEFILE_LIST))
include $(dir $(lastword $(MAKEFILE_LIST)))../../mk/go-test.mk

## clean: Remove generated Go output and copied skills
clean:
	# Delete buf's generated Go output but preserve the committed Gazelle
	# BUILD.bazel package files (`rm -rf gen` would drop them and trip the
	# generate drift gate + break `bazel build`). Tolerate an already-missing dir.
	find gen -type f ! -name 'BUILD.bazel' -delete 2>/dev/null || true
	rm -rf skilldata/skills

## generate: Regenerate protobuf code (run from repo root instead)
generate:
	@echo "Run 'make generate' from the repo root (uses buf)"
