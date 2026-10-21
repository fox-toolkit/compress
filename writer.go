// Copyright 2026 Sylvain Müller. All rights reserved.
// Mount of this source code is governed by a MIT license that can be found
// at https://github.com/fox-toolkit/compress/blob/main/LICENSE.txt.

package compress

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fox-toolkit/compress/internal/bytesconv"
	"github.com/fox-toolkit/fox"
)

var _ fox.ResponseWriter = (*writer)(nil)

var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

const (
	undecided = iota
	plain
	compressed
)

type writer struct {
	w        fox.ResponseWriter
	enc      *encoder
	zw       Writer
	m        *middleware
	buf      []byte
	code     int
	size     int
	declared int64
	state    uint8
	written  bool
	hijacked bool
}

func (w *writer) reset(rw fox.ResponseWriter, enc *encoder, m *middleware) {
	w.w = rw
	w.enc = enc
	w.m = m
	w.buf = w.buf[:0]
	w.code = http.StatusOK
	w.size = 0
	w.declared = -1
	w.state = undecided
	w.written = false
	w.hijacked = false
}

func (w *writer) release() {
	if w.zw != nil {
		w.enc.put(w.zw)
		w.zw = nil
	}
	w.w = nil
	w.enc = nil
	w.m = nil
}

// Status recorded after Write or WriteHeader.
func (w *writer) Status() int {
	return w.code
}

// Written returns true if the response has been written.
func (w *writer) Written() bool {
	return w.written
}

// Size returns the size of the written response.
func (w *writer) Size() int {
	return w.size
}

func (w *writer) Header() http.Header {
	return w.w.Header()
}

// WriteHeader sends an HTTP response header with the provided
// status code. See [http.ResponseWriter] for more details.
func (w *writer) WriteHeader(code int) {
	if code < 100 || code > 999 {
		panic(fmt.Sprintf("invalid status code %d", code))
	}
	if w.hijacked {
		logf("http: response.WriteHeader on hijacked connection")
		return
	}
	if w.written {
		logf("http: superfluous response.WriteHeader call")
		return
	}
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		w.w.WriteHeader(code)
		return
	}
	w.code = code
	w.written = true
	if code == http.StatusSwitchingProtocols {
		w.state = plain
		w.w.WriteHeader(code)
	}
}

// Write writes the data to the connection as part of an HTTP reply.
// See [http.ResponseWriter] for more details.
func (w *writer) Write(p []byte) (int, error) {
	if w.hijacked {
		return 0, http.ErrHijacked
	}
	w.written = true
	if w.state == undecided {
		if len(p) == 0 {
			return 0, nil
		}
		if cl, ok := contentLength(w.w.Header()); ok && int64(w.size+len(p)) > cl {
			return 0, http.ErrContentLength
		}
		if len(w.buf)+len(p) < w.threshold() && !w.decidable() {
			w.buf = append(w.buf, p...)
			w.size += len(p)
			return len(p), nil
		}
		if err := w.commit(w.large(len(w.buf)+len(p)), p); err != nil {
			return 0, err
		}
	}

	if w.declared >= 0 && int64(w.size+len(p)) > w.declared {
		return 0, http.ErrContentLength
	}
	n, err := w.send(p)
	w.size += n
	return n, err
}

// WriteString writes the provided string to the underlying connection
// as part of an HTTP reply. The method returns the number of bytes written
// and an error, if any.
func (w *writer) WriteString(s string) (int, error) {
	if w.state == plain && !w.hijacked {
		n, err := w.w.WriteString(s)
		w.size += n
		return n, err
	}
	return w.Write(bytesconv.Bytes(s))
}

// ReadFrom reads data from src until EOF or error. The return value n is the number of bytes read.
// Any error except EOF encountered during the read is also returned.
func (w *writer) ReadFrom(src io.Reader) (n int64, err error) {
	bufp := copyBufPool.Get().(*[]byte)
	n, err = io.CopyBuffer(onlyWrite{w}, src, *bufp)
	copyBufPool.Put(bufp)
	return
}

