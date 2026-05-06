// Command cfgshape enforces the typed-pluggable-config contract
// described in FIX-003. See package cfgshape (this directory) for the
// rule set.
//
// Usage (via go vet):
//
//	go vet -vettool=$(pwd)/bin/cfgshape ./...
package main

import "golang.org/x/tools/go/analysis/singlechecker"

func main() { singlechecker.Main(Analyzer) }
