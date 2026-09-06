//go:build gobridge_other || gobridge_all

package main

import awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"

func other() {
	awsstore.Register(reg)
	b.RegisterStoreFactory("dynamodb", nil)
}
