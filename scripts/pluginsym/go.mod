module github.com/mariotoffia/gobridge/scripts/pluginsym

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store v0.0.0
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.22.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.14.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.12.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus v1.10.0 // indirect
	github.com/Azure/go-amqp v1.7.0 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.8.0 // indirect
	github.com/aws/aws-sdk-go-v2 v1.45.1 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.36 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.35 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.36 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.1 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.1 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.37 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.63.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.12.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.5.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sqs v1.46.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.33.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.38.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.45.5 // indirect
	github.com/aws/smithy-go v1.28.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq v0.3.6 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease v0.3.6 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbmanagedsubscriptions v0.3.6 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox v0.3.6 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/rabbitmq/amqp091-go v1.13.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/eclipse/paho.golang v0.23.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091 v0.3.6
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10 v0.3.6
	github.com/mariotoffia/gobridge/adapters/aws/store v0.3.6
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs v0.3.6
	github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus v0.3.6
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

replace github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../adapters/mqtt/transport/paho

replace github.com/mariotoffia/gobridge/adapters/native/store => ../../adapters/native/store

replace github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../../adapters/native/store/memorydlq

replace github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../adapters/native/store/memoryoutbox

replace github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq => ../../adapters/native/store/sqlitedlq

replace github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions => ../../adapters/native/store/sqlitemanagedsubscriptions

replace github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox => ../../adapters/native/store/sqliteoutbox

replace github.com/mariotoffia/gobridge/testutil/wait => ../../testutil/wait

replace github.com/mariotoffia/gobridge/testutil/mqttlocal => ../../testutil/mqttlocal

replace github.com/mariotoffia/gobridge/adapters/aws/store => ../../adapters/aws/store

replace github.com/mariotoffia/gobridge/adapters/aws/transport/sqs => ../../adapters/aws/transport/sqs

replace github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus => ../../adapters/azure/transport/servicebus

replace github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091 => ../../adapters/amqp/transport/amqp091

replace github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10 => ../../adapters/amqp/transport/amqp10

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq => ../../adapters/aws/store/dynamodbdlq

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease => ../../adapters/aws/store/dynamodblease

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbmanagedsubscriptions => ../../adapters/aws/store/dynamodbmanagedsubscriptions

replace github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox => ../../adapters/aws/store/dynamodboutbox

replace github.com/mariotoffia/gobridge/testutil/ddblocal => ../../testutil/ddblocal

replace github.com/mariotoffia/gobridge/testutil/flocilocal => ../../testutil/flocilocal

replace github.com/mariotoffia/gobridge/testutil/asblocal => ../../testutil/asblocal

replace github.com/mariotoffia/gobridge/testutil/rabbitmqlocal => ../../testutil/rabbitmqlocal

replace github.com/mariotoffia/gobridge/testutil/artemislocal => ../../testutil/artemislocal
