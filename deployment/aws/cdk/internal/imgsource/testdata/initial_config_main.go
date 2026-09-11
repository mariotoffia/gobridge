package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"os"
)

//go:embed initial-config.base64
var initialConfigBase64 string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-initial-config-digest" {
		data, err := base64.StdEncoding.DecodeString(initialConfigBase64)
		if err != nil {
			os.Exit(1)
		}
		fmt.Printf("%x\n", sha256.Sum256(data))
		return
	}
	fmt.Print(initialConfigBase64)
}
