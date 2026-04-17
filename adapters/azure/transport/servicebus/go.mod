module github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus

go 1.25.0

replace (
	github.com/mariotoffia/gobridge => ../../../..
	github.com/mariotoffia/gobridge/testutil/asblocal => ../../../../testutil/asblocal
	github.com/mariotoffia/gobridge/testutil/wait => ../../../../testutil/wait
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.21.0
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.13.1
	github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus v1.10.0
	github.com/mariotoffia/gobridge v0.0.0-20260323112336-b482fe636793
	github.com/mariotoffia/gobridge/testutil/asblocal v0.0.0-00010101000000-000000000000
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.11.2 // indirect
	github.com/Azure/go-amqp v1.5.1 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.7.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0-00010101000000-000000000000 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.35.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
