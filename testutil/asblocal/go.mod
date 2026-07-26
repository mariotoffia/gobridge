module github.com/mariotoffia/gobridge/testutil/asblocal

go 1.25.0

require (
	github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus v1.10.0
	github.com/mariotoffia/gobridge v0.0.0
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.18.2 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.11.2 // indirect
	github.com/Azure/go-amqp v1.4.0 // indirect
	golang.org/x/net v0.42.0 // indirect
	golang.org/x/text v0.27.0 // indirect
)

replace github.com/mariotoffia/gobridge => ../..
