module github.com/mariotoffia/gobridge/adapters/native/config/file

go 1.25.0

require (
	github.com/fsnotify/fsnotify v1.9.0
	github.com/mariotoffia/gobridge v0.0.0
)

require golang.org/x/sys v0.42.0 // indirect

replace github.com/mariotoffia/gobridge => ../../../..
