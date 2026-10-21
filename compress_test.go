// Copyright 2026 Sylvain Müller. All rights reserved.
// Mount of this source code is governed by a MIT license that can be found
// at https://github.com/fox-toolkit/compress/blob/main/LICENSE.txt.

package compress

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/fox-toolkit/fox"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var text = []byte(strings.Repeat("Fox is a high-performance HTTP router. ", 100))

func decode(t *testing.T, encoding string, body []byte) []byte {
	t.Helper()
	var r io.Reader
	switch encoding {
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		require.NoError(t, err)
		r = zr
	case "br":
		r = brotli.NewReader(bytes.NewReader(body))
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(body))
		require.NoError(t, err)
		defer zr.Close()
		r = zr
	default:
		return body
	}
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return out
}

func serve(t *testing.T, h fox.HandlerFunc, req *http.Request, opts ...Option) *httptest.ResponseRecorder {
	t.Helper()
	mw, err := Middleware(opts...)
	require.NoError(t, err)
	f := fox.MustRouter(fox.WithMiddleware(mw))
	f.MustAdd(fox.MethodAny, "/", h)
	w := httptest.NewRecorder()
	f.ServeHTTP(w, req)
	return w
}

func get(acceptEncoding string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	return req
}

func write(ct string, body []byte) fox.HandlerFunc {
	return func(c *fox.Context) {
		if ct != "" {
			c.Writer().Header().Set("Content-Type", ct)
		}
		_, _ = c.Writer().Write(body)
	}
}

func TestNegotiate(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)

	cases := []struct {
		name   string
		values []string
		want   string
	}{
		{"none", nil, ""},
		{"empty", []string{""}, ""},
		{"single", []string{"gzip"}, "gzip"},
		{"server order on tie", []string{"gzip, br, zstd"}, "zstd"},
		{"client weight", []string{"zstd;q=0.5, gzip"}, "gzip"},
		{"case insensitive", []string{"GZIP;Q=1"}, "gzip"},
		{"x-gzip", []string{"x-gzip"}, "gzip"},
		{"refused", []string{"gzip;q=0"}, ""},
		{"q after other param", []string{"gzip;level=1;q=0"}, ""},
		{"whitespace", []string{"gzip ; q = 0"}, ""},
		{"malformed weight", []string{"gzip;q=abc, br;q=0.1"}, "br"},
		{"out of range weight", []string{"gzip;q=1.5, br;q=0.1"}, "br"},
		{"first occurrence wins", []string{"gzip;q=0, gzip"}, ""},
		{"multiple lines", []string{"deflate", "br"}, "br"},
		{"wildcard", []string{"*"}, "zstd"},
		{"wildcard with exclusion", []string{"*, zstd;q=0, br;q=0"}, "gzip"},
		{"refused wildcard", []string{"*;q=0"}, ""},
		{"identity only", []string{"identity"}, ""},
		{"unknown", []string{"deflate, compress"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc := m.negotiate(tc.values)
			if tc.want == "" {
				assert.Nil(t, enc)
				return
			}
			require.NotNil(t, enc)
			assert.Equal(t, tc.want, enc.name)
		})
	}
}

func TestParseQ(t *testing.T) {
	cases := map[string]int{
		"1":      1000,
		"1.":     1000,
		"1.000":  1000,
		"0":      0,
		"0.5":    500,
		"0.123":  123,
		"0.001":  1,
		"0.0001": 0,
		"1.001":  0,
		"2":      0,
		"-1":     0,
		".5":     0,
		"0x1":    0,
		"":       0,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, parseQ(in))
		})
	}
}

func TestMiddleware_WithEncoders(t *testing.T) {
	w := serve(t, write("text/plain", text), get("zstd, br, gzip"), WithEncoders(Gzip(6), Brotli(4), Zstd(3)))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	assert.Equal(t, text, decode(t, "gzip", w.Body.Bytes()))

	w = serve(t, write("text/plain", text), get("gzip"), WithEncoders(Zstd(3)))
	assert.Empty(t, w.Header().Get("Content-Encoding"))
}

