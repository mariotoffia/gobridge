module github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib

go 1.25.0

require (
	github.com/aws/aws-sdk-go-v2 v1.41.4
	github.com/aws/aws-sdk-go-v2/config v1.32.12
	github.com/aws/aws-sdk-go-v2/credentials v1.19.12
	github.com/aws/aws-sdk-go-v2/service/ssm v1.68.3
	github.com/mariotoffia/gobridge v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm v0.0.0
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs v0.0.0
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/config/file v0.0.0
	github.com/mariotoffia/gobridge/adapters/native/store v0.0.0
	github.com/mariotoffia/gobridge/testutil/localstack v0.0.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/cloudwatch v1.55.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.20 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/sqs v1.42.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.35.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.41.9 // indirect
	github.com/aws/smithy-go v1.24.2 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/eclipse/paho.golang v0.23.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq v0.0.0 // indirect
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox v0.0.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
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
	github.com/mariotoffia/gobridge => ../../..
	github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm => ../../../adapters/aws/credentials/ssm
	github.com/mariotoffia/gobridge/adapters/aws/transport/sqs => ../../../adapters/aws/transport/sqs
	github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho => ../../../adapters/mqtt/transport/paho
	github.com/mariotoffia/gobridge/adapters/native/config/file => ../../../adapters/native/config/file
	github.com/mariotoffia/gobridge/adapters/native/store => ../../../adapters/native/store
	github.com/mariotoffia/gobridge/adapters/native/store/memorydlq => ../../../adapters/native/store/memorydlq
	github.com/mariotoffia/gobridge/adapters/native/store/memorylease => ../../../adapters/native/store/memorylease
	github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox => ../../../adapters/native/store/memoryoutbox
	github.com/mariotoffia/gobridge/adapters/native/store/sqlitedlq => ../../../adapters/native/store/sqlitedlq
	github.com/mariotoffia/gobridge/adapters/native/store/sqliteoutbox => ../../../adapters/native/store/sqliteoutbox
	github.com/mariotoffia/gobridge/processors/circuitbreaker => ../../../processors/circuitbreaker
	github.com/mariotoffia/gobridge/testutil/localstack => ../../../testutil/localstack
)
