module github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho

go 1.24.0

require github.com/mariotoffia/gobridge v0.0.0

require (
	github.com/eclipse/paho.golang v0.23.0
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.43.0 // indirect
)

replace github.com/mariotoffia/gobridge => ../../../..
