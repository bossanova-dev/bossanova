.PHONY: db-production db-staging release split stage-release

DB_PROJECT_ID ?= madverts-production
DB_INSTANCE ?= madverts-production
DB_REGION ?= europe-west1
DB_CONNECTION_NAME ?= $(DB_PROJECT_ID):$(DB_REGION):$(DB_INSTANCE)
DB_SOCKET_DIR ?= $(HOME)/.bossanova-sql

# db-staging / db-production: human operators only, for SQL applies and grants
# as the app user. Both databases live on the shared Madverts instance, so one
# Cloud SQL Auth Proxy serves both. It listens on a private 0700 Unix socket
# under HOME, with no bastion and no TCP port, the same way madverts-core's
# db-production-admin does since MAD-1387. The proxy authenticates the
# connection with your Application Default Credentials; Postgres still checks
# the app user's password.
define db_proxy
	@command -v cloud-sql-proxy >/dev/null 2>&1 \
		|| { echo "cloud-sql-proxy not found. Install with: brew install cloud-sql-proxy"; exit 1; }
	@gcloud auth application-default print-access-token >/dev/null 2>&1 \
		|| { echo "Application Default Credentials are missing or expired."; \
		     echo "Run: gcloud auth application-default login"; exit 1; }
	@SOCKET_DIR="$(DB_SOCKET_DIR)"; \
		HOME_DIR="$(HOME)"; \
		case "$$SOCKET_DIR" in "$$HOME_DIR"/*) ;; \
			*) echo "Socket directory must be a child of HOME: $$SOCKET_DIR"; exit 1 ;; \
		esac; \
		case "/$$SOCKET_DIR/" in */../*) \
			echo "Socket directory must not contain '..': $$SOCKET_DIR"; exit 1 ;; \
		esac; \
		mkdir -p "$$SOCKET_DIR" \
			|| { echo "Cannot create socket directory: $$SOCKET_DIR"; exit 1; }; \
		REAL_HOME=$$(cd "$$HOME_DIR" && pwd -P) \
			|| { echo "Cannot resolve HOME: $$HOME_DIR"; exit 1; }; \
		REAL_SOCKET_DIR=$$(cd "$$SOCKET_DIR" && pwd -P) \
			|| { echo "Cannot resolve socket directory: $$SOCKET_DIR"; exit 1; }; \
		case "$$REAL_SOCKET_DIR" in "$$REAL_HOME"/*) ;; \
			*) echo "Socket directory resolves outside HOME: $$SOCKET_DIR"; exit 1 ;; \
		esac; \
		REAL_SOCKET_PATH="$$REAL_SOCKET_DIR/$(DB_CONNECTION_NAME)/.s.PGSQL.5432"; \
		if [ $$(printf '%s' "$$REAL_SOCKET_PATH" | wc -c | tr -d ' ') -gt 103 ]; then \
			echo "Socket path exceeds Darwin's 103-byte limit: $$REAL_SOCKET_PATH"; exit 1; \
		fi; \
		chmod 700 "$$REAL_SOCKET_DIR" \
			|| { echo "Cannot make socket directory private: $$REAL_SOCKET_DIR"; exit 1; }; \
		if [ ! -O "$$REAL_SOCKET_DIR" ]; then \
			echo "Socket directory is owned by another user: $$REAL_SOCKET_DIR"; exit 1; \
		fi; \
		PERMISSIONS=$$(stat -f '%Lp' "$$REAL_SOCKET_DIR" 2>/dev/null \
			|| stat -c '%a' "$$REAL_SOCKET_DIR" 2>/dev/null); \
		if [ "$$PERMISSIONS" != "700" ]; then \
			echo "Socket directory must have mode 0700: $$REAL_SOCKET_DIR"; exit 1; \
		fi; \
		echo "Human operators only: production SQL applies and grants. Agents never use this socket."; \
		echo "psql 'postgresql:///$(1)?host=$$REAL_SOCKET_DIR/$(DB_CONNECTION_NAME)&user=<app user>'"; \
		echo "  (app user and password: the pgbouncer_auth_user and pgbouncer_auth_password outputs of the bossanova-$(2) Terraform workspace)"; \
		echo "Starting the Cloud SQL Auth Proxy on a private socket (Ctrl+C to close)..."; \
		cloud-sql-proxy --unix-socket "$$REAL_SOCKET_DIR" $(DB_CONNECTION_NAME)
endef

db-staging:
	$(call db_proxy,bossanova_staging,staging)

db-production:
	$(call db_proxy,bossanova_production,production)

## release: Trigger the production release workflow (creates a PR from main → production)
release:
	gh workflow run create-production-release.yml --ref main

## stage-release: Trigger the staging release workflow (creates a PR to staging from a release branch cut at main)
stage-release:
	gh workflow run create-staging-release.yml --ref main

## split: Mirror subtrees to separate repos via splitsh/lite
split:
	splitsh-lite --prefix=proto --target=refs/heads/split/proto
	splitsh-lite --prefix=lib/bossalib --target=refs/heads/split/bossalib
	splitsh-lite --prefix=services/boss --target=refs/heads/split/boss
	splitsh-lite --prefix=services/bossd --target=refs/heads/split/bossd
