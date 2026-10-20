.PHONY: codex-skills codex-skills-check release-codex-check skills-check vendor-toolbox vendor-toolbox-check

codex-skills:
	node scripts/sync-codex-skills.mjs --root .

codex-skills-check:
	node scripts/sync-codex-skills.mjs --root . --check

## vendor-toolbox: Copy the canonical skills-toolbox/ helpers into every committed vendored
## root in one command: the repo-local .claude/skills/ copies and the embedded skillpayload
## tree that both the boss CLI and bossd-plugin-claude embed.
vendor-toolbox:
	node scripts/vendor-toolbox.mjs

## vendor-toolbox-check: Fail if any vendored skill toolbox has drifted.
vendor-toolbox-check:
	node scripts/vendor-toolbox.mjs --check
## skills-check: Build boss and fail if installed skill trees, binary embed, or checkout
## sources disagree. Fail-closed per-machine/local/handoff gate; cannot run in CI.
skills-check: build-boss
	# Read-only checks can select an authenticated checkout without the install/sync write opt-in.
	./bin/boss skills check

## release-codex-check: Verify the codex plugin builds for every distribution
## platform. Invoked from CI (and by humans before merging codex changes) so
## the release workflow never fails on a missing GOOS/GOARCH cross-build. The
## per-platform set mirrors PLATFORMS plus linux/arm64 (added explicitly here
## because the broader PLATFORMS list will catch up in a future commit).
##
## Deliberately does NOT depend on $(GEN_STAMP). The cross-build only needs
## the committed proto-generated code under lib/bossalib/gen/, which is
## already on disk in any clean checkout. Pulling GEN_STAMP would chain in
## $(WEB_DEPS_STAMP) (pnpm install for protoc-gen-es) — and CI for the
## codex plugin doesn't install pnpm, which would fail this gate at the
## point of the dependency resolution rather than the actual build.
RELEASE_CODEX_PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
release-codex-check:
	@for platform in $(RELEASE_CODEX_PLATFORMS); do \
		os=$${platform%%/*}; \
		arch=$${platform##*/}; \
		echo "==> Building bossd-plugin-codex ($$os/$$arch)"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' \
			-o $(BIN_DIR)/bossd-plugin-codex-$$os-$$arch ./plugins/bossd-plugin-codex \
			|| { echo "FAIL: bossd-plugin-codex did not build for $$os/$$arch"; exit 1; }; \
	done
	@echo "==> bossd-plugin-codex builds clean across $(RELEASE_CODEX_PLATFORMS)"