func TestMiddleware_WriteString(t *testing.T) {
	writeString := func(ct string) fox.HandlerFunc {
		return func(c *fox.Context) {
			c.Writer().Header().Set("Content-Type", ct)
			for i := 0; i < len(text); i += 100 {
				_, err := c.Writer().WriteString(string(text[i : i+100]))
				require.NoError(t, err)
			}
		}
	}

	w := serve(t, writeString("text/plain"), get("gzip"))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	assert.Equal(t, text, decode(t, "gzip", w.Body.Bytes()))

	w = serve(t, writeString("image/png"), get("gzip"))
	assert.Empty(t, w.Header().Get("Content-Encoding"))
	assert.Equal(t, text, w.Body.Bytes())
}

func TestMiddleware_Encodings(t *testing.T) {
	for _, enc := range []string{"gzip", "br", "zstd"} {
		t.Run(enc, func(t *testing.T) {
			w := serve(t, func(c *fox.Context) {
				c.Writer().Header().Set("Content-Length", strconv.Itoa(len(text)))
				c.Writer().Header().Set("Accept-Ranges", "bytes")
				_, _ = c.Writer().Write(text)
			}, get(enc))
			assert.Equal(t, enc, w.Header().Get("Content-Encoding"))
			assert.Equal(t, "Accept-Encoding", w.Header().Get("Vary"))
			assert.Empty(t, w.Header().Get("Content-Length"))
			assert.Empty(t, w.Header().Get("Accept-Ranges"))
			assert.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"))
			assert.Equal(t, text, decode(t, enc, w.Body.Bytes()))
		})
	}
}

func TestMiddleware_MinSize(t *testing.T) {
	oneByte := func(n int) fox.HandlerFunc {
		return func(c *fox.Context) {
			for i := range n {
				_, _ = c.Writer().Write(text[i : i+1])
			}
		}
	}

	w := serve(t, oneByte(99), get("gzip"), WithMinSize(100))
	assert.Empty(t, w.Header().Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", w.Header().Get("Vary"))
	assert.Equal(t, text[:99], w.Body.Bytes())

	w = serve(t, oneByte(100), get("gzip"), WithMinSize(100))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	assert.Equal(t, text[:100], decode(t, "gzip", w.Body.Bytes()))

	w = serve(t, oneByte(1), get("gzip"), WithMinSize(0))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	assert.Equal(t, text[:1], decode(t, "gzip", w.Body.Bytes()))
}

func TestMiddleware_ContentLength(t *testing.T) {
	t.Run("small declared length is sent at once", func(t *testing.T) {
		mw, err := Middleware()
		require.NoError(t, err)
		f := fox.MustRouter(fox.WithMiddleware(mw))
		w := httptest.NewRecorder()
		f.MustAdd(fox.MethodGet, "/", func(c *fox.Context) {
			c.Writer().Header().Set("Content-Length", "10")
			_, _ = c.Writer().Write(text[:10])
			assert.Equal(t, 10, w.Body.Len())
		})
		f.ServeHTTP(w, get("gzip"))
		assert.Empty(t, w.Header().Get("Content-Encoding"))
		assert.Equal(t, "Accept-Encoding", w.Header().Get("Vary"))
	})

	t.Run("shorter body", func(t *testing.T) {
		assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
			serve(t, func(c *fox.Context) {
				c.Writer().Header().Set("Content-Length", strconv.Itoa(len(text)+1))
				_, _ = c.Writer().Write(text)
			}, get("gzip"))
		})
	})

	t.Run("longer body", func(t *testing.T) {
		assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
			serve(t, func(c *fox.Context) {
				c.Writer().Header().Set("Content-Length", strconv.Itoa(len(text)-1))
				_, err := c.Writer().Write(text)
				assert.ErrorIs(t, err, http.ErrContentLength)
			}, get("gzip"))
		})
	})

	t.Run("longer body while buffering", func(t *testing.T) {
		w := serve(t, func(c *fox.Context) {
			c.Writer().Header().Set("Content-Length", "300")
			for i := range 3 {
				_, err := c.Writer().Write(text[i*100 : (i+1)*100])
				require.NoError(t, err)
			}
			_, err := c.Writer().Write(text[300:400])
			assert.ErrorIs(t, err, http.ErrContentLength)
		}, get("gzip"), WithMinSize(100))
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		assert.Equal(t, text[:300], decode(t, "gzip", w.Body.Bytes()))
	})

	t.Run("longer body after compression started", func(t *testing.T) {
		w := serve(t, func(c *fox.Context) {
			c.Writer().Header().Set("Content-Type", "text/plain")
			c.Writer().Header().Set("Content-Length", strconv.Itoa(len(text)))
			_, err := c.Writer().Write(text)
			require.NoError(t, err)
			_, err = c.Writer().Write(text[:1])
			assert.ErrorIs(t, err, http.ErrContentLength)
		}, get("gzip"))
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		assert.Equal(t, text, decode(t, "gzip", w.Body.Bytes()))
	})

	t.Run("large declared length is compressed at once", func(t *testing.T) {
		w := serve(t, func(c *fox.Context) {
			c.Writer().Header().Set("Content-Length", strconv.Itoa(len(text)))
			_, _ = c.Writer().Write(nil)
			_, _ = c.Writer().Write([]byte{})
			_, _ = c.Writer().Write(text[:10])
			_, _ = c.Writer().Write(text[10:])
		}, get("gzip"))
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		assert.Equal(t, text, decode(t, "gzip", w.Body.Bytes()))
	})
}

