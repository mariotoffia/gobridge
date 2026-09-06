//go:build gobridge_aws || gobridge_all

package main

import awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"

func plugins() { awsstore.Register(reg) }
