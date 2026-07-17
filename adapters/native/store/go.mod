module github.com/mariotoffia/gobridge/adapters/native/store

go 1.25.0

require (
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox v0.0.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.42.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.47.0 // indirect
)

replace (
	github.com/mariotoffia/gobridge => ../../..
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ./memorydlq
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease => ./memorylease
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ./memoryoutbox
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq => ./sqlitedlq
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions => ./sqlitemanagedsubscriptions
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox => ./sqliteoutbox
	github.com/mariotoffia/gobridge/testutil/wait => ../../../testutil/wait
)