func TestMiddleware_PassthroughIsImmediate(t *testing.T) {
	mw, err := Middleware()
	require.NoError(t, err)
	f := fox.MustRouter(fox.WithMiddleware(mw))
	w := httptest.NewRecorder()
	f.MustAdd(fox.MethodGet, "/", func(c *fox.Context) {
		c.Writer().Header().Set("Content-Type", "image/png")
		_, _ = c.Writer().Write(text[:10])
		assert.Equal(t, 10, w.Body.Len())
	})
	f.ServeHTTP(w, get("gzip"))
	assert.Empty(t, w.Header().Get("Content-Encoding"))
	assert.Empty(t, w.Header().Get("Vary"))
}

func TestMiddleware_ContentTypes(t *testing.T) {
	cases := []struct {
		name     string
		ct       string
		opts     []Option
		compress bool
	}{
		{"default text", "text/html; charset=utf-8", nil, true},
		{"default svg", "image/svg+xml", nil, true},
		{"default png", "image/png", nil, false},
		{"default video", "video/mp4", nil, false},
		{"default grpc", "application/grpc", nil, false},
		{"default case insensitive", "IMAGE/PNG", nil, false},
		{"malformed", "html", nil, false},
		{"excluded", "application/json", []Option{WithExcludedContentTypes("application/json")}, false},
		{"excluded keeps defaults", "image/png", []Option{WithExcludedContentTypes("application/json")}, false},
		{"excluded wildcard", "text/css", []Option{WithExcludedContentTypes("text/*")}, false},
		{"included", "application/json", []Option{WithIncludedContentTypes("application/json")}, true},
		{"not included", "text/html", []Option{WithIncludedContentTypes("application/json")}, false},
		{"included wildcard", "text/css", []Option{WithIncludedContentTypes("text/*")}, true},
		{"included replaces defaults", "image/png", []Option{WithIncludedContentTypes("image/*")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(t, write(tc.ct, text), get("gzip"), tc.opts...)
			if tc.compress {
				assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
				return
			}
			assert.Empty(t, w.Header().Get("Content-Encoding"))
			assert.Equal(t, text, w.Body.Bytes())
		})
	}
}