// FlushError flushes buffered data to the client. If flush is not supported, FlushError returns an error
// matching [http.ErrNotSupported]. See [http.Flusher] for more details.
func (w *writer) FlushError() error {
	if w.hijacked {
		return http.ErrHijacked
	}
	w.written = true
	if w.state == undecided {
		// Streaming responses flush early, possibly before any write (e.g. httputil.ReverseProxy),
		// so they are compressed whatever the size of the data written so far.
		if err := w.commit(w.large(w.m.minSize), nil); err != nil {
			return err
		}
	}
	if w.zw != nil {
		if err := w.zw.Flush(); err != nil {
			return err
		}
	}
	return w.w.FlushError()
}

// Hijack lets the caller take over the connection. If hijacking the connection is not supported, Hijack returns
// an error matching [http.ErrNotSupported]. See [http.Hijacker] for more details.
func (w *writer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.w.Hijack()
	if err == nil {
		w.hijacked = true
		w.written = true
		w.size = 0
	}
	return conn, rw, err
}

// Push initiates an HTTP/2 server push. Push returns [http.ErrNotSupported] if the client has disabled push or if push
// is not supported on the underlying connection. See [http.Pusher] for more details.
func (w *writer) Push(target string, opts *http.PushOptions) error {
	return w.w.Push(target, opts)
}

// SetReadDeadline sets the deadline for reading the entire request, including the body. Reads from the request
// body after the deadline has been exceeded will return an error. A zero value means no deadline. Setting the read
// deadline after it has been exceeded will not extend it. If SetReadDeadline is not supported, it returns
// an error matching [http.ErrNotSupported].
func (w *writer) SetReadDeadline(deadline time.Time) error {
	return w.w.SetReadDeadline(deadline)
}

// SetWriteDeadline sets the deadline for writing the response. Writes to the response body after the deadline has
// been exceeded will not block, but may succeed if the data has been buffered. A zero value means no deadline.
// Setting the write deadline after it has been exceeded will not extend it. If SetWriteDeadline is not supported,
// it returns an error matching [http.ErrNotSupported].
func (w *writer) SetWriteDeadline(deadline time.Time) error {
	return w.w.SetWriteDeadline(deadline)
}

// EnableFullDuplex indicates that the request handler will interleave reads from [http.Request.Body]
// with writes to the [ResponseWriter].
//
// For HTTP/1 requests, the Go HTTP server by default consumes any unread portion of
// the request body before beginning to write the response, preventing handlers from
// concurrently reading from the request and writing the response.
// Calling EnableFullDuplex disables this behavior and permits handlers to continue to read
// from the request while concurrently writing the response.
//
// For HTTP/2 requests, the Go HTTP server always permits concurrent reads and responses.
// If EnableFullDuplex is not supported, it returns an error matching [http.ErrNotSupported].
func (w *writer) EnableFullDuplex() error {
	return w.w.EnableFullDuplex()
}

func (w *writer) Unwrap() http.ResponseWriter {
	return w.w
}

func (w *writer) close() {
	if w.hijacked || !w.written {
		return
	}
	if w.state == undecided {
		if err := w.commit(w.large(len(w.buf)), nil); err != nil {
			return
		}
	}
	// Content-Length is removed when compressing, so a body that does not match it must abort the response
	// like net/http does.
	if w.state == compressed && w.declared >= 0 && int64(w.size) != w.declared {
		panic(http.ErrAbortHandler)
	}
	if w.state == compressed {
		if w.zw == nil {
			w.zw = w.enc.get(w.w)
		}
		_ = w.zw.Close()
	}
}

func (w *writer) decidable() bool {
	h := w.w.Header()
	cl, hasCL := contentLength(h)
	if hasCL && cl < int64(w.m.minSize) {
		return true
	}
	if _, ok := h["Content-Type"]; !ok {
		// The content type is sniffed at commit, so only the status and headers can decide early.
		return w.code == http.StatusNoContent || w.code == http.StatusResetContent || w.code == http.StatusPartialContent ||
			w.code == http.StatusNotModified ||
			h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" || noTransform(h)
	}
	return hasCL || !w.eligible(h, h.Get("Content-Type"))
}

