# Shared Go test rules for module Makefiles.
# Full module tests keep coverage. Fast agent paths set SHORT=1 COVERAGE=0.

GO_TEST_TIMEOUT ?= 300s
GO_TEST_PACKAGES ?= ./...
GO_TEST_RUN ?=
GO_TEST_COUNT ?=
GO_TEST_COVERAGE_PROFILE ?= coverage.out
GO_TEST_MAKEFILE ?= $(firstword $(MAKEFILE_LIST))
GO_TEST_DIR ?= $(dir $(GO_TEST_MAKEFILE))

RACE_FLAG := $(if $(RACE),-race,)
SHORT_FLAG := $(if $(SHORT),-short,)
RUN_FLAG := $(if $(GO_TEST_RUN),-run '$(GO_TEST_RUN)',)
COUNT_FLAG := $(if $(GO_TEST_COUNT),-count=$(GO_TEST_COUNT),)
COVERAGE_FLAG := $(if $(filter 1 true yes on,$(COVERAGE)),-coverprofile=$(GO_TEST_COVERAGE_PROFILE),)

.PHONY: test test-fast go-test

test:
	$(MAKE) -f $(GO_TEST_MAKEFILE) go-test COVERAGE=1

test-fast:
	$(MAKE) -f $(GO_TEST_MAKEFILE) go-test SHORT=1 COVERAGE=0

go-test:
	cd "$(GO_TEST_DIR)" && go test $(RACE_FLAG) $(SHORT_FLAG) $(RUN_FLAG) $(COUNT_FLAG) -timeout $(GO_TEST_TIMEOUT) $(COVERAGE_FLAG) $(GO_TEST_PACKAGES)