func TestMiddleware_Sniff(t *testing.T) {
	body := append([]byte("<!DOCTYPE html>"), text...)
	w := serve(t, write("", body), get("gzip"))
	assert.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))

	w = serve(t, func(c *fox.Context) {
		_, _ = c.Writer().Write([]byte("\n"))
		_, _ = c.Writer().Write(body)
	}, get("gzip"))
	assert.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))

	w = serve(t, func(c *fox.Context) {
		c.Writer().Header()["Content-Type"] = nil
		_, _ = c.Writer().Write(body)
	}, get("gzip"))
	assert.Empty(t, w.Header().Get("Content-Type"))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))

	w = serve(t, func(c *fox.Context) {
		c.Writer().Header()["Content-Type"] = nil
		_, _ = c.Writer().Write(body)
	}, get("gzip"), WithIncludedContentTypes("text/html"))
	assert.Empty(t, w.Header().Get("Content-Encoding"))

	w = serve(t, func(c *fox.Context) {
		c.Writer().Header().Set("Content-Length", strconv.Itoa(len(body)))
		for i := range body {
			_, _ = c.Writer().Write(body[i : i+1])
		}
	}, get("gzip"))
	assert.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))

	w = serve(t, func(c *fox.Context) {
		c.Writer().Header().Set("Cache-Control", "no-transform")
		for i := range body {
			_, _ = c.Writer().Write(body[i : i+1])
		}
	}, get("gzip"))
	assert.Empty(t, w.Header().Get("Content-Encoding"))
	assert.Empty(t, w.Header().Values("Content-Type"))

	w = serve(t, func(c *fox.Context) {
		c.Writer().Header().Set("Content-Encoding", "gzip")
		_, _ = c.Writer().Write(body)
	}, get("gzip"))
	assert.Empty(t, w.Header().Values("Content-Type"))

	w = serve(t, func(c *fox.Context) {
		for i := 0; i < len(body); i += 4 {
			_, _ = c.Writer().Write(body[i:min(i+4, len(body))])
		}
	}, get("gzip"), WithMinSize(10))
	assert.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))

	w = serve(t, func(c *fox.Context) {
		for i := 0; i < len(body); i += 100 {
			_, _ = c.Writer().Write(body[i:min(i+100, len(body))])
		}
	}, get("gzip"), WithIncludedContentTypes("text/html"))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	assert.Equal(t, body, decode(t, "gzip", w.Body.Bytes()))
}

func TestMiddleware_Skip(t *testing.T) {
	cases := []struct {
		name    string
		handler fox.HandlerFunc
		req     *http.Request
		vary    bool
	}{
		{
			name:    "no accept encoding",
			handler: write("text/plain", text),
			req:     get(""),
		},
		{
			name:    "head",
			handler: write("text/plain", text),
			req: func() *http.Request {
				r := get("gzip")
				r.Method = http.MethodHead
				return r
			}(),
		},
		{
			name: "content encoding set",
			handler: func(c *fox.Context) {
				c.Writer().Header().Set("Content-Encoding", "identity")
				_, _ = c.Writer().Write(text)
			},
			req: get("gzip"),
		},
		{
			name: "content range set",
			handler: func(c *fox.Context) {
				c.Writer().Header().Set("Content-Range", "bytes 0-9/100")
				_, _ = c.Writer().Write(text)
			},
			req:  get("gzip"),
			vary: true,
		},
		{
			name: "no transform",
			handler: func(c *fox.Context) {
				c.Writer().Header().Set("Cache-Control", "public, No-Transform")
				_, _ = c.Writer().Write(text)
			},
			req: get("gzip"),
		},
		{
			name:    "request no transform",
			handler: write("text/plain", text),
			req: func() *http.Request {
				r := get("gzip")
				r.Header.Set("Cache-Control", "no-transform")
				return r
			}(),
		},
		{
			name: "partial content",
			handler: func(c *fox.Context) {
				c.Writer().WriteHeader(http.StatusPartialContent)
				_, _ = c.Writer().Write(text)
			},
			req:  get("gzip"),
			vary: true,
		},
		{
			name: "not modified",
			handler: func(c *fox.Context) {
				c.Writer().WriteHeader(http.StatusNotModified)
			},
			req:  get("gzip"),
			vary: true,
		},
		{
			name: "no content",
			handler: func(c *fox.Context) {
				c.Writer().WriteHeader(http.StatusNoContent)
			},
			req: get("gzip"),
		},
		{
			name: "reset content",
			handler: func(c *fox.Context) {
				c.Writer().WriteHeader(http.StatusResetContent)
				require.NoError(t, c.Writer().FlushError())
			},
			req: get("gzip"),
		},
		{
			name: "switching protocols",
			handler: func(c *fox.Context) {
				c.Writer().WriteHeader(http.StatusSwitchingProtocols)
				_, _ = c.Writer().Write(text)
			},
			req: get("gzip"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(t, tc.handler, tc.req)
			assert.NotEqual(t, "gzip", w.Header().Get("Content-Encoding"))
			if tc.vary {
				assert.Equal(t, "Accept-Encoding", w.Header().Get("Vary"))
				return
			}
			assert.Empty(t, w.Header().Get("Vary"))
		})
	}
}

