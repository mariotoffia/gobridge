//go:build gobridge_aws || gobridge_all

package main

import (
	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqs "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
)

func plugins() {
	sqs.Register(reg)
	awsstore.Register(reg)
	sup.RegisterTransport("sqs", nil)
	sup.RegisterStoreFactory("dynamodb", nil)
}
