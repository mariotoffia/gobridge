# Makefile — deployment/aws-filebased-config
#
# Targets here are scoped to the file-based AWS deployment package; they do
# not pollute the repo-root Makefile.
.PHONY: help test integration-aws

help:  ## Show this help.
	@awk 'BEGIN {FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

test:  ## Run the profile's unit tests.
	@go -C infra test -short ./...
	@go -C cdk test -short ./...
	@go -C lib test -short ./...

integration-aws:  ## Run opt-in AWS integration tests (requires GOBRIDGE_INT_* env + AWS creds + cdk CLI).
	@cd cdk && go test -tags=integration_aws -count=1 -timeout=45m ./integration/...