func TestMiddleware_Vary(t *testing.T) {
	w := serve(t, func(c *fox.Context) {
		c.Writer().Header().Set("Vary", "Origin")
		_, _ = c.Writer().Write(text)
	}, get("gzip"))
	assert.Equal(t, []string{"Origin", "Accept-Encoding"}, w.Header().Values("Vary"))

	w = serve(t, func(c *fox.Context) {
		c.Writer().Header().Set("Vary", "Origin, accept-encoding")
		_, _ = c.Writer().Write(text)
	}, get("gzip"))
	assert.Equal(t, []string{"Origin, accept-encoding"}, w.Header().Values("Vary"))
}

func TestMiddleware_ETag(t *testing.T) {
	cases := map[string]string{
		`"abc"`:   `W/"abc"`,
		`W/"abc"`: `W/"abc"`,
	}
	for in, want := range cases {
		w := serve(t, func(c *fox.Context) {
			c.Writer().Header().Set("ETag", in)
			_, _ = c.Writer().Write(text)
		}, get("gzip"))
		assert.Equal(t, want, w.Header().Get("ETag"))
	}

	w := serve(t, func(c *fox.Context) {
		c.Writer().Header().Set("ETag", `"abc"`)
		_, _ = c.Writer().Write(text[:10])
	}, get("gzip"))
	assert.Equal(t, `"abc"`, w.Header().Get("ETag"))
}

func TestMiddleware_Informational(t *testing.T) {
	mw, err := Middleware()
	require.NoError(t, err)
	f := fox.MustRouter(fox.WithMiddleware(mw))
	f.MustAdd(fox.MethodGet, "/", func(c *fox.Context) {
		c.Writer().Header().Set("Link", "</style.css>; rel=preload")
		c.Writer().WriteHeader(http.StatusEarlyHints)
		c.Writer().WriteHeader(http.StatusCreated)
		_, _ = c.Writer().Write(text)
	})
	srv := httptest.NewServer(f)
	defer srv.Close()

	var informational []int
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			informational = append(informational, code)
			return nil
		},
	}))
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, []int{http.StatusEarlyHints}, informational)
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, text, decode(t, "gzip", body))
}

