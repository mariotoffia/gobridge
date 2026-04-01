package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/infra"
)

func main() {
	var specPath string
	flag.StringVar(&specPath, "spec", "", "path to the deployment spec JSON file")
	flag.Parse()

	if specPath == "" {
		fmt.Fprintln(os.Stderr, "missing -spec")
		os.Exit(1)
	}

	data, err := os.ReadFile(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read spec: %v\n", err)
		os.Exit(1)
	}

	var spec infra.AppSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		fmt.Fprintf(os.Stderr, "decode spec: %v\n", err)
		os.Exit(1)
	}

	spec = spec.Normalized()
	if err := spec.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "validate spec: %v\n", err)
		os.Exit(1)
	}

	if err := json.NewEncoder(os.Stdout).Encode(spec); err != nil {
		fmt.Fprintf(os.Stderr, "write normalized spec: %v\n", err)
		os.Exit(1)
	}
}