// threshold returns the size to buffer before deciding. Without a Content-Type, it is at least the 512 bytes
// net/http uses to sniff the content type.
func (w *writer) threshold() int {
	if _, ok := w.w.Header()["Content-Type"]; !ok {
		return max(w.m.minSize, 512)
	}
	return w.m.minSize
}

func (w *writer) large(n int) bool {
	if cl, ok := contentLength(w.w.Header()); ok {
		return cl >= int64(w.m.minSize)
	}
	return n >= w.m.minSize
}

// varies reports whether the response representation depends on Accept-Encoding.
func (w *writer) varies(h http.Header, ct string) bool {
	if w.code == http.StatusNoContent || w.code == http.StatusResetContent || h.Get("Content-Encoding") != "" || noTransform(h) {
		return false
	}
	return w.m.allowed(ct)
}

// eligible reports whether the response may be compressed regardless of its size.
func (w *writer) eligible(h http.Header, ct string) bool {
	if w.code == http.StatusPartialContent || w.code == http.StatusNotModified || h.Get("Content-Range") != "" {
		return false
	}
	return w.varies(h, ct)
}

func (w *writer) commit(large bool, p []byte) error {
	h := w.w.Header()
	ct := h.Get("Content-Type")
	_, typed := h["Content-Type"]
	if !typed && len(w.buf)+len(p) > 0 {
		ct = sniff(w.buf, p)
	}

	w.state = plain
	if w.varies(h, ct) {
		addVary(h)
		if large && w.eligible(h, ct) {
			// net/http does not sniff the content type of an encoded body.
			if !typed && ct != "" {
				h.Set("Content-Type", ct)
			}
			if cl, ok := contentLength(h); ok {
				w.declared = cl
			}
			h.Set("Content-Encoding", w.enc.name)
			h.Del("Content-Length")
			h.Del("Accept-Ranges")
			if etag := h.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
				h.Set("ETag", "W/"+etag)
			}
			w.state = compressed
		}
	}
	w.w.WriteHeader(w.code)

	if len(w.buf) > 0 {
		buf := w.buf
		w.buf = w.buf[:0]
		if _, err := w.send(buf); err != nil {
			return err
		}
	}
	return nil
}

func (w *writer) send(p []byte) (int, error) {
	if w.state != compressed {
		return w.w.Write(p)
	}
	if w.zw == nil {
		w.zw = w.enc.get(w.w)
	}
	return w.zw.Write(p)
}

func sniff(buf, p []byte) string {
	switch {
	case len(buf) == 0:
		return http.DetectContentType(p)
	case len(buf) >= 512 || len(p) == 0:
		return http.DetectContentType(buf)
	}
	var b [512]byte
	n := copy(b[:], buf)
	n += copy(b[n:], p)
	return http.DetectContentType(b[:n])
}

func contentLength(h http.Header) (int64, bool) {
	v := h.Get("Content-Length")
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func noTransform(h http.Header) bool {
	for _, v := range h.Values("Cache-Control") {
		for d := range strings.SplitSeq(v, ",") {
			name, _, _ := strings.Cut(d, "=")
			if strings.EqualFold(trimOWS(name), "no-transform") {
				return true
			}
		}
	}
	return false
}

func addVary(h http.Header) {
	for _, v := range h.Values("Vary") {
		for token := range strings.SplitSeq(v, ",") {
			token = trimOWS(token)
			if token == "*" || strings.EqualFold(token, "Accept-Encoding") {
				return
			}
		}
	}
	h.Add("Vary", "Accept-Encoding")
}

type onlyWrite struct {
	w *writer
}

func (o onlyWrite) Write(p []byte) (int, error) {
	return o.w.Write(p)
}

func logf(msg string) {
	caller := relevantCaller()
	log.Printf("%s from %s (%s:%d)", msg, caller.Function, path.Base(caller.File), caller.Line)
}

func relevantCaller() runtime.Frame {
	pc := make([]uintptr, 16)
	n := runtime.Callers(1, pc)
	frames := runtime.CallersFrames(pc[:n])
	var frame runtime.Frame
	for {
		var more bool
		frame, more = frames.Next()
		if !strings.HasPrefix(frame.Function, "github.com/fox-toolkit/compress.") {
			return frame
		}
		if !more {
			break
		}
	}
	return frame
}
