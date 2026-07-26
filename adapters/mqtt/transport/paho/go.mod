module github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho

go 1.25.0

require (
	github.com/eclipse/paho.golang v0.23.0
	github.com/gorilla/websocket v1.5.3
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0
	github.com/stretchr/testify v1.11.1
	golang.org/x/net v0.52.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/mariotoffia/gobridge/testutil/mqttlocal v0.0.0
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
)

replace github.com/mariotoffia/gobridge => ../../../..

replace github.com/mariotoffia/gobridge/testutil/wait => ../../../../testutil/wait

replace github.com/mariotoffia/gobridge/testutil/mqttlocal => ../../../../testutil/mqttlocal
