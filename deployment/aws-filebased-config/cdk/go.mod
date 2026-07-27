module github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk

go 1.25.0

require (
	github.com/aws/aws-cdk-go/awscdk/v2 v2.248.0
	github.com/aws/aws-sdk-go-v2 v1.41.7
	github.com/aws/aws-sdk-go-v2/config v1.32.12
	github.com/aws/aws-sdk-go-v2/service/cloudwatch v1.55.2
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.57.0
	github.com/aws/aws-sdk-go-v2/service/ecs v1.79.1
	github.com/aws/aws-sdk-go-v2/service/sqs v1.42.24
	github.com/aws/constructs-go/constructs/v10 v10.6.0
	github.com/aws/jsii-runtime-go v1.127.0
	github.com/mariotoffia/gobridge/adapters/aws/store v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/native/store v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra v0.0.0
)

require (
	github.com/aws/aws-sdk-go-v2/credentials v1.19.12 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.11.20 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.20 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssm v1.68.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.41.9 // indirect
	github.com/aws/smithy-go v1.25.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/eclipse/paho.golang v0.23.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbmanagedsubscriptions v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox v0.0.0 // indirect
	github.com/mariotoffia/gobridge/testutil/ddblocal v0.0.0-20260507130243-4750750b6096 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/net v0.57.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.47.0 // indirect
)

require (
	github.com/Masterminds/semver/v3 v3.4.0 // indirect
	github.com/cdklabs/awscdk-asset-awscli-go/awscliv1/v2 v2.2.273 // indirect
	github.com/cdklabs/awscdk-asset-node-proxy-agent-go/nodeproxyagentv6/v2 v2.1.1 // indirect
	github.com/cdklabs/cloud-assembly-schema-go/awscdkcloudassemblyschema/v53 v53.0.0 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm v0.0.0
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/yuin/goldmark v1.7.16 // indirect
	golang.org/x/lint v0.0.0-20241112194109-818c5a804067 // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260213145524-e0ab670178e1 // indirect
	golang.org/x/tools v0.42.0 // indirect
	golang.org/x/tools/cmd/godoc v0.1.0-deprecated // indirect
	golang.org/x/tools/godoc v0.1.0-deprecated // indirect
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra => ../infra

replace github.com/mariotoffia/gobridge => ../../..

replace github.com/mariotoffia/gobridge/adapters/aws/transport/sqs => ../../../adapters/aws/transport/sqs

replace github.com/mariotoffia/gobridge/adapters/aws/store => ../../../adapters/aws/store

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq => ../../../adapters/aws/store/dynamodbdlq

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease => ../../../adapters/aws/store/dynamodblease

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox => ../../../adapters/aws/store/dynamodboutbox

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbmanagedsubscriptions => ../../../adapters/aws/store/dynamodbmanagedsubscriptions

replace github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../../adapters/mqtt/transport/paho

replace github.com/mariotoffia/gobridge/adapters/native/store => ../../../adapters/native/store

replace github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../../../adapters/native/store/memorydlq

replace github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../../adapters/native/store/memoryoutbox

replace github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq => ../../../adapters/native/store/sqlitedlq

replace github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox => ../../../adapters/native/store/sqliteoutbox

replace github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions => ../../../adapters/native/store/sqlitemanagedsubscriptions

replace github.com/mariotoffia/gobridge/testutil/wait => ../../../testutil/wait

replace github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm => ../../../adapters/aws/credentials/ssm

replace github.com/mariotoffia/gobridge/testutil/localstack => ../../../testutil/localstack

replace github.com/mariotoffia/gobridge/testutil/mqttlocal => ../../../testutil/mqttlocal
