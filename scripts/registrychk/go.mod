module github.com/mariotoffia/gobridge/scripts/registrychk

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/store v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs v0.0.0
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store v0.0.0
)

require (
	github.com/aws/aws-sdk-go-v2 v1.43.5 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.36 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.35 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.36 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.36 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.36 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.37 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.63.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.16 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.12.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.36 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.5.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sqs v1.46.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.33.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.38.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.45.5 // indirect
	github.com/aws/smithy-go v1.27.7 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/eclipse/paho.golang v0.23.0 // indirect
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
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.75.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.0 // indirect
	modernc.org/sqlite v1.56.0 // indirect
)

replace github.com/mariotoffia/gobridge => ../..

replace github.com/mariotoffia/gobridge/adapters/aws/store => ../../adapters/aws/store

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq => ../../adapters/aws/store/dynamodbdlq

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease => ../../adapters/aws/store/dynamodblease

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbmanagedsubscriptions => ../../adapters/aws/store/dynamodbmanagedsubscriptions

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox => ../../adapters/aws/store/dynamodboutbox

replace github.com/mariotoffia/gobridge/adapters/aws/transport/sqs => ../../adapters/aws/transport/sqs

replace github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../adapters/mqtt/transport/paho

replace github.com/mariotoffia/gobridge/adapters/native/store => ../../adapters/native/store

replace github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../../adapters/native/store/memorydlq

replace github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../adapters/native/store/memoryoutbox

replace github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq => ../../adapters/native/store/sqlitedlq

replace github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions => ../../adapters/native/store/sqlitemanagedsubscriptions

replace github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox => ../../adapters/native/store/sqliteoutbox

replace github.com/mariotoffia/gobridge/testutil/wait => ../../testutil/wait

replace github.com/mariotoffia/gobridge/testutil/ddblocal => ../../testutil/ddblocal

replace github.com/mariotoffia/gobridge/testutil/mqttlocal => ../../testutil/mqttlocal
