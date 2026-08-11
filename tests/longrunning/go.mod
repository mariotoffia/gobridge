module github.com/mariotoffia/gobridge/tests/longrunning

go 1.25.0

replace (
	github.com/mariotoffia/gobridge => ../..
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091 => ../../adapters/amqp/transport/amqp091
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10 => ../../adapters/amqp/transport/amqp10
	github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb => ../../adapters/aws/config/dynamodb
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease => ../../adapters/aws/store/dynamodblease
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox => ../../adapters/aws/store/dynamodboutbox
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout => ../../adapters/aws/store/dynamodbrollout
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs => ../../adapters/aws/transport/sqs
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../adapters/mqtt/transport/paho
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../../adapters/native/store/memorydlq
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../adapters/native/store/memoryoutbox
	github.com/mariotoffia/gobridge/httpapi => ../../httpapi
	github.com/mariotoffia/gobridge/processors/circuitbreaker => ../../processors/circuitbreaker
	github.com/mariotoffia/gobridge/processors/transform => ../../processors/transform
	github.com/mariotoffia/gobridge/testutil/artemislocal => ../../testutil/artemislocal
	github.com/mariotoffia/gobridge/testutil/ddblocal => ../../testutil/ddblocal
	github.com/mariotoffia/gobridge/testutil/rabbitmqlocal => ../../testutil/rabbitmqlocal
	github.com/mariotoffia/gobridge/testutil/sqslocal => ../../testutil/sqslocal
	github.com/mariotoffia/gobridge/testutil/testcontent => ../../testutil/testcontent
	github.com/mariotoffia/gobridge/testutil/wait => ../../testutil/wait
)

require (
	github.com/aws/aws-sdk-go-v2 v1.43.5
	github.com/aws/aws-sdk-go-v2/service/sqs v1.46.5
	github.com/eclipse/paho.golang v0.23.0
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091 v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10 v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/httpapi v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/processors/circuitbreaker v0.0.0-20260405055210-02d996a46256
	github.com/mariotoffia/gobridge/processors/transform v0.0.0-20260405055210-02d996a46256
	github.com/mariotoffia/gobridge/testutil/artemislocal v0.0.0
	github.com/mariotoffia/gobridge/testutil/ddblocal v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/testutil/rabbitmqlocal v0.0.0
	github.com/mariotoffia/gobridge/testutil/sqslocal v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0
	github.com/stretchr/testify v1.11.1
)

require github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.37 // indirect

require (
	github.com/Azure/go-amqp v1.7.0 // indirect
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
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mariotoffia/gobridge/testutil/mqttlocal v0.0.0
	github.com/ohler55/ojg v1.28.4 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rabbitmq/amqp091-go v1.13.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/mariotoffia/gobridge/testutil/mqttlocal => ../../testutil/mqttlocal
