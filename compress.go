// Copyright 2026 Sylvain Müller. All rights reserved.
// Mount of this source code is governed by a MIT license that can be found
// at https://github.com/fox-toolkit/compress/blob/main/LICENSE.txt.

package compress

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/fox-toolkit/fox"
)

type middleware struct {
	pool     sync.Pool
	encoders []*encoder
	filter   []mediaType
	minSize  int
	include  bool
}

type mediaType struct {
	typ string
	sub string
}

// Middleware returns a [fox.MiddlewareFunc] that compresses responses with the encoding preferred by the client,
// among the configured encoders.
func Middleware(opts ...Option) (fox.MiddlewareFunc, error) {
	m, err := newMiddleware(opts...)
	if err != nil {
		return nil, err
	}
	return m.run, nil
}

func newMiddleware(opts ...Option) (*middleware, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		if err := opt.apply(cfg); err != nil {
			return nil, err
		}
	}
	if cfg.include && cfg.exclude {
		return nil, fmt.Errorf("%w: included and excluded content types are mutually exclusive", ErrInvalidConfig)
	}

	m := &middleware{
		minSize: cfg.minSize,
		include: cfg.include,
	}
	types := cfg.included
	if !cfg.include {
		types = append(append([]string(nil), defaultExcluded...), cfg.excluded...)
	}
	for _, t := range types {
		typ, sub, _ := strings.Cut(t, "/")
		m.filter = append(m.filter, mediaType{typ: typ, sub: sub})
	}
	for _, e := range cfg.encoders {
		m.encoders = append(m.encoders, newEncoder(e))
	}
	m.pool.New = func() any {
		return new(writer)
	}
	return m, nil
}

func (m *middleware) run(next fox.HandlerFunc) fox.HandlerFunc {
	return func(c *fox.Context) {
		req := c.Request()
		if req.Method == http.MethodHead || req.Method == http.MethodConnect || noTransform(req.Header) {
			next(c)
			return
		}
		enc := m.negotiate(req.Header.Values("Accept-Encoding"))
		if enc == nil {
			next(c)
			return
		}

		w := m.pool.Get().(*writer)
		w.reset(c.Writer(), enc, m)
		cc := c.CloneWith(w, req)
		defer func() {
			cc.Close()
			w.release()
			m.pool.Put(w)
		}()

		next(cc)
		// Not reached on panic, so a truncated body never ends with a valid trailer.
		w.close()
	}
}

func (m *middleware) negotiate(values []string) *encoder {
	if len(values) == 0 {
		return nil
	}
	star, hasStar := weight(values, "*")
	var best *encoder
	bestQ := 0
	for _, e := range m.encoders {
		q, ok := weight(values, e.name)
		if !ok && e.name == "gzip" {
			q, ok = weight(values, "x-gzip")
		}
		if !ok && hasStar {
			q, ok = star, true
		}
		if ok && q > bestQ {
			best, bestQ = e, q
		}
	}
	return best
}

// weight returns the weight in thousandths of the first occurrence of coding in the Accept-Encoding field.
func weight(values []string, coding string) (int, bool) {
	for _, v := range values {
		for item := range strings.SplitSeq(v, ",") {
			name, params, _ := strings.Cut(item, ";")
			if !strings.EqualFold(trimOWS(name), coding) {
				continue
			}
			q := 1000
			for p := range strings.SplitSeq(params, ";") {
				k, val, _ := strings.Cut(trimOWS(p), "=")
				if strings.EqualFold(trimOWS(k), "q") {
					q = parseQ(trimOWS(val))
					break
				}
			}
			return q, true
		}
	}
	return 0, false
}

// parseQ parses a qvalue as defined in RFC 9110, section 12.4.2. A malformed value is not acceptable.
func parseQ(s string) int {
	if s == "" || len(s) > 5 || (s[0] != '0' && s[0] != '1') {
		return 0
	}
	if len(s) == 1 {
		return int(s[0]-'0') * 1000
	}
	if s[1] != '.' {
		return 0
	}
	q, mul := int(s[0]-'0')*1000, 100
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		q += int(s[i]-'0') * mul
		mul /= 10
	}
	if q > 1000 {
		return 0
	}
	return q
}

func trimOWS(s string) string {
	return strings.Trim(s, " \t")
}

func (m *middleware) allowed(ct string) bool {
	if ct == "" {
		return !m.include
	}
	mt, _, _ := strings.Cut(ct, ";")
	mt = trimOWS(mt)
	typ, sub, ok := strings.Cut(mt, "/")
	if !ok || typ == "" || sub == "" {
		return false
	}
	for _, f := range m.filter {
		if (f.typ == "*" || strings.EqualFold(typ, f.typ)) && (f.sub == "*" || strings.EqualFold(sub, f.sub)) {
			return m.include
		}
	}
	return !m.include
}
