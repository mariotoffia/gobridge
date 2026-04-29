// Command aclcheck enforces the hexagonal anti-corruption layer naming
// convention. See package aclcheck for the policy.
//
// Usage (via go vet):
//
//	go vet -vettool=$(pwd)/bin/aclcheck ./adapters/...
package main

import "golang.org/x/tools/go/analysis/singlechecker"

func main() { singlechecker.Main(Analyzer) }
