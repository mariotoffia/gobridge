module github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/processors/circuitbreaker v0.0.0-20260405055210-02d996a46256
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

require (
	github.com/eclipse/paho.golang v0.23.0
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.52.0 // indirect
)

replace github.com/mariotoffia/gobridge => ../../../..
