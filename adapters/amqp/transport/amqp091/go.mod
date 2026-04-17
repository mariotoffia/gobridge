module github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091

go 1.25.0

replace github.com/mariotoffia/gobridge => ../../../..

replace github.com/mariotoffia/gobridge/testutil/rabbitmqlocal => ../../../../testutil/rabbitmqlocal

replace github.com/mariotoffia/gobridge/testutil/wait => ../../../../testutil/wait

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/testutil/rabbitmqlocal v0.0.0
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0
	github.com/rabbitmq/amqp091-go v1.10.0
)

require (
	github.com/kr/text v0.2.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
