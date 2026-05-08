# Makefile — deployment/aws-filebased-config
#
# Targets here are scoped to the file-based AWS deployment package; they do
# not pollute the repo-root Makefile.
.PHONY: help test update-seeder-image integration-aws

SEEDER_DIR := cdk/constructs/internal/seeder

help:  ## Show this help.
	@awk 'BEGIN {FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

test:  ## Run seeder integration tests (bash + python3 + PyYAML).
	@bash $(SEEDER_DIR)/tests/run.sh

update-seeder-image:  ## Refresh pinned aws-cli image digest in image.txt.
	@bash $(SEEDER_DIR)/scripts/update-image.sh

integration-aws:  ## Run opt-in AWS integration tests (requires GOBRIDGE_INT_* env + AWS creds + cdk CLI).
	@cd cdk && go test -tags=integration_aws -count=1 -timeout=45m ./integration/...
