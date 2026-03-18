.PHONY: all clean generate format build test

SERVICES := boss bossd bosso web

all: clean generate format build test

generate:
	$(MAKE) -C .. generate

format:
	@for svc in $(SERVICES); do \
		echo "==> Formatting $$svc"; \
		$(MAKE) -C $$svc format; \
	done

build:
	@for svc in $(SERVICES); do \
		echo "==> Building $$svc"; \
		$(MAKE) -C $$svc build; \
	done

test:
	@for svc in $(SERVICES); do \
		echo "==> Testing $$svc"; \
		$(MAKE) -C $$svc test; \
	done

clean:
	@for svc in $(SERVICES); do \
		$(MAKE) -C $$svc clean; \
	done
