module github.com/mariotoffia/gobridge/adapters/native/config/file

go 1.25.0

require (
	github.com/fsnotify/fsnotify v1.10.1
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/testutil/wait v0.0.0-20260424080041-92c57d17e957
)

require (
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/mariotoffia/gobridge => ../../../..
