[![Go Reference](https://pkg.go.dev/badge/github.com/fox-toolkit/compress.svg)](https://pkg.go.dev/github.com/fox-toolkit/compress)
[![tests](https://github.com/fox-toolkit/compress/actions/workflows/tests.yaml/badge.svg)](https://github.com/fox-toolkit/compress/actions?query=workflow%3Atests)
[![codecov](https://codecov.io/gh/fox-toolkit/compress/graph/badge.svg?token=MNXRMQM59T)](https://codecov.io/gh/fox-toolkit/compress)
![GitHub release (latest SemVer)](https://img.shields.io/github/v/release/fox-toolkit/compress)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/fox-toolkit/compress)

# Compress

Compress is a middleware for [Fox](https://github.com/fox-toolkit/fox) that compresses responses with zstd, brotli
or gzip, depending on the client `Accept-Encoding`.

## Disclaimer
The current API is not yet stabilized. Breaking changes may occur before v1.0.0 and will be noted on the release note.

## Getting started
### Installation

````shell
go get -u github.com/fox-toolkit/compress
````
### Feature
- zstd, brotli and gzip, or your own encoder.
- Compress only bodies above a minimum size.
- Allowlist or denylist of content types.
- Choose the encoder order when the client has no preference.
- Works with streaming responses, like server-sent events or a reverse proxy.

### Usage
````go
package main

import (
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/fox-toolkit/compress"
	"github.com/fox-toolkit/fox"
)

func main() {
	mw, err := compress.Middleware(compress.WithMinSize(512))
	if err != nil {
		log.Fatalln(err)
	}

	f := fox.MustRouter(fox.WithMiddleware(mw))

	f.MustAdd(fox.MethodGet, "/*{filepath}", fox.WrapH(http.FileServerFS(os.DirFS("public"))))

	if err := http.ListenAndServe(":8080", f); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalln(err)
	}
}
````
