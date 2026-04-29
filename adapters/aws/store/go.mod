module github.com/mariotoffia/gobridge/adapters/aws/store

go 1.25.0

require (
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.57.0
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox v0.0.0
)

require (
	github.com/aws/aws-sdk-go-v2 v1.41.5 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.21 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.21 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.11.20 // indirect
	github.com/aws/smithy-go v1.24.2 // indirect
)

replace (
	github.com/mariotoffia/gobridge => ../../..
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbdlq => ./dynamodbdlq
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease => ./dynamodblease
	github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox => ./dynamodboutbox
	github.com/mariotoffia/gobridge/testutil/ddblocal => ../../../testutil/ddblocal
	github.com/mariotoffia/gobridge/testutil/wait => ../../../testutil/wait
)
