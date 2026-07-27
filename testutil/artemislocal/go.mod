module github.com/mariotoffia/gobridge/testutil/artemislocal

go 1.25.0

require github.com/Azure/go-amqp v1.5.1

require (
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/mariotoffia/gobridge v0.3.3
)

replace github.com/mariotoffia/gobridge => ../..
