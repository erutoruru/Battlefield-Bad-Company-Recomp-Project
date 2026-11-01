module bfbclauncher

go 1.22

require (
	github.com/jchv/go-webview2 v0.0.0
	github.com/jchv/go-winloader v0.0.0
	golang.org/x/image v0.0.0
	golang.org/x/sys v0.0.0
)

require golang.org/x/text v0.0.0 // indirect

replace (
	github.com/jchv/go-webview2 => ./vendor-src/go-webview2
	github.com/jchv/go-winloader => ./vendor-src/go-winloader
	golang.org/x/image => ./vendor-src/ximage
	golang.org/x/sys => ./vendor-src/sys
	golang.org/x/text => ./vendor-src/xtext
)
