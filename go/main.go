package main

// The forge's process: bind an ephemeral port, register it with the env, serve git smart HTTP, REST v3,
// GraphQL v4 and MCP from memory, and exit 0 once the env says the trial is done.

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Server struct {
	st      *Store
	reqSeq   atomic.Int64
	started  time.Time
	idPrefix string // "F0RG:<pid>:" of every X-GitHub-Request-Id
}

func newGzipReader(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) }

func itoa(n int) string { return strconv.Itoa(n) }

func (s *Server) requestID() string {
	n := s.reqSeq.Add(1)
	b := make([]byte, 0, 26)
	b = append(b, s.idPrefix...)
	b = appendHexPad(b, uint64(n>>24), 6)
	b = append(b, ':')
	b = appendHexPad(b, uint64(n&0xffffffff), 8)
	return string(b)
}

// appendHexPad appends v in upper-case hex, zero-padded to width digits.
func appendHexPad(b []byte, v uint64, width int) []byte {
	const digits = "0123456789ABCDEF"
	var tmp [16]byte
	i := len(tmp)
	for v > 0 || len(tmp)-i < width {
		i--
		tmp[i] = digits[v&0xF]
		v >>= 4
	}
	return append(b, tmp[i:]...)
}

// authorized answers whether the request carries the trial's bearer token.
func (s *Server) authorized(r *http.Request) bool {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return false
	}
	lower := strings.ToLower(h)
	var tok string
	switch {
	case strings.HasPrefix(lower, "bearer "):
		tok = strings.TrimSpace(h[7:])
	case strings.HasPrefix(lower, "token "):
		tok = strings.TrimSpace(h[6:])
	case strings.HasPrefix(lower, "basic "):
		// git credential helpers send the token as the password of a basic pair.
		raw, err := decodeBase64(strings.TrimSpace(h[6:]))
		if err != nil {
			return false
		}
		pair := string(raw)
		if i := strings.IndexByte(pair, ':'); i >= 0 {
			tok = pair[i+1:]
			if tok == "" || tok == "x-oauth-basic" {
				tok = pair[:i]
			}
		}
	default:
		return false
	}
	if s.st.token == "" {
		return tok != ""
	}
	return tok == s.st.token
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-GitHub-Request-Id", s.requestID())
	path := r.URL.Path
	if path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if !s.authorized(r) {
		if strings.Contains(path, ".git/") || strings.HasSuffix(path, ".git") {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Requires authentication", "documentation_url": "https://docs.github.com/rest"})
		return
	}
	switch {
	case path == "/api/graphql" || path == "/graphql" || path == "/api/v3/graphql":
		s.serveGraphQL(w, r)
		return
	case path == "/mcp" || path == "/mcp/":
		s.serveMCP(w, r)
		return
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		s.serveREST(w, r, strings.TrimPrefix(path, "/api/v3"))
		return
	}
	// /{owner}/{repo}.git/...
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) >= 2 && strings.HasSuffix(parts[1], ".git") {
		rest := ""
		if len(parts) == 3 {
			rest = parts[2]
		}
		s.serveGit(w, r, parts[0], strings.TrimSuffix(parts[1], ".git"), rest)
		return
	}
	if len(parts) == 3 && (strings.HasPrefix(parts[2], "info/refs") || parts[2] == "git-upload-pack" || parts[2] == "git-receive-pack") {
		s.serveGit(w, r, parts[0], parts[1], parts[2])
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := appendJSON(make([]byte, 0, 1024), v)
	if err != nil {
		body = []byte(`{"message":"encoding failed"}`)
		status = http.StatusInternalServerError
	}
	writeRawJSON(w, status, body)
}

func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		w.Write(body)
	}
}

// ---------------------------------------------------------------------------------------------
// lifecycle

func envURL() string { return strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/") }

func register(client *http.Client, base string, port int) {
	body := fmt.Sprintf(`{"port":%d}`, port)
	for attempt := 0; ; attempt++ {
		resp, err := client.Post(base+"/register", "application/json", strings.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				fmt.Fprintf(os.Stderr, "[forge] register answered HTTP %d\n", resp.StatusCode)
				os.Exit(1)
			}
		}
		time.Sleep(backoff(attempt))
	}
}

func backoff(attempt int) time.Duration {
	d := 250 * time.Millisecond << min(attempt, 3)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

func waitDone(client *http.Client, base string) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(base + "/done")
		if err == nil {
			var v struct {
				Done bool `json:"done"`
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && json.Unmarshal(data, &v) == nil && v.Done {
				return
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		time.Sleep(backoff(attempt))
	}
}

func main() {
	fitGOMAXPROCS()
	// The pod has 1 GiB shared with the suite: collect harder before the forge gets near it.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(640 << 20)
	}
	// The live heap is small next to that limit: collect less often (the limit still bounds the heap).
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(400)
	}
	base := envURL()
	listenAddr := "0.0.0.0:0"
	if v := os.Getenv("FORGE_LISTEN"); v != "" {
		listenAddr = v
	}
	if base == "" && os.Getenv("FORGE_LISTEN") == "" {
		fmt.Fprintln(os.Stderr, "[forge] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	srv := &Server{st: newStore(os.Getenv("MAKEFUN_SHOP_TOKEN")), started: time.Now(),
		idPrefix: fmt.Sprintf("F0RG:%04X:", os.Getpid()&0xffff)}
	go serveHTTP1(ln, srv)
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "[forge] listening on %d (GOMAXPROCS %d)\n", port, runtime.GOMAXPROCS(0))
	if base == "" {
		select {}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	register(client, base, port)
	waitDone(client, base)
	ln.Close()
	os.Exit(0)
}
