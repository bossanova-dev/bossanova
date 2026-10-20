.PHONY: mutate mutate-coverage mutate-diff mutate-fix mutate-loop mutate-pkg mutate-report mutate-survivors mutate-uncovered

# --- Mutation Testing ---------------------------------------------------
# NOTE: gremlins ./... silently produces no results in Go workspaces.
# We work around this by iterating over individual packages per module.
# We also skip packages with no test files to avoid "go: no such tool covdata"
# errors from gremlins on Go 1.25+ (covdata moved out of GOROOT/pkg/tool/).
MUTATE_TIMEOUT := 30

## mutate: Run mutation testing across all modules
mutate: $(GEN_STAMP)
	@mkdir -p $(MUTATE_DIR)
	@root=$$(git rev-parse --show-toplevel); \
	failed=0; \
	for mod in $(MODULES); do \
		echo "==> Mutating $$mod"; \
		modname=$$(basename $$mod); \
		modabs=$$(cd $$mod && pwd); \
		for pkg in $$(cd $$mod && go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./...); do \
			pkgdir=$$(cd $$mod && go list -f '{{.Dir}}' "$$pkg"); \
			reldir=$${pkgdir#$$modabs/}; \
			[ "$$reldir" = "$$pkgdir" ] && reldir="."; \
			if [ "$$reldir" = "." ]; then safename="root"; else safename=$$(echo "$$reldir" | tr '/' '-'); fi; \
			echo "    -> $$modname/$$reldir"; \
			(cd $$mod && gremlins unleash \
				-o "$$root/$(MUTATE_DIR)/$$modname--$$safename.json" \
				--timeout-coefficient $(MUTATE_TIMEOUT) \
				--workers 0 \
				"./$$reldir") || failed=1; \
		done; \
	done; \
	echo ""; \
	echo "==> Reports in $(MUTATE_DIR)/"; \
	echo "==> Run 'make mutate-report' for summary"; \
	if [ "$$failed" = "1" ]; then exit 1; fi

## mutate-diff: Mutation testing on changed code only (fast, for PRs)
mutate-diff: $(GEN_STAMP)
	@mkdir -p $(MUTATE_DIR)
	@root=$$(git rev-parse --show-toplevel); \
	for mod in $(MODULES); do \
		echo "==> Mutating changed code in $$mod"; \
		modname=$$(basename $$mod); \
		modabs=$$(cd $$mod && pwd); \
		for pkg in $$(cd $$mod && go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./...); do \
			pkgdir=$$(cd $$mod && go list -f '{{.Dir}}' "$$pkg"); \
			reldir=$${pkgdir#$$modabs/}; \
			[ "$$reldir" = "$$pkgdir" ] && reldir="."; \
			if [ "$$reldir" = "." ]; then safename="root"; else safename=$$(echo "$$reldir" | tr '/' '-'); fi; \
			(cd $$mod && gremlins unleash \
				--diff main \
				-o "$$root/$(MUTATE_DIR)/$$modname--$$safename.json" \
				--timeout-coefficient $(MUTATE_TIMEOUT) \
				--workers 0 \
				"./$$reldir") || true; \
		done; \
	done

## mutate-report: Summarize mutation testing results
mutate-report:
	@echo "=== Mutation Testing Summary ==="
	@for f in $(MUTATE_DIR)/*.json; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f" .json); \
		echo ""; \
		echo "--- $$name ---"; \
		jq -r '"  Efficacy:     \(.test_efficacy // "n/a")%\n  Coverage:     \(.mutations_coverage // "n/a")%\n  Total:        \(.mutants_total // 0)\n  Killed:       \(.mutants_killed // 0)\n  Lived:        \(.mutants_lived // 0)\n  Not covered:  \(.mutants_not_covered // 0)"' "$$f" 2>/dev/null \
			|| echo "  (no results)"; \
	done
	@echo ""
	@echo "==> Surviving mutants: make mutate-survivors"

## mutate-survivors: List surviving mutants (for LLM consumption)
mutate-survivors:
	@for f in $(MUTATE_DIR)/*.json; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f" .json); \
		jq -r --arg mod "$$name" \
			'.files[]? | .file_name as $$file | .mutations[]? | select(.status == "LIVED") | "[\($$mod)] \($$file):\(.line) \(.type)"' \
			"$$f" 2>/dev/null; \
	done

## mutate-uncovered: List NOT_COVERED mutants (untested code, for coverage work)
mutate-uncovered:
	@for f in $(MUTATE_DIR)/*.json; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f" .json); \
		jq -r --arg mod "$$name" \
			'.files[]? | .file_name as $$file | .mutations[]? | select(.status == "NOT_COVERED" or .status == "NOT COVERED") | "[\($$mod)] \($$file):\(.line) \(.type)"' \
			"$$f" 2>/dev/null; \
	done

## mutate-coverage: Rank packages by mutation coverage, lowest first.
## Columns (tab-separated): coverage%  not_covered  package
mutate-coverage:
	@for f in $(MUTATE_DIR)/*.json; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f" .json); \
		jq -r --arg mod "$$name" \
			'"\(.mutations_coverage // 0)\t\(.mutants_not_covered // 0)\t\($$mod)"' \
			"$$f" 2>/dev/null; \
	done | sort -n

## mutate-fix: Feed surviving mutants to Claude Code to generate tests
mutate-fix:
	@mkdir -p $(MUTATE_DIR)
	@$(MAKE) --no-print-directory mutate-survivors > $(MUTATE_DIR)/survivors.txt 2>/dev/null
	@count=$$(wc -l < $(MUTATE_DIR)/survivors.txt | tr -d ' '); \
	if [ "$$count" = "0" ]; then \
		echo "No surviving mutants. Run 'make mutate' first."; \
		exit 0; \
	fi; \
	echo "==> $$count surviving mutants found"; \
	echo "==> Launching Claude Code to generate tests..."; \
	cat $(MUTATE_DIR)/survivors.txt | claude -p --dangerously-skip-permissions "$$(cat .claude/prompts/mutate-fix.md)"

## mutate-loop: Full cycle — mutate, fix survivors, verify
mutate-loop:
	@$(MAKE) mutate
	@$(MAKE) mutate-fix
	@echo ""
	@echo "==> Verifying fixes..."
	@$(MAKE) mutate
	@$(MAKE) mutate-report

## Per-module mutation targets
define run-mutate-module
	@mkdir -p $(MUTATE_DIR)
	@root=$$(git rev-parse --show-toplevel); \
	modabs=$$(cd $(1) && pwd); \
	for pkg in $$(cd $(1) && go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./...); do \
		pkgdir=$$(cd $(1) && go list -f '{{.Dir}}' "$$pkg"); \
		reldir=$${pkgdir#$$modabs/}; \
		[ "$$reldir" = "$$pkgdir" ] && reldir="."; \
		if [ "$$reldir" = "." ]; then safename="root"; else safename=$$(echo "$$reldir" | tr '/' '-'); fi; \
		echo "==> $(2)/$$reldir"; \
		(cd $(1) && gremlins unleash \
			-o "$$root/$(MUTATE_DIR)/$(2)--$$safename.json" \
			--timeout-coefficient $(3) \
			--workers 0 \
			"./$$reldir") || true; \
	done
endef

## mutate-pkg: Mutation-test a single package (fast, for targeted verification).
## Usage: make mutate-pkg MODULE=services/bosso PKG=./internal/auth
## MODNAME defaults to $(notdir MODULE), matching the full `mutate` target's
## JSON naming (e.g. plugins stay bossd-plugin-claude); override only to retarget.
mutate-pkg:
	@test -n "$(MODULE)" || { echo "MODULE is required (e.g. MODULE=services/bosso)"; exit 2; }
	@test -n "$(PKG)" || { echo "PKG is required (e.g. PKG=./internal/auth)"; exit 2; }
	@$(MAKE) --no-print-directory $(GEN_STAMP)
	@mkdir -p $(MUTATE_DIR)
	@root=$$(git rev-parse --show-toplevel); \
	modname="$(MODNAME)"; [ -n "$$modname" ] || modname=$$(basename "$(MODULE)"); \
	reldir=$$(echo "$(PKG)" | sed -e 's|^\./||' -e 's|/$$||'); \
	if [ -z "$$reldir" ] || [ "$$reldir" = "." ]; then safename="root"; else safename=$$(echo "$$reldir" | tr '/' '-'); fi; \
	echo "==> $$modname/$$reldir"; \
	(cd "$(MODULE)" && gremlins unleash \
		-o "$$root/$(MUTATE_DIR)/$$modname--$$safename.json" \
		--timeout-coefficient $(MUTATE_TIMEOUT) \
		--workers 0 \
		"$(PKG)")

mutate-bossalib:
	$(call run-mutate-module,lib/bossalib,bossalib,$(MUTATE_TIMEOUT))

mutate-boss:
	$(call run-mutate-module,services/boss,boss,$(MUTATE_TIMEOUT))

mutate-bossd:
	$(call run-mutate-module,services/bossd,bossd,$(MUTATE_TIMEOUT))

ifneq ($(wildcard services/bosso/go.mod),)
mutate-bosso:
	$(call run-mutate-module,services/bosso,bosso,$(MUTATE_TIMEOUT))
endif

# Auto-generate per-plugin mutate targets from detected modules
define define-plugin-mutate
mutate-$(2):
	$$(call run-mutate-module,$(1),$(notdir $(1)),$$(MUTATE_TIMEOUT))
endef
$(foreach p,$(PLUGIN_MODULES),$(eval \
  $(call define-plugin-mutate,$(p),$(patsubst bossd-plugin-%,%,$(notdir $(p))))))
