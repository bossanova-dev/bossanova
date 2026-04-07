.PHONY: all clean format build test

PLUGINS := bossd-plugin-autopilot bossd-plugin-dependabot bossd-plugin-linear

all: clean format build test

format:
	@for plugin in $(PLUGINS); do \
		echo "==> Formatting $$plugin"; \
		$(MAKE) -C $$plugin format; \
	done

build:
	@for plugin in $(PLUGINS); do \
		echo "==> Building $$plugin"; \
		$(MAKE) -C $$plugin build; \
	done

test:
	@for plugin in $(PLUGINS); do \
		echo "==> Testing $$plugin"; \
		$(MAKE) -C $$plugin test; \
	done

clean:
	@for plugin in $(PLUGINS); do \
		$(MAKE) -C $$plugin clean; \
	done
