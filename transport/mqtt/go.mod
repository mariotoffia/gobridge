module github.com/mariotoffia/gobridge/transport/mqtt

go 1.24.0

require (
	github.com/eclipse/paho.golang v0.23.0
	github.com/mariotoffia/gobridge v0.0.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/net v0.43.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/mariotoffia/gobridge => ../..
