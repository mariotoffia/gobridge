module github.com/mariotoffia/gobridge/tests/integration

go 1.25.0

replace (
	github.com/mariotoffia/gobridge => ../..
	github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb => ../../adapters/aws/config/dynamodb
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq => ../../adapters/aws/store/dynamodbdlq
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease => ../../adapters/aws/store/dynamodblease
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox => ../../adapters/aws/store/dynamodboutbox
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs => ../../adapters/aws/transport/sqs
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../adapters/mqtt/transport/paho
	github.com/mariotoffia/gobridge/adapters/native/config/file => ../../adapters/native/config/file
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../../adapters/native/store/memorydlq
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease => ../../adapters/native/store/memorylease
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../adapters/native/store/memoryoutbox
	github.com/mariotoffia/gobridge/httpapi => ../../httpapi
	github.com/mariotoffia/gobridge/testutil/ddblocal => ../../testutil/ddblocal
	github.com/mariotoffia/gobridge/testutil/sqslocal => ../../testutil/sqslocal
	github.com/mariotoffia/gobridge/testutil/testcontent => ../../testutil/testcontent
	github.com/mariotoffia/gobridge/testutil/wait => ../../testutil/wait
)

require (
	github.com/aws/aws-sdk-go-v2/service/sqs v1.42.24
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/config/file v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease v0.0.0
	github.com/mariotoffia/gobridge/httpapi v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/testutil/ddblocal v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/testutil/sqslocal v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0
)

require (
	github.com/aws/aws-sdk-go-v2 v1.41.7 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.12 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.12 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.57.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodbstreams v1.32.14 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.11.20 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.20 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.41.9 // indirect
	github.com/aws/smithy-go v1.25.1 // indirect
	github.com/eclipse/paho.golang v0.23.0
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
