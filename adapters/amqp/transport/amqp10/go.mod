module github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10

go 1.25.0

replace github.com/mariotoffia/gobridge => ../../../..

replace github.com/mariotoffia/gobridge/testutil/artemislocal => ../../../../testutil/artemislocal

replace github.com/mariotoffia/gobridge/testutil/wait => ../../../../testutil/wait

require (
	github.com/Azure/go-amqp v1.5.1
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/testutil/artemislocal v0.0.0
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0-20260424080041-92c57d17e957
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
