module github.com/mariotoffia/gobridge/scripts/pluginsym

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store v0.0.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/eclipse/paho.golang v0.23.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox v0.0.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.47.0 // indirect
)

replace github.com/mariotoffia/gobridge => ../..

replace github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../adapters/mqtt/transport/paho

replace github.com/mariotoffia/gobridge/adapters/native/store => ../../adapters/native/store

replace github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../../adapters/native/store/memorydlq

replace github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../adapters/native/store/memoryoutbox

replace github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq => ../../adapters/native/store/sqlitedlq

replace github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions => ../../adapters/native/store/sqlitemanagedsubscriptions

replace github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox => ../../adapters/native/store/sqliteoutbox

replace github.com/mariotoffia/gobridge/testutil/wait => ../../testutil/wait
