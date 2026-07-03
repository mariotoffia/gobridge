module github.com/mariotoffia/gobridge/tests/docsexamples

go 1.25.0

replace (
	github.com/mariotoffia/gobridge => ../..
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091 => ../../adapters/amqp/transport/amqp091
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10 => ../../adapters/amqp/transport/amqp10
	github.com/mariotoffia/gobridge/adapters/aws/store => ../../adapters/aws/store
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq => ../../adapters/aws/store/dynamodbdlq
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease => ../../adapters/aws/store/dynamodblease
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox => ../../adapters/aws/store/dynamodboutbox
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs => ../../adapters/aws/transport/sqs
	github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus => ../../adapters/azure/transport/servicebus
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../adapters/mqtt/transport/paho
	github.com/mariotoffia/gobridge/adapters/native/store => ../../adapters/native/store
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../../adapters/native/store/memorydlq
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease => ../../adapters/native/store/memorylease
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../adapters/native/store/memoryoutbox
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq => ../../adapters/native/store/sqlitedlq
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox => ../../adapters/native/store/sqliteoutbox
	github.com/mariotoffia/gobridge/testutil/artemislocal => ../../testutil/artemislocal
	github.com/mariotoffia/gobridge/testutil/asblocal => ../../testutil/asblocal
	github.com/mariotoffia/gobridge/testutil/ddblocal => ../../testutil/ddblocal
	github.com/mariotoffia/gobridge/testutil/rabbitmqlocal => ../../testutil/rabbitmqlocal
	github.com/mariotoffia/gobridge/testutil/sqslocal => ../../testutil/sqslocal
	github.com/mariotoffia/gobridge/testutil/wait => ../../testutil/wait
)

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091 v0.0.0
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10 v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/store v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs v0.0.0
	github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus v0.0.0
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store v0.0.0
	github.com/stretchr/testify v1.11.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.21.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.13.1 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.11.2 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus v1.10.0 // indirect
	github.com/Azure/go-amqp v1.5.1 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.7.0 // indirect
	github.com/aws/aws-sdk-go-v2 v1.41.7 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.12 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.12 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.57.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.11.20 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.20 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/sqs v1.42.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.41.9 // indirect
	github.com/aws/smithy-go v1.25.1 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/eclipse/paho.golang v0.23.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox v0.0.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rabbitmq/amqp091-go v1.10.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.35.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.47.0 // indirect
)
