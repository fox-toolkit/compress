// Copyright 2026 Sylvain Müller. All rights reserved.
// Mount of this source code is governed by a MIT license that can be found
// at https://github.com/fox-toolkit/compress/blob/main/LICENSE.txt.

package compress

import (
	"errors"
	"fmt"
	"mime"
	"strings"

	"golang.org/x/net/http/httpguts"
)

var ErrInvalidConfig = errors.New("invalid config")

// DefaultMinSize is the default minimum body size to compress.
const DefaultMinSize = 1024

// Option configures the middleware.
type Option interface {
	apply(*config) error
}

type optionFunc func(*config) error

func (f optionFunc) apply(c *config) error {
	return f(c)
}

type config struct {
	encoders []Encoder
	included []string
	excluded []string
	minSize  int
	include  bool
	exclude  bool
}

func defaultConfig() *config {
	return &config{
		encoders: []Encoder{Zstd(3), Brotli(4), Gzip(6)},
		minSize:  DefaultMinSize,
	}
}

// WithEncoders sets the encoders in order of preference. A preference expressed by the client takes
// precedence over this order. The default is zstd, br then gzip.
func WithEncoders(enc ...Encoder) Option {
	return optionFunc(func(c *config) error {
		if len(enc) == 0 {
			return fmt.Errorf("%w: no encoder", ErrInvalidConfig)
		}
		seen := make(map[string]struct{}, len(enc))
		for _, e := range enc {
			if e == nil {
				return fmt.Errorf("%w: nil encoder", ErrInvalidConfig)
			}
			if v, ok := e.(validator); ok {
				if err := v.validate(); err != nil {
					return err
				}
			}
			name := strings.ToLower(e.Encoding())
			if name == "identity" || name == "*" || !httpguts.ValidHeaderFieldName(name) {
				return fmt.Errorf("%w: invalid encoding %q", ErrInvalidConfig, e.Encoding())
			}
			if _, ok := seen[name]; ok {
				return fmt.Errorf("%w: duplicate encoding %q", ErrInvalidConfig, e.Encoding())
			}
			seen[name] = struct{}{}
		}
		c.encoders = enc
		return nil
	})
}

// WithMinSize sets the minimum body size to compress. Smaller bodies are sent as is. The default is
// [DefaultMinSize].
func WithMinSize(n int) Option {
	return optionFunc(func(c *config) error {
		if n < 0 {
			return fmt.Errorf("%w: negative min size", ErrInvalidConfig)
		}
		c.minSize = n
		return nil
	})
}

// WithIncludedContentTypes compresses only the given media types. A "type/*" entry matches any subtype.
// It cannot be combined with [WithExcludedContentTypes].
func WithIncludedContentTypes(types ...string) Option {
	return optionFunc(func(c *config) error {
		mt, err := parseMediaTypes(types)
		if err != nil {
			return err
		}
		c.included = append(c.included, mt...)
		c.include = true
		return nil
	})
}

// WithExcludedContentTypes never compresses the given media types, in addition to the default ones
// (already compressed formats and gRPC). A "type/*" entry matches any subtype. It cannot be combined with
// [WithIncludedContentTypes].
func WithExcludedContentTypes(types ...string) Option {
	return optionFunc(func(c *config) error {
		mt, err := parseMediaTypes(types)
		if err != nil {
			return err
		}
		c.excluded = append(c.excluded, mt...)
		c.exclude = true
		return nil
	})
}

func parseMediaTypes(types []string) ([]string, error) {
	if len(types) == 0 {
		return nil, fmt.Errorf("%w: no media type", ErrInvalidConfig)
	}
	out := make([]string, 0, len(types))
	for _, t := range types {
		mt, params, err := mime.ParseMediaType(t)
		if err != nil {
			return nil, fmt.Errorf("%w: media type %q: %w", ErrInvalidConfig, t, err)
		}
		if len(params) > 0 {
			return nil, fmt.Errorf("%w: media type %q: parameters are not supported", ErrInvalidConfig, t)
		}
		typ, sub, ok := strings.Cut(mt, "/")
		if !ok {
			return nil, fmt.Errorf("%w: media type %q: missing subtype", ErrInvalidConfig, t)
		}
		switch {
		case typ == "*" && sub != "*", typ != "*" && strings.Contains(typ, "*"), sub != "*" && strings.Contains(sub, "*"):
			return nil, fmt.Errorf("%w: media type %q: unsupported wildcard", ErrInvalidConfig, t)
		}
		out = append(out, mt)
	}
	return out, nil
}

// defaultExcluded lists formats that are already compressed, and gRPC which has its own compression.
var defaultExcluded = []string{
	"application/grpc",
	"application/grpc+proto",
	"application/grpc+json",
	"application/gzip",
	"application/x-gzip",
	"application/zstd",
	"application/zip",
	"application/x-bzip2",
	"application/x-xz",
	"application/x-7z-compressed",
	"application/vnd.rar",
	"application/x-rar-compressed",
	"font/woff",
	"font/woff2",
	"image/jpeg",
	"image/png",
	"image/gif",
	"image/webp",
	"image/avif",
	"image/heic",
	"audio/*",
	"video/*",
}
