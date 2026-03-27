module github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho

go 1.25.0

require github.com/mariotoffia/gobridge v0.0.0

require gopkg.in/yaml.v3 v3.0.1 // indirect

require (
	github.com/eclipse/paho.golang v0.23.0
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.52.0 // indirect
)

replace github.com/mariotoffia/gobridge => ../../../..
