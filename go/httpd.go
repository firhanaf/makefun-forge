package main

// A lean HTTP/1.1 front end. net/http's server spends far more per request than the forge's handlers
// do: every request without a body starts a background read (an extra goroutine, a read syscall and a
// poller arm) to notice a client hang-up, and its idle connections run timers and state hooks. Every
// workload the forge is measured on is one request at a time on a keep-alive connection, so that
// overhead is the latency. This server keeps net/http's request parser (http.ReadRequest) and the
// handlers' http.ResponseWriter interface, buffers each response and sends it with one write.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// bufferedResponse is the http.ResponseWriter a handler writes into; serveConn sends it in one write.
type bufferedResponse struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (w *bufferedResponse) Header() http.Header { return w.header }

func (w *bufferedResponse) WriteHeader(code int) {
	if w.wroteHeader || code < 100 || code > 999 {
		return
	}
	w.status = code
	w.wroteHeader = true
}

func (w *bufferedResponse) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(p)
}

func (w *bufferedResponse) WriteString(s string) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.WriteString(s)
}

// Flush is a no-op: the response goes out whole once the handler returns.
func (w *bufferedResponse) Flush() {}

func (w *bufferedResponse) reset() {
	for k := range w.header {
		delete(w.header, k)
	}
	w.status = http.StatusOK
	w.wroteHeader = false
	w.body.Reset()
}

// httpDate answers the Date header for the current second, formatted once a second.
type dateCache struct {
	sec  atomic.Int64
	text atomic.Pointer[string]
}

var dates dateCache

func (d *dateCache) now() string {
	t := time.Now()
	sec := t.Unix()
	if sec == d.sec.Load() {
		if p := d.text.Load(); p != nil {
			return *p
		}
	}
	s := t.UTC().Format(http.TimeFormat)
	d.text.Store(&s)
	d.sec.Store(sec)
	return s
}

var connPool = sync.Pool{New: func() any {
	return &connState{resp: bufferedResponse{header: http.Header{}}}
}}

type connState struct {
	resp bufferedResponse
	out  []byte
}

// serveHTTP1 accepts connections on ln and serves each on its own goroutine until ln is closed.
func serveHTTP1(ln net.Listener, h http.Handler) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			return err
		}
		go serveConn(c, h)
	}
}

func serveConn(c net.Conn, h http.Handler) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 16<<10)
	st := connPool.Get().(*connState)
	defer connPool.Put(st)
	remote := c.RemoteAddr().String()
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF && !isClosedConn(err) {
				c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\nContent-Length: 11\r\n\r\nBad Request"))
			}
			return
		}
		req.RemoteAddr = remote
		if req.ContentLength != 0 && strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
			if _, err := c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
				return
			}
		}
		closeAfter := req.Close
		st.resp.reset()
		if !runHandler(h, &st.resp, req) {
			closeAfter = true
		}
		// Whatever body the handler left unread must be consumed before the next request can be parsed.
		if req.Body != nil && req.Body != http.NoBody {
			if _, err := io.Copy(io.Discard, req.Body); err != nil {
				closeAfter = true
			}
			req.Body.Close()
		}
		st.out = appendResponse(st.out[:0], &st.resp, req.Method == http.MethodHead, closeAfter)
		if _, err := c.Write(st.out); err != nil {
			return
		}
		if cap(st.out) > 1<<20 {
			st.out = nil // do not keep a pack-sized buffer alive in the pool
		}
		if st.resp.body.Cap() > 1<<20 {
			st.resp.body = bytes.Buffer{}
		}
		if closeAfter {
			return
		}
	}
}

// runHandler runs h, answering 500 for a panic as net/http does (logged, connection closed).
func runHandler(h http.Handler, w *bufferedResponse, r *http.Request) (ok bool) {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "[forge] panic serving %s %s: %v\n", r.Method, r.URL.Path, p)
			w.reset()
			w.header.Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			w.body.WriteString(`{"message":"Internal Server Error"}`)
			ok = false
		}
	}()
	h.ServeHTTP(w, r)
	return true
}

func bodyAllowed(status int) bool {
	return !(status >= 100 && status < 200) && status != http.StatusNoContent && status != http.StatusNotModified
}

// appendResponse serializes the status line, headers and body. Content-Length is always the length of
// the body written (HEAD keeps it and drops the body), so a handler cannot frame a response wrong.
func appendResponse(out []byte, w *bufferedResponse, head, closeConn bool) []byte {
	status := w.status
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	text := http.StatusText(status)
	if text == "" {
		text = "status code " + strconv.Itoa(status)
	}
	out = append(out, text...)
	out = append(out, "\r\n"...)
	allowed := bodyAllowed(status)
	if allowed && w.body.Len() > 0 && w.header.Get("Content-Type") == "" {
		w.header.Set("Content-Type", http.DetectContentType(w.body.Bytes()))
	}
	for k, vs := range w.header {
		if k == "Content-Length" || k == "Connection" || k == "Transfer-Encoding" || k == "Date" {
			continue
		}
		for _, v := range vs {
			out = append(out, k...)
			out = append(out, ": "...)
			out = appendHeaderValue(out, v)
			out = append(out, "\r\n"...)
		}
	}
	out = append(out, "Date: "...)
	out = append(out, dates.now()...)
	out = append(out, "\r\n"...)
	if allowed {
		out = append(out, "Content-Length: "...)
		out = strconv.AppendInt(out, int64(w.body.Len()), 10)
		out = append(out, "\r\n"...)
	}
	if closeConn {
		out = append(out, "Connection: close\r\n"...)
	}
	out = append(out, "\r\n"...)
	if allowed && !head {
		out = append(out, w.body.Bytes()...)
	}
	return out
}

// appendHeaderValue drops CR and LF from a header value, as net/http does, so no value splits a header.
func appendHeaderValue(out []byte, v string) []byte {
	if strings.IndexAny(v, "\r\n") < 0 {
		return append(out, v...)
	}
	for i := 0; i < len(v); i++ {
		if v[i] == '\r' || v[i] == '\n' {
			out = append(out, ' ')
		} else {
			out = append(out, v[i])
		}
	}
	return out
}

func isClosedConn(err error) bool {
	if err == io.ErrUnexpectedEOF {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "use of closed network connection") || strings.Contains(s, "connection reset by peer")
}
