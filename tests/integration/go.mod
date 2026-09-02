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
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../adapters/native/store/memoryoutbox
	github.com/mariotoffia/gobridge/httpapi => ../../httpapi
	github.com/mariotoffia/gobridge/testutil/ddblocal => ../../testutil/ddblocal
	github.com/mariotoffia/gobridge/testutil/flocilocal => ../../testutil/flocilocal
	github.com/mariotoffia/gobridge/testutil/testcontent => ../../testutil/testcontent
	github.com/mariotoffia/gobridge/testutil/wait => ../../testutil/wait
)

require (
	github.com/aws/aws-sdk-go-v2/service/sqs v1.46.5
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/config/file v0.0.0
	github.com/mariotoffia/gobridge/httpapi v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/testutil/ddblocal v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/testutil/flocilocal v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0
)

require (
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.37 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	modernc.org/libc v1.75.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.0 // indirect
	modernc.org/sqlite v1.56.0 // indirect
)

require (
	github.com/aws/aws-sdk-go-v2 v1.43.5
	github.com/aws/aws-sdk-go-v2/config v1.32.36 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.35 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.36 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.36 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.36 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.63.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodbstreams v1.36.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.16 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.12.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.36 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.5.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.33.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.38.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.45.5 // indirect
	github.com/aws/smithy-go v1.27.7 // indirect
	github.com/eclipse/paho.golang v0.23.0
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions v0.0.0
	github.com/mariotoffia/gobridge/testutil/mqttlocal v0.0.0
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions => ../../adapters/native/store/sqlitemanagedsubscriptions

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout => ../../adapters/aws/store/dynamodbrollout

replace github.com/mariotoffia/gobridge/testutil/mqttlocal => ../../testutil/mqttlocal
