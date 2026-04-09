module github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10

go 1.25.0

replace github.com/mariotoffia/gobridge => ../../../..

replace github.com/mariotoffia/gobridge/testutil/artemislocal => ../../../../testutil/artemislocal

require (
	github.com/Azure/go-amqp v1.5.1
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/testutil/artemislocal v0.0.0
)

require (
	github.com/kr/text v0.2.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