func TestMiddleware_Flush(t *testing.T) {
	t.Run("without data", func(t *testing.T) {
		w := serve(t, func(c *fox.Context) {
			c.Writer().Header().Set("Content-Type", "text/event-stream")
			require.NoError(t, c.Writer().FlushError())
			_, _ = c.Writer().Write(text[:10])
			require.NoError(t, c.Writer().FlushError())
		}, get("gzip"))
		assert.True(t, w.Flushed)
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		assert.Equal(t, text[:10], decode(t, "gzip", w.Body.Bytes()))
	})

	t.Run("without body", func(t *testing.T) {
		w := serve(t, func(c *fox.Context) {
			require.NoError(t, c.Writer().FlushError())
		}, get("gzip"))
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		assert.Empty(t, decode(t, "gzip", w.Body.Bytes()))
	})

	t.Run("with data", func(t *testing.T) {
		w := serve(t, func(c *fox.Context) {
			_, _ = c.Writer().Write(text[:10])
			require.NoError(t, c.Writer().FlushError())
			_, _ = c.Writer().Write(text[10:])
			require.NoError(t, c.Writer().FlushError())
		}, get("gzip"))
		assert.True(t, w.Flushed)
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
		assert.Equal(t, text, decode(t, "gzip", w.Body.Bytes()))
	})

	t.Run("small declared length", func(t *testing.T) {
		w := serve(t, func(c *fox.Context) {
			c.Writer().Header().Set("Content-Length", "10")
			_, _ = c.Writer().Write(text[:10])
			require.NoError(t, c.Writer().FlushError())
		}, get("gzip"))
		assert.Empty(t, w.Header().Get("Content-Encoding"))
	})
}

func TestMiddleware_Panic(t *testing.T) {
	mw, err := Middleware()
	require.NoError(t, err)
	f := fox.MustRouter(fox.WithMiddleware(mw))
	f.MustAdd(fox.MethodGet, "/", func(c *fox.Context) {
		_, _ = c.Writer().Write(text)
		require.NoError(t, c.Writer().FlushError())
		panic(http.ErrAbortHandler)
	})
	w := httptest.NewRecorder()
	assert.Panics(t, func() {
		f.ServeHTTP(w, get("gzip"))
	})
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)
	_, err = io.ReadAll(zr)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestMiddleware_OuterWriter(t *testing.T) {
	mw, err := Middleware()
	require.NoError(t, err)

	var status, size int
	var written bool
	outer := func(next fox.HandlerFunc) fox.HandlerFunc {
		return func(c *fox.Context) {
			next(c)
			status, size, written = c.Writer().Status(), c.Writer().Size(), c.Writer().Written()
		}
	}
	f := fox.MustRouter(fox.WithMiddleware(outer, mw))
	f.MustAdd(fox.MethodGet, "/created", func(c *fox.Context) {
		c.Writer().WriteHeader(http.StatusCreated)
		_, _ = c.Writer().Write(text)
	})
	f.MustAdd(fox.MethodGet, "/empty", func(c *fox.Context) {})

	w := httptest.NewRecorder()
	f.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/created", nil))
	assert.Equal(t, http.StatusCreated, status)
	assert.True(t, written)
	assert.Equal(t, w.Body.Len(), size)

	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/empty", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	f.ServeHTTP(w, req)
	assert.False(t, written)
	assert.Empty(t, w.Header())
}

func TestMiddleware_ReadFrom(t *testing.T) {
	w := serve(t, func(c *fox.Context) {
		_, _ = c.Writer().ReadFrom(bytes.NewReader(text))
	}, get("gzip"))
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	assert.Equal(t, text, decode(t, "gzip", w.Body.Bytes()))
}

