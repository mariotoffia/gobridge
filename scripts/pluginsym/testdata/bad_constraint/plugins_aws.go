//go:build gobridge_aws && linux

package main

import awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"

func plugins() {
	awsstore.Register(reg)
	sup.RegisterStoreFactory("dynamodb", nil)
}
