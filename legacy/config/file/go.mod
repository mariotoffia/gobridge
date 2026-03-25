module github.com/mariotoffia/gobridge/config/file

go 1.24.0

require (
	github.com/fsnotify/fsnotify v1.9.0
	github.com/mariotoffia/gobridge v0.0.0
)

require golang.org/x/sys v0.39.0 // indirect

replace github.com/mariotoffia/gobridge => ../..