func TestMiddleware_Config(t *testing.T) {
	cases := map[string][]Option{
		"no encoder":         {WithEncoders()},
		"nil encoder":        {WithEncoders(nil)},
		"duplicate encoding": {WithEncoders(Gzip(6), Gzip(1))},
		"gzip level":         {WithEncoders(Gzip(10))},
		"brotli level":       {WithEncoders(Brotli(12))},
		"zstd level":         {WithEncoders(Zstd(0))},
		"negative min size":  {WithMinSize(-1)},
		"invalid media type": {WithIncludedContentTypes("text/")},
		"media type params":  {WithExcludedContentTypes("text/html; charset=utf-8")},
		"no subtype":         {WithExcludedContentTypes("text")},
		"no included type":   {WithIncludedContentTypes()},
		"no excluded type":   {WithExcludedContentTypes()},
		"wildcard type":      {WithExcludedContentTypes("*/html")},
		"partial wildcard":   {WithIncludedContentTypes("text/*+json")},
		"invalid encoding":   {WithEncoders(namedEncoder("gzip, br"))},
		"wildcard encoding":  {WithEncoders(namedEncoder("*"))},
		"identity encoding":  {WithEncoders(namedEncoder("identity"))},
		"include and exclude": {
			WithIncludedContentTypes("text/html"),
			WithExcludedContentTypes("image/png"),
		},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Middleware(opts...)
			assert.ErrorIs(t, err, ErrInvalidConfig)
		})
	}
}

type namedEncoder string

func (e namedEncoder) Encoding() string { return string(e) }

func (e namedEncoder) NewWriter(w io.Writer) Writer { return Gzip(6).NewWriter(w) }

func TestMiddleware_ReverseProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < len(text); i += 100 {
			_, _ = w.Write(text[i:min(i+100, len(text))])
			http.NewResponseController(w).Flush()
		}
	}))
	defer backend.Close()

	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Header.Del("Accept-Encoding")
	}}

	mw, err := Middleware()
	require.NoError(t, err)
	f := fox.MustRouter(fox.WithMiddleware(mw))
	f.MustAdd(fox.MethodGet, "/", fox.WrapH(proxy))
	srv := httptest.NewServer(f)
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	var wg sync.WaitGroup
	for _, enc := range []string{"gzip", "br", "zstd", "gzip", "br", "zstd"} {
		wg.Go(func() {
			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			require.NoError(t, err)
			req.Header.Set("Accept-Encoding", enc)
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, enc, resp.Header.Get("Content-Encoding"))
			assert.Equal(t, text, decode(t, enc, body))
		})
	}
	wg.Wait()
}

func TestMiddleware_Hijack(t *testing.T) {
	mw, err := Middleware()
	require.NoError(t, err)
	f := fox.MustRouter(fox.WithMiddleware(mw))
	f.MustAdd(fox.MethodGet, "/", func(c *fox.Context) {
		conn, _, err := c.Writer().Hijack()
		require.NoError(t, err)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
		_ = conn.Close()
	})
	srv := httptest.NewServer(f)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Empty(t, resp.Header.Get("Content-Encoding"))
	assert.Equal(t, "ok", string(body))
}

type discardWriter struct {
	h http.Header
}

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(int)             {}

func BenchmarkMiddleware(b *testing.B) {
	mw, err := Middleware()
	require.NoError(b, err)
	f := fox.MustRouter(fox.WithMiddleware(mw))
	f.MustAdd(fox.MethodGet, "/image", write("image/png", text))
	f.MustAdd(fox.MethodGet, "/text", write("text/plain", text))
	var chunks []string
	for i := 0; i < len(text); i += 100 {
		chunks = append(chunks, string(text[i:i+100]))
	}
	f.MustAdd(fox.MethodGet, "/strings", func(c *fox.Context) {
		c.Writer().Header().Set("Content-Type", "text/plain")
		for _, chunk := range chunks {
			_, _ = c.Writer().WriteString(chunk)
		}
	})

	cases := []struct {
		name string
		path string
		ae   string
	}{
		{"no accept encoding", "/text", ""},
		{"excluded type", "/image", "gzip"},
		{"gzip", "/text", "gzip"},
		{"gzip write string", "/strings", "gzip"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.ae != "" {
				req.Header.Set("Accept-Encoding", tc.ae)
			}
			w := &discardWriter{h: make(http.Header)}
			b.ReportAllocs()
			for b.Loop() {
				clear(w.h)
				f.ServeHTTP(w, req)
			}
		})
	}
}
