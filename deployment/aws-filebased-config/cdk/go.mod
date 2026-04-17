module github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk

go 1.25.0

require (
	github.com/aws/aws-cdk-go/awscdk/v2 v2.248.0
	github.com/aws/constructs-go/constructs/v10 v10.6.0
	github.com/aws/jsii-runtime-go v1.127.0
	github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra v0.0.0
)

require (
	github.com/Masterminds/semver/v3 v3.4.0 // indirect
	github.com/cdklabs/awscdk-asset-awscli-go/awscliv1/v2 v2.2.273 // indirect
	github.com/cdklabs/awscdk-asset-node-proxy-agent-go/nodeproxyagentv6/v2 v2.1.1 // indirect
	github.com/cdklabs/cloud-assembly-schema-go/awscdkcloudassemblyschema/v53 v53.0.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/yuin/goldmark v1.7.16 // indirect
	golang.org/x/lint v0.0.0-20241112194109-818c5a804067 // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/telemetry v0.0.0-20260213145524-e0ab670178e1 // indirect
	golang.org/x/tools v0.42.0 // indirect
	golang.org/x/tools/cmd/godoc v0.1.0-deprecated // indirect
	golang.org/x/tools/godoc v0.1.0-deprecated // indirect
)

replace github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra => ../infra
