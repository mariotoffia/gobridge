module github.com/mariotoffia/gobridge/legacy

go 1.24.0

// This module exists solely to isolate legacy code from the root module.
// It is NOT included in go.work and will not compile.
// Legacy code is kept for reference only during the architecture cutover.

require (
	github.com/fsnotify/fsnotify v1.9.0
	github.com/mariotoffia/gobridge v0.0.0-20260323112336-b482fe636793
	github.com/stretchr/testify v1.11.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
)
