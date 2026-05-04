// Command aggcheck enforces the DDD aggregate-root naming convention.
// See the package doc on Analyzer for the rules.
//
// Usage (via go vet):
//
//	go vet -vettool=$(pwd)/bin/aggcheck ./domain/...
package main

import "golang.org/x/tools/go/analysis/singlechecker"

func main() { singlechecker.Main(Analyzer) }
