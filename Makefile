.PHONY: generate build test lint clean split format

# Binaries output to bin/
BIN_DIR := bin

# All Go modules in the workspace
MODULES := lib/bossalib services/boss services/bossd services/bosso

## generate: Run buf generate to produce Go code from proto definitions
generate:
	rm -rf lib/bossalib/gen
	buf generate

## build: Build all three binaries
build: $(BIN_DIR)/boss $(BIN_DIR)/bossd $(BIN_DIR)/bosso

$(BIN_DIR)/boss:
	go build -o $(BIN_DIR)/boss ./services/boss/cmd

$(BIN_DIR)/bossd:
	go build -o $(BIN_DIR)/bossd ./services/bossd/cmd

$(BIN_DIR)/bosso:
	go build -o $(BIN_DIR)/bosso ./services/bosso/cmd

## test: Run tests across all modules
test:
	@for mod in $(MODULES); do \
		echo "==> Testing $$mod"; \
		(cd $$mod && go test ./...); \
	done

## lint: Run golangci-lint and buf lint
lint:
	buf lint
	@for mod in $(MODULES); do \
		echo "==> Linting $$mod"; \
		(cd $$mod && golangci-lint run ./...); \
	done

## format: Format Go code and markdown
format:
	@for mod in $(MODULES); do \
		(cd $$mod && gofmt -w .); \
	done
	pnpm run format:docs

## clean: Remove build artifacts and generated code
clean:
	rm -rf $(BIN_DIR)
	rm -rf lib/bossalib/gen

## split: Mirror subtrees to separate repos via splitsh/lite
split:
	splitsh-lite --prefix=proto --target=refs/heads/split/proto
	splitsh-lite --prefix=lib/bossalib --target=refs/heads/split/bossalib
	splitsh-lite --prefix=services/boss --target=refs/heads/split/boss
	splitsh-lite --prefix=services/bossd --target=refs/heads/split/bossd
