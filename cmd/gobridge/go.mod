module github.com/mariotoffia/gobridge/cmd/gobridge

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/config/file v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/credentials/file v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store v0.0.0
	github.com/mariotoffia/gobridge/httpapi v0.0.0-00010101000000-000000000000
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/eclipse/paho.golang v0.23.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox v0.0.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.47.0 // indirect
)

replace (
	github.com/mariotoffia/gobridge => ../..
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../adapters/mqtt/transport/paho
	github.com/mariotoffia/gobridge/adapters/native/config/file => ../../adapters/native/config/file
	github.com/mariotoffia/gobridge/adapters/native/credentials/file => ../../adapters/native/credentials/file
	github.com/mariotoffia/gobridge/adapters/native/store => ../../adapters/native/store
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../../adapters/native/store/memorydlq
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease => ../../adapters/native/store/memorylease
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../adapters/native/store/memoryoutbox
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq => ../../adapters/native/store/sqlitedlq
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox => ../../adapters/native/store/sqliteoutbox
	github.com/mariotoffia/gobridge/httpapi => ../../httpapi
	github.com/mariotoffia/gobridge/testutil/wait => ../../testutil/wait
)
