.PHONY: connect-production connect-staging db-production db-staging deploy-production deploy-staging release split stage-release verify-production verify-staging

deploy-staging:
	$(MAKE) -C infra/kustomize deploy-staging

deploy-production:
	$(MAKE) -C infra/kustomize deploy-production

db-staging:
	$(MAKE) -C infra/kustomize db-staging

db-production:
	$(MAKE) -C infra/kustomize db-production

connect-staging:
	$(MAKE) -C infra/kustomize connect-staging

connect-production:
	$(MAKE) -C infra/kustomize connect-production

verify-staging:
	$(MAKE) -C infra/kustomize verify-staging

verify-production:
	$(MAKE) -C infra/kustomize verify-production

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
