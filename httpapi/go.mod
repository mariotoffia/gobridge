module github.com/mariotoffia/gobridge/httpapi

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq v0.0.0
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/mariotoffia/gobridge => ..
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../adapters/native/store/memorydlq
	github.com/mariotoffia/gobridge/testutil/wait => ../testutil/wait
)
