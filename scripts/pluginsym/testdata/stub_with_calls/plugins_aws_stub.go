//go:build !gobridge_aws && !gobridge_all

package main

func plugins() { b.RegisterStoreFactory("dynamodb", nil) }
