SERVICES = lib/shared services/cli services/daemon services/webhook services/orchestrator

.PHONY: format lint test build dev clean claude

format:
	@for dir in $(SERVICES); do \
		if [ -f $$dir/Makefile ]; then $(MAKE) -C $$dir format; fi; \
	done

lint:
	@for dir in $(SERVICES); do \
		if [ -f $$dir/Makefile ]; then $(MAKE) -C $$dir lint; fi; \
	done

test:
	@for dir in $(SERVICES); do \
		if [ -f $$dir/Makefile ]; then $(MAKE) -C $$dir test; fi; \
	done

build:
	@for dir in $(SERVICES); do \
		if [ -f $$dir/Makefile ]; then $(MAKE) -C $$dir build; fi; \
	done

clean:
	@for dir in $(SERVICES); do \
		if [ -f $$dir/Makefile ]; then $(MAKE) -C $$dir clean; fi; \
	done

claude:
	claude --dangerously-skip-permissions
