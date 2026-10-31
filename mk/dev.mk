.PHONY: clean kill kill-all

claude:
	claude --dangerously-skip-permissions

codex:
	codex --dangerously-bypass-approvals-and-sandbox

## clean-cache: Prune the Go build cache at GO_CACHE_MAX_GIB (100 GiB default).
clean-cache:
	node scripts/dev/clean-go-build-cache.mjs

## recover-disk-space: Reclaim disk from build and test leftovers (idle worktrees and their
## Bazel output bases, Go and golangci-lint caches, old logs, stale temp dirs, package-manager
## caches; see scripts/dev/recover-disk-space.mjs, which takes --dry-run), Docker caches,
## ALL WhatsApp Desktop media (quits WhatsApp; chat text survives) and downloaded
## macOS aerial wallpapers (re-downloaded on demand).
WHATSAPP_MEDIA := $(HOME)/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/Message/Media
AERIAL_VIDEOS := $(HOME)/Library/Application Support/com.apple.wallpaper/aerials/videos
recover-disk-space:
	node scripts/dev/recover-disk-space.mjs
	@if docker info >/dev/null 2>&1; then \
		docker image prune -a; \
		docker builder prune -a -f; \
		for b in $$(docker buildx ls --format json | grep '"Status":"running"' | sed -n 's/.*"Name":"\([^"]*\)","Nodes".*/\1/p'); do \
			docker buildx prune -a -f --builder "$$b" || true; \
		done; \
		docker volume prune -f; \
	else echo "==> Docker not running; skipping Docker cleanup"; fi
	@if pgrep -x WhatsApp >/dev/null; then osascript -e 'tell application id "net.whatsapp.WhatsApp" to quit'; sleep 3; fi
	-rm -rf "$(WHATSAPP_MEDIA)"/*
	rm -f "$(AERIAL_VIDEOS)"/*.mov
	-killall WallpaperAgent 2>/dev/null

## clean: Remove build artifacts and generated code
clean:
	rm -rf $(BIN_DIR)
	rm -f $(GEN_STAMP)
	rm -rf $(MUTATE_DIR)
	@for mod in $(MODULES); do \
		$(MAKE) -C $$mod clean; \
	done
	@if [ -d services/web ]; then \
		$(MAKE) -C services/web clean; \
	fi
	@if [ -d services/docs ]; then \
		$(MAKE) -C services/docs clean; \
	fi

## kill: Stop all local boss dev processes for this repo (daemon, plugins, web/FE)
kill:
	@echo "==> Killing boss dev processes for $(CURDIR)"
	@pids=""; \
	add() { for p in $$1; do case " $$pids " in *" $$p "*) ;; *) pids="$$pids $$p" ;; esac; done; }; \
	for sock in \
		"$(CURDIR)/.config/bossd.sock" \
		"$$HOME/Library/Application Support/bossanova/bossd.sock" \
		"$$HOME/.config/bossanova/bossd.sock"; do \
		[ -S "$$sock" ] && add "$$(lsof -t "$$sock" 2>/dev/null)"; \
	done; \
	add "$$(pgrep -f '$(CURDIR)/bin/boss' 2>/dev/null)"; \
	for d in services/web services/marketing services/bosso/web services/docs; do \
		add "$$(pgrep -f "$(CURDIR)/$$d" 2>/dev/null)"; \
	done; \
	for p in $$(pgrep -f '/go-build.*/exe/' 2>/dev/null); do \
		lsof -p $$p 2>/dev/null | grep -qF '$(CURDIR)' && add "$$p"; \
	done; \
	self=$$$$; pids="$$(printf '%s\n' $$pids | sort -un | grep -vx $$self || true)"; \
	if [ -z "$$pids" ]; then echo "  nothing running"; exit 0; fi; \
	for p in $$pids; do ps -o pid=,command= -p $$p 2>/dev/null | sed 's/^/  kill /'; done; \
	kill $$pids 2>/dev/null || true; \
	sleep 1; \
	still=""; for p in $$pids; do kill -0 $$p 2>/dev/null && still="$$still $$p"; done; \
	[ -n "$$still" ] && { echo "  force -9:$$still"; kill -9 $$still 2>/dev/null || true; } || true; \
	echo "==> Done."

## kill-all: Stop EVERY local boss process on this machine — across ALL worktrees
## AND the main checkout. Unlike `kill` (scoped to $(CURDIR)), this matches by
## EXECUTABLE basename + socket ownership, never by argv text, so it (a) catches
## daemons started from another repo dir and (b) will NOT false-kill an unrelated
## process whose arguments merely mention a boss path (e.g. an agent carrying a
## prompt). WARNING: this also stops boss daemons/plugins owned by OTHER running
## worktrees (e.g. concurrent cron sessions). Use `make kill` for just this repo.
kill-all:
	@echo "==> Killing ALL boss processes on this machine (every worktree + main checkout)"
	@pids=""; \
	add() { for p in $$1; do case " $$pids " in *" $$p "*) ;; *) pids="$$pids $$p" ;; esac; done; }; \
	add "$$(ps -axo pid=,comm= | awk '$$2 ~ /\/(boss|bossd|bosso|bossd?-plugin-[a-z0-9-]+)$$/ {print $$1}')"; \
	for p in $$(pgrep -x cmd 2>/dev/null); do \
		lsof -p $$p 2>/dev/null | grep -qiE 'boss[od]\.(sock|db)' && add "$$p"; \
	done; \
	self=$$$$; pids="$$(printf '%s\n' $$pids | sort -un | grep -vx $$self || true)"; \
	if [ -z "$$pids" ]; then echo "  nothing running"; exit 0; fi; \
	for p in $$pids; do ps -o pid=,command= -p $$p 2>/dev/null | cut -c1-100 | sed 's/^/  kill /'; done; \
	kill $$pids 2>/dev/null || true; \
	sleep 1; \
	still=""; for p in $$pids; do kill -0 $$p 2>/dev/null && still="$$still $$p"; done; \
	[ -n "$$still" ] && { echo "  force -9:$$still"; kill -9 $$still 2>/dev/null || true; } || true; \
	echo "==> Done."

ngrok:
	ngrok http --url=bossanova.ngrok.app 8080
