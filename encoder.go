// Copyright 2026 Sylvain Müller. All rights reserved.
// Mount of this source code is governed by a MIT license that can be found
// at https://github.com/fox-toolkit/compress/blob/main/LICENSE.txt.

package compress

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// Encoder creates the [Writer] for a content encoding.
type Encoder interface {
	// Encoding returns the encoding name, such as "gzip".
	Encoding() string
	// NewWriter returns a new [Writer] that writes to w.
	// Internally, writers are pooled.
	NewWriter(w io.Writer) Writer
}

// Writer is a resettable compressor.
type Writer interface {
	io.WriteCloser
	// Flush writes any pending compressed data to the underlying writer.
	Flush() error
	// Reset discards the writer state and makes it write to w. The middleware calls Reset(nil) before
	// returning a writer to its pool.
	Reset(w io.Writer)
}

type validator interface {
	validate() error
}

// Gzip returns an [Encoder] for the gzip encoding. The level ranges from -3 (stateless) to 9.
func Gzip(level int) Encoder {
	return gzipEncoder{level: level}
}

// Brotli returns an [Encoder] for the br encoding. The level ranges from 0 to 11.
func Brotli(level int) Encoder {
	return brotliEncoder{level: level}
}

// Zstd returns an [Encoder] for the zstd encoding. The level follows the zstd scale, from 1 to 22.
func Zstd(level int) Encoder {
	return zstdEncoder{level: level}
}

type gzipEncoder struct {
	level int
}

func (e gzipEncoder) Encoding() string { return "gzip" }

func (e gzipEncoder) NewWriter(w io.Writer) Writer {
	zw, _ := gzip.NewWriterLevel(w, e.level)
	return zw
}

func (e gzipEncoder) validate() error {
	if e.level < gzip.StatelessCompression || e.level > gzip.BestCompression {
		return fmt.Errorf("%w: gzip level %d", ErrInvalidConfig, e.level)
	}
	return nil
}

type brotliEncoder struct {
	level int
}

func (e brotliEncoder) Encoding() string { return "br" }

func (e brotliEncoder) NewWriter(w io.Writer) Writer {
	// A 256 KiB window bounds the memory of pooled writers.
	return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: e.level, LGWin: 18})
}

func (e brotliEncoder) validate() error {
	if e.level < brotli.BestSpeed || e.level > brotli.BestCompression {
		return fmt.Errorf("%w: brotli level %d", ErrInvalidConfig, e.level)
	}
	return nil
}

type zstdEncoder struct {
	level int
}

func (e zstdEncoder) Encoding() string { return "zstd" }

func (e zstdEncoder) NewWriter(w io.Writer) Writer {
	zw, _ := zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(e.level)),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true),
		zstd.WithWindowSize(128<<10),
		zstd.WithZeroFrames(true),
	)
	return zw
}

func (e zstdEncoder) validate() error {
	if e.level < 1 || e.level > 22 {
		return fmt.Errorf("%w: zstd level %d", ErrInvalidConfig, e.level)
	}
	return nil
}

type encoder struct {
	pool sync.Pool
	name string
}

func newEncoder(e Encoder) *encoder {
	enc := &encoder{name: strings.ToLower(e.Encoding())}
	enc.pool.New = func() any {
		return e.NewWriter(nil)
	}
	return enc
}

func (e *encoder) get(w io.Writer) Writer {
	zw := e.pool.Get().(Writer)
	zw.Reset(w)
	return zw
}

func (e *encoder) put(zw Writer) {
	zw.Reset(nil)
	e.pool.Put(zw)
}
