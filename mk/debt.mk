.PHONY: debt-knip

## Technical-debt scanners (make debt-<kind>-<module>) — narrow, per-module, NON-BLOCKING
## detectors used by the bs-sweep-debt skill. Each auto-fetches its pinned tool via
## `go run`. `<module>` is the short name: bossalib, boss, bossd, bosso, or a plugin short
## name (claude, codex, dependabot, linear, repair, sentry). Examples:
##   make debt-deadcode-bossd   # unreachable code (whole-program, includes tests)
##   make debt-dupl-bossalib    # duplicated token sequences within the module
##   make debt-cyclo-boss       # functions over the cyclomatic-complexity threshold
##   make debt-vuln-bosso       # reachable known vulnerabilities (govulncheck)
##   make debt-filesize-boss    # files over the effective-line limit (revive file-length-limit)
# Libraries have no executable roots of their own: include workspace consumers
# in reachability analysis, then restrict reported packages to the library. This
# loads more packages; uncalled exported APIs and their helpers can still appear.
# Exported-symbol guardrails and the fix compile gate reject unsafe removals.
define define-debt-targets
debt-deadcode-$(2):
ifneq ($(filter lib/%,$(1)),)
	go run $$(DEADCODE_PKG) -test -filter '^$(subst .,\.,$(shell sed -n 's/^module[[:space:]][[:space:]]*//p' $(1)/go.mod))(/|$$$$)' $(addsuffix /...,$(addprefix ./,$(MODULES)))
else
	cd $(1) && go run $$(DEADCODE_PKG) -test ./...
endif
debt-dupl-$(2):
	cd $(1) && go run $$(DUPL_PKG) -t $$(DEBT_DUPL_THRESHOLD) .
debt-cyclo-$(2):
	cd $(1) && go run $$(GOCYCLO_PKG) -over $$(DEBT_CYCLO_THRESHOLD) .
debt-vuln-$(2):
	cd $(1) && go run $$(GOVULNCHECK_PKG) ./...
# File-size (not function-size) hotspots. revive only takes rule arguments from a
# config file, so DEBT_FILESIZE_THRESHOLD is sed'd over the committed default into
# a temp copy. `|| true` keeps the DETECTOR RUN strictly non-blocking even if a
# future revive changes its exit-status behaviour on findings — but the config
# build is deliberately NOT tolerant: revive given an empty/ruleless config prints
# nothing and exits 0, which is indistinguishable from "this module is clean" —
# and `|| true` would swallow a config it cannot parse at all. Worse, revive
# DISABLES file-length-limit when max <= 0, so a bare `0` would silence the
# detector while looking green. So a DEBT_FILESIZE_THRESHOLD that is not a
# positive integer without a leading zero (`0`, `00`, `0800`, `abc`), a failed
# sed, or a generated config that lost the rule hard-fails loudly rather than
# reporting a silent all-clear. `trap` removes the temp copy on interrupt too.
debt-filesize-$(2):
	@case "$$(DEBT_FILESIZE_THRESHOLD)" in ''|*[!0-9]*|0*) \
	    echo "debt-filesize: DEBT_FILESIZE_THRESHOLD must be a positive integer with no leading zero, got '$$(DEBT_FILESIZE_THRESHOLD)'" >&2; exit 1;; esac; \
	  cfg=$$$$(mktemp -t revive-filesize.XXXXXX) || exit 1; \
	  trap 'rm -f "$$$$cfg"' EXIT INT TERM; \
	  sed 's/\(max = \)[0-9][0-9]*/\1$$(DEBT_FILESIZE_THRESHOLD)/' \
	    $$(CURDIR)/scripts/debt/revive-filesize.toml > "$$$$cfg" || \
	    { echo "debt-filesize: cannot build config from $$(CURDIR)/scripts/debt/revive-filesize.toml" >&2; exit 1; }; \
	  grep -q '^\[rule\.file-length-limit\]' "$$$$cfg" && grep -qE "max = $$(DEBT_FILESIZE_THRESHOLD)[,}[:space:]]" "$$$$cfg" || \
	    { echo "debt-filesize: generated config lost the file-length-limit rule or the requested max; detector would be silently inert" >&2; exit 1; }; \
	  (cd $(1) && go run $$(REVIVE_PKG) -config "$$$$cfg" -formatter default ./...) || true
endef
$(foreach m,$(MODULES),$(eval \
  $(call define-debt-targets,$(m),$(patsubst bossd-plugin-%,%,$(notdir $(m))))))

## debt-knip: Report unused TS exports/files/deps in services/web (non-blocking detector).
debt-knip:
	$(MAKE) -C services/web knip
