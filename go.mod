module github.com/mariotoffia/gobridge

go 1.25.0

require (
	github.com/fsnotify/fsnotify v1.9.0
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store v0.0.0
	github.com/stretchr/testify v1.11.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/eclipse/paho.golang v0.23.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

replace (
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ./adapters/mqtt/transport/paho
	github.com/mariotoffia/gobridge/adapters/native/store => ./adapters/native/store
)
