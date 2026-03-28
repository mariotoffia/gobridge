module github.com/mariotoffia/gobridge/tests/longrunning

go 1.25.0

replace (
	github.com/mariotoffia/gobridge => ../..
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease => ../../adapters/aws/store/dynamodblease
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox => ../../adapters/aws/store/dynamodboutbox
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs => ../../adapters/aws/transport/sqs
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../adapters/mqtt/transport/paho
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease => ../../adapters/native/store/memorylease
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../adapters/native/store/memoryoutbox
	github.com/mariotoffia/gobridge/testutil/ddblocal => ../../testutil/ddblocal
	github.com/mariotoffia/gobridge/testutil/sqslocal => ../../testutil/sqslocal
)

require (
	github.com/aws/aws-sdk-go-v2/service/sqs v1.42.24
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0
	github.com/mariotoffia/gobridge/testutil/ddblocal v0.0.0-00010101000000-000000000000
	github.com/mariotoffia/gobridge/testutil/sqslocal v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.11.1
)
