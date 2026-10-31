package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// listenerToken is the bearer token the listener tests configure.
const listenerToken = "Az09-._~+/abc=="

// listenerTimeout bounds every client request and raw connection in the
// listener tests, so a regression fails instead of hanging.
const listenerTimeout = 30 * time.Second

// h2cUpgradeSettings is a valid HTTP2-Settings value for an h2c upgrade.
const h2cUpgradeSettings = "AAMAAABkAAQAAP__"

// startMCPListener serves srv on a loopback httptest server through the
// http.Server ListenHTTP builds: TLS with HTTP/2 negotiated when useTLS, h2c
// otherwise. The server's own client is bounded by listenerTimeout.
func startMCPListener(t *testing.T, srv *Server, useTLS bool) *httptest.Server {
	t.Helper()

	ts := httptest.NewUnstartedServer(nil)
	ts.Config = srv.newHTTPServer("", useTLS)
	if useTLS {
		ts.EnableHTTP2 = true
		ts.StartTLS()
	} else {
		ts.Start()
	}
	t.Cleanup(ts.Close)
	ts.Client().Timeout = listenerTimeout
	return ts
}

// h2cPriorKnowledgeClient speaks HTTP/2 over cleartext TCP from the first
// byte, without an HTTP/1.1 upgrade. Callers release its connections with
// CloseIdleConnections.
func h2cPriorKnowledgeClient() *http.Client {
	return &http.Client{
		Timeout: listenerTimeout,
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

// listenerCases are the ways a client reaches ListenHTTP's server: both
// protocols an h2c listener accepts, and HTTP/2 over TLS.
var listenerCases = []struct {
	name   string
	useTLS bool
	proto  int // expected resp.ProtoMajor
	client func(t *testing.T, ts *httptest.Server) *http.Client
}{
	{"h2c listener, HTTP/1.1", false, 1, func(_ *testing.T, ts *httptest.Server) *http.Client { return ts.Client() }},
	{"h2c listener, prior knowledge", false, 2, func(t *testing.T, _ *httptest.Server) *http.Client {
		c := h2cPriorKnowledgeClient()
		t.Cleanup(c.CloseIdleConnections)
		return c
	}},
	{"TLS listener, HTTP/2", true, 2, func(_ *testing.T, ts *httptest.Server) *http.Client { return ts.Client() }},
}

// mcpRequest builds a request to url carrying mcpHeaders, the initialize
// body when method is POST, and auth as the Authorization value when set.
func mcpRequest(t *testing.T, method, url, auth string) *http.Request {
	t.Helper()

	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(initializeBody)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", method, url, err)
	}
	mcpHeaders(req.Header)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req
}

// sendRequest sends req through c and returns the response with its body
// closed unread; callers inspect only the status line and headers.
func sendRequest(t *testing.T, c *http.Client, req *http.Request) *http.Response {
	t.Helper()

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", req.Method, req.URL.Path, err)
	}
	_ = resp.Body.Close()
	return resp
}

// rawHTTP1Status writes request verbatim to ts over HTTP/1.1 (negotiated by
// ALPN on a TLS server) and returns the response status.
func rawHTTP1Status(t *testing.T, ts *httptest.Server, request string) int {
	t.Helper()

	addr := ts.Listener.Addr().String()
	dialer := &net.Dialer{Timeout: listenerTimeout}
	var (
		conn net.Conn
		err  error
	)
	if ts.TLS != nil {
		cfg := ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		cfg.NextProtos = []string{"http/1.1"}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, cfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		t.Fatalf("dial %s error = %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(listenerTimeout)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}

	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write request error = %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("ReadResponse() error = %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// h2cUpgradeStatuses sends an initialize POST carrying upgrade to addr as an
// HTTP/1.1 "Upgrade: h2c" request and requires 101. Over the upgraded
// connection it reads stream 1's :status (the upgraded request itself), then
// opens stream 3, an initialize POST carrying next, and reads its :status.
// Both requests also carry mcpHeaders.
func h2cUpgradeStatuses(t *testing.T, addr string, upgrade, next http.Header) (stream1, stream3 int) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, listenerTimeout)
	if err != nil {
		t.Fatalf("dial %s error = %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(listenerTimeout)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(initializeBody))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	mcpHeaders(req.Header)
	for k, vs := range upgrade {
		req.Header[k] = vs
	}
	req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	req.Header.Set("Upgrade", "h2c")
	req.Header.Set("HTTP2-Settings", h2cUpgradeSettings)
	if err := req.Write(conn); err != nil {
		t.Fatalf("write upgrade request error = %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read upgrade response error = %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	// The framer reads through br, which may already hold the server's
	// first frames.
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatalf("write client preface error = %v", err)
	}
	fr := http2.NewFramer(conn, br)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := fr.WriteSettings(); err != nil {
		t.Fatalf("write SETTINGS error = %v", err)
	}
	status := func(streamID uint32) int {
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				t.Fatalf("waiting for stream %d status: %v", streamID, err)
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					if err := fr.WriteSettingsAck(); err != nil {
						t.Fatalf("write SETTINGS ack error = %v", err)
					}
				}
			case *http2.PingFrame:
				if !f.IsAck() {
					if err := fr.WritePing(true, f.Data); err != nil {
						t.Fatalf("write PING ack error = %v", err)
					}
				}
			case *http2.GoAwayFrame:
				t.Fatalf("GOAWAY %v before stream %d answered", f.ErrCode, streamID)
			case *http2.RSTStreamFrame:
				if f.StreamID == streamID {
					t.Fatalf("RST_STREAM %v on stream %d before its status", f.ErrCode, streamID)
				}
			case *http2.MetaHeadersFrame:
				if f.StreamID == streamID {
					code, err := strconv.Atoi(f.PseudoValue("status"))
					if err != nil {
						t.Fatalf("stream %d :status %q: %v", streamID, f.PseudoValue("status"), err)
					}
					return code
				}
			}
		}
	}
	stream1 = status(1)

	h := http.Header{}
	mcpHeaders(h)
	for k, vs := range next {
		h[k] = vs
	}
	h.Set("Content-Length", strconv.Itoa(len(initializeBody)))
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	fields := []hpack.HeaderField{
		{Name: ":method", Value: http.MethodPost},
		{Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: addr},
		{Name: ":path", Value: "/mcp"},
	}
	for k, vs := range h {
		for _, v := range vs {
			fields = append(fields, hpack.HeaderField{Name: strings.ToLower(k), Value: v})
		}
	}
	for _, f := range fields {
		if err := enc.WriteField(f); err != nil {
			t.Fatalf("hpack encode %s error = %v", f.Name, err)
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 3, BlockFragment: block.Bytes(), EndHeaders: true}); err != nil {
		t.Fatalf("write stream 3 HEADERS error = %v", err)
	}
	if err := fr.WriteData(3, true, []byte(initializeBody)); err != nil {
		t.Fatalf("write stream 3 DATA error = %v", err)
	}
	stream3 = status(3)
	return stream1, stream3
}

// countMCPRequests counts every JSON-RPC request srv's SDK server
// dispatches, initialize included, so a test can tell whether an HTTP
// request reached MCP at all.
func countMCPRequests(srv *Server) *atomic.Int64 {
	n := new(atomic.Int64)
	srv.mcpServer.AddReceivingMiddleware(func(next gomcp.MethodHandler) gomcp.MethodHandler {
		return func(ctx context.Context, method string, req gomcp.Request) (gomcp.Result, error) {
			n.Add(1)
			return next(ctx, method, req)
		}
	})
	return n
}

// spaceReader is an endless source of ASCII spaces.
type spaceReader struct{}

func (spaceReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// paddedInitialize returns size bytes: initializeBody followed by spaces,
// which JSON ignores, so the whole body is still one valid initialize
// request the SDK would dispatch if it read it.
func paddedInitialize(size int64) io.Reader {
	return io.MultiReader(strings.NewReader(initializeBody),
		io.LimitReader(spaceReader{}, size-int64(len(initializeBody))))
}

// TestListenerOptionsStar pins DisableGeneralOptionsHandler: with a token,
// "OPTIONS *" reaches the gate instead of Go's built-in 200.
func TestListenerOptionsStar(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		useTLS bool
		want   int
	}{
		{"h2c, token", listenerToken, false, http.StatusUnauthorized},
		{"h2c, no token", "", false, http.StatusOK},
		{"TLS, token", listenerToken, true, http.StatusUnauthorized},
		{"TLS, no token", "", true, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := startMCPListener(t, newTestMCPServer(t, tt.token), tt.useTLS)
			request := "OPTIONS * HTTP/1.1\r\nHost: " + ts.Listener.Addr().String() + "\r\nContent-Length: 0\r\n\r\n"
			if got := rawHTTP1Status(t, ts, request); got != tt.want {
				t.Fatalf("OPTIONS * status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestListenerRequiresBearerOnEveryPath(t *testing.T) {
	for _, lc := range listenerCases {
		t.Run(lc.name, func(t *testing.T) {
			ts := startMCPListener(t, newTestMCPServer(t, listenerToken), lc.useTLS)
			c := lc.client(t, ts)

			for _, r := range []struct{ method, path string }{
				{http.MethodPost, "/mcp"},
				{http.MethodGet, "/"},
				{http.MethodGet, "/healthz"},
				{http.MethodPost, "/mcp/extra"},
			} {
				resp := sendRequest(t, c, mcpRequest(t, r.method, ts.URL+r.path, ""))
				if resp.ProtoMajor != lc.proto {
					t.Errorf("%s %s: ProtoMajor = %d, want %d", r.method, r.path, resp.ProtoMajor, lc.proto)
				}
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s %s without bearer: status = %d, want %d", r.method, r.path, resp.StatusCode, http.StatusUnauthorized)
				}
			}
			resp := sendRequest(t, c, mcpRequest(t, http.MethodPost, ts.URL+"/mcp", "Bearer "+listenerToken))
			if resp.StatusCode != http.StatusOK {
				t.Errorf("POST /mcp with bearer: status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
		})
	}
}

// TestH2CStreamsRequireBearer checks each stream on one prior-knowledge
// connection separately: two refusals do not end the connection, and a
// later authenticated stream on it succeeds.
func TestH2CStreamsRequireBearer(t *testing.T) {
	ts := startMCPListener(t, newTestMCPServer(t, listenerToken), false)
	c := h2cPriorKnowledgeClient()
	t.Cleanup(c.CloseIdleConnections)

	conns := map[net.Conn]bool{}
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { conns[info.Conn] = true }}
	for i, tt := range []struct {
		auth string
		want int
	}{
		{"", http.StatusUnauthorized},
		{"", http.StatusUnauthorized},
		{"Bearer " + listenerToken, http.StatusOK},
	} {
		req := mcpRequest(t, http.MethodPost, ts.URL+"/mcp", tt.auth)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
		resp := sendRequest(t, c, req)
		if resp.ProtoMajor != 2 {
			t.Fatalf("request %d: ProtoMajor = %d, want 2", i+1, resp.ProtoMajor)
		}
		if resp.StatusCode != tt.want {
			t.Errorf("request %d: status = %d, want %d", i+1, resp.StatusCode, tt.want)
		}
	}
	if len(conns) != 1 {
		t.Fatalf("requests used %d connections, want 1", len(conns))
	}
}

// TestH2CUpgradeStreamsRequireBearer checks the upgraded request (stream 1)
// and a later stream on the same upgraded connection (stream 3) separately:
// a bearer on the upgrade does not carry over. An upgrade without the bearer
// never switches protocols; TestH2CUpgradeWithoutBearerIsRefused covers it.
func TestH2CUpgradeStreamsRequireBearer(t *testing.T) {
	bearer := http.Header{"Authorization": {"Bearer " + listenerToken}}
	tests := []struct {
		name          string
		upgrade, next http.Header
		want1, want3  int
	}{
		{"bearer on upgrade only", bearer, nil, http.StatusOK, http.StatusUnauthorized},
		{"bearer on both", bearer, bearer, http.StatusOK, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := startMCPListener(t, newTestMCPServer(t, listenerToken), false)
			s1, s3 := h2cUpgradeStatuses(t, ts.Listener.Addr().String(), tt.upgrade, tt.next)
			if s1 != tt.want1 {
				t.Errorf("stream 1 status = %d, want %d", s1, tt.want1)
			}
			if s3 != tt.want3 {
				t.Errorf("stream 3 status = %d, want %d", s3, tt.want3)
			}
		})
	}
}

// TestH2CUpgradeWithoutBearerIsRefused pins the check in front of h2c on a
// real listener: an HTTP/1.1 upgrade without the right bearer gets the
// gate's 401 instead of 101. (Without the check the inner gate would still
// refuse stream 1, so only the HTTP/1.1 status tells the two apart.)
func TestH2CUpgradeWithoutBearerIsRefused(t *testing.T) {
	for _, auth := range []string{"", "Authorization: Bearer wrong-token\r\n"} {
		srv := newTestMCPServer(t, listenerToken)
		ts := startMCPListener(t, srv, false)
		request := "POST /mcp HTTP/1.1\r\nHost: " + ts.Listener.Addr().String() + "\r\n" +
			"Content-Type: application/json\r\nAccept: application/json, text/event-stream\r\n" +
			auth +
			"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: " + h2cUpgradeSettings + "\r\n" +
			"Content-Length: " + strconv.Itoa(len(initializeBody)) + "\r\n\r\n" + initializeBody
		if got := rawHTTP1Status(t, ts, request); got != http.StatusUnauthorized {
			t.Errorf("auth %q: upgrade status = %d, want %d", auth, got, http.StatusUnauthorized)
		}
	}
}

// TestH2CUpgradeWithoutBearerIsNotBuffered pins the order on the h2c handler
// newHTTPServer builds: an upgrade without the right bearer gets the gate's
// own 401 before h2c reads any of its body, and a declared oversize body
// still gets the limit's 413 first. Under a ResponseRecorder h2c cannot
// hijack, so a missing check shows as a 500 after h2c reads to the limit.
func TestH2CUpgradeWithoutBearerIsNotBuffered(t *testing.T) {
	tests := []struct {
		name, method, target, proto, auth string
		declared                          bool // Content-Length above the limit
		want                              int
	}{
		{"no bearer", http.MethodPost, "/mcp", "", "", false, http.StatusUnauthorized},
		{"wrong bearer", http.MethodPost, "/mcp", "", "Bearer wrong-token", false, http.StatusUnauthorized},
		{"GET without bearer", http.MethodGet, "/mcp", "", "", false, http.StatusUnauthorized},
		// Only the header-less preface skips the check, as in h2c itself.
		{"PRI preface with headers", "PRI", "*", "HTTP/2.0", "", false, http.StatusUnauthorized},
		{"declared oversize body without bearer", http.MethodPost, "/mcp", "", "", true, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestMCPServer(t, listenerToken)
			body := &countingReader{reader: paddedInitialize(4 * maxMCPHTTPBodyBytes)}
			req := httptest.NewRequest(tt.method, "http://localhost/mcp", body)
			if tt.target == "*" {
				req.URL.Path = "*"
			}
			if tt.proto != "" {
				req.Proto, req.ProtoMajor, req.ProtoMinor = tt.proto, 2, 0
			}
			if tt.declared {
				req.ContentLength = 4 * maxMCPHTTPBodyBytes
			}
			mcpHeaders(req.Header)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
			req.Header.Set("Upgrade", "h2c")
			req.Header.Set("HTTP2-Settings", h2cUpgradeSettings)
			rec := httptest.NewRecorder()

			srv.newHTTPServer("", false).Handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			wantAuth, wantBody := `Bearer realm="go-llm-mcp"`, "unauthorized\n"
			if tt.want == http.StatusRequestEntityTooLarge {
				wantAuth, wantBody = "", "request body too large\n"
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != wantAuth {
				t.Errorf("WWW-Authenticate = %q, want %q", got, wantAuth)
			}
			if got := rec.Body.String(); got != wantBody {
				t.Errorf("body = %q, want %q", got, wantBody)
			}
			// This refusal answers every HTTP/1.1 request on the default
			// listener, so it must echo neither credential either.
			text := rec.Body.String()
			for k, vs := range rec.Header() {
				text += "\n" + k + ": " + strings.Join(vs, "\n")
			}
			assertNoBearerEcho(t, text, listenerToken)
			if _, presented, ok := strings.Cut(tt.auth, " "); ok {
				assertNoBearerEcho(t, text, presented)
			}
			if body.read != 0 {
				t.Errorf("read %d body bytes before refusing, want 0", body.read)
			}
		})
	}
}

func TestTLSRequiresBearer(t *testing.T) {
	ts := startMCPListener(t, newTestMCPServer(t, listenerToken), true)
	for _, tt := range []struct {
		name string
		auth string
		want int
	}{
		{"no bearer", "", http.StatusUnauthorized},
		{"wrong bearer", "Bearer wrong", http.StatusUnauthorized},
		{"configured bearer", "Bearer " + listenerToken, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := sendRequest(t, ts.Client(), mcpRequest(t, http.MethodPost, ts.URL+"/mcp", tt.auth))
			if resp.ProtoMajor != 2 {
				t.Fatalf("ProtoMajor = %d, want 2", resp.ProtoMajor)
			}
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

// TestAuthenticatedStackKeepsCrossOriginAndBodyLimits runs each row against
// the handler directly and over both listeners. Bodies are a valid
// initialize padded with spaces, so a removed limiter would let the SDK read
// and dispatch them; the request counter and, in the direct run, the exact
// bytes read show it did not.
func TestAuthenticatedStackKeepsCrossOriginAndBodyLimits(t *testing.T) {
	const maxBody = maxMCPHTTPBodyBytes
	valid := "Bearer " + listenerToken
	initLen := int64(len(initializeBody))

	rows := []struct {
		name     string
		auth     string
		origin   string
		size     int64 // body bytes
		declared bool  // send Content-Length: size; otherwise the length is unknown
		want     int
		wantMCP  int64 // JSON-RPC requests the SDK dispatches
		wantRead int64 // body bytes the server reads (direct run only)
	}{
		{"valid bearer, hostile origin", valid, "https://malicious.example", initLen, true, http.StatusForbidden, 0, 0},
		{"invalid bearer, hostile origin", "Bearer wrong", "https://malicious.example", initLen, true, http.StatusUnauthorized, 0, 0},
		{"valid bearer, declared oversize body", valid, "", maxBody + 1, true, http.StatusRequestEntityTooLarge, 0, 0},
		{"valid bearer, unknown-length oversize body", valid, "", 4 * maxBody, false, http.StatusBadRequest, 0, maxBody + 1},
		// Control: the same stack dispatches a well-formed request, so the
		// counter and byte count above can register one.
		{"valid bearer, initialize", valid, "", initLen, true, http.StatusOK, 1, initLen},
	}

	runs := []struct {
		name   string
		direct bool
		useTLS bool
	}{
		{"direct", true, false},
		{"TLS listener", false, true},
		{"h2c listener, prior knowledge", false, false},
	}

	for _, run := range runs {
		for _, row := range rows {
			t.Run(run.name+"/"+row.name, func(t *testing.T) {
				srv := newTestMCPServer(t, listenerToken)
				dispatched := countMCPRequests(srv)
				body := &countingReader{reader: paddedInitialize(row.size)}

				var req *http.Request
				var ts *httptest.Server
				if run.direct {
					req = httptest.NewRequest(http.MethodPost, "http://localhost/mcp", body)
				} else {
					ts = startMCPListener(t, srv, run.useTLS)
					var err error
					req, err = http.NewRequest(http.MethodPost, ts.URL+"/mcp", body)
					if err != nil {
						t.Fatalf("NewRequest() error = %v", err)
					}
				}
				req.ContentLength = -1
				if row.declared {
					req.ContentLength = row.size
				}
				mcpHeaders(req.Header)
				req.Header.Set("Authorization", row.auth)
				if row.origin != "" {
					req.Header.Set("Origin", row.origin)
				}

				var status int
				if run.direct {
					rec := httptest.NewRecorder()
					streamableHTTPHandler(srv).ServeHTTP(rec, req)
					status = rec.Code
				} else {
					c := ts.Client()
					if !run.useTLS {
						c = h2cPriorKnowledgeClient()
						t.Cleanup(c.CloseIdleConnections)
					}
					resp, err := c.Do(req)
					if err != nil {
						t.Fatalf("POST error = %v", err)
					}
					// Read the whole response: an SSE reply carries the
					// initialize result only after its headers.
					_, err = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if err != nil {
						t.Fatalf("read response error = %v", err)
					}
					if resp.ProtoMajor != 2 {
						t.Errorf("ProtoMajor = %d, want 2", resp.ProtoMajor)
					}
					status = resp.StatusCode
				}

				if status != row.want {
					t.Errorf("status = %d, want %d", status, row.want)
				}
				if got := dispatched.Load(); got != row.wantMCP {
					t.Errorf("SDK dispatched %d requests, want %d", got, row.wantMCP)
				}
				if run.direct && int64(body.read) != row.wantRead {
					t.Errorf("server read %d body bytes, want %d", body.read, row.wantRead)
				}
			})
		}
	}
}

// TestAuthenticatedH2CUpgradeBodyStaysBounded pins the outer limiter on the
// h2c handler newHTTPServer builds: an authenticated upgrade with an
// unknown-length body is cut at the limit before h2c buffers it.
func TestAuthenticatedH2CUpgradeBodyStaysBounded(t *testing.T) {
	srv := newTestMCPServer(t, listenerToken)
	body := &countingReader{reader: paddedInitialize(4 * maxMCPHTTPBodyBytes)}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", body)
	mcpHeaders(req.Header)
	req.Header.Set("Authorization", "Bearer "+listenerToken)
	req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	req.Header.Set("Upgrade", "h2c")
	req.Header.Set("HTTP2-Settings", h2cUpgradeSettings)

	srv.newHTTPServer("", false).Handler.ServeHTTP(httptest.NewRecorder(), req)

	// h2c needs a hijackable connection, so the inner handler cannot run under
	// a ResponseRecorder; the exact read count is the check.
	if want := maxMCPHTTPBodyBytes + 1; int64(body.read) != want {
		t.Fatalf("h2c upgrade read %d body bytes, want bounded probe of %d", body.read, want)
	}
}

// TestListenHTTPRefusesNonLoopbackWithoutTLS guards the refusal ListenHTTP
// makes before building anything. The context only bounds a regression: a
// removed guard would serve until it expires and then return nil.
func TestListenHTTPRefusesNonLoopbackWithoutTLS(t *testing.T) {
	for _, token := range []string{"", listenerToken} {
		for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:0"} {
			t.Run("token="+strconv.FormatBool(token != "")+"/"+addr, func(t *testing.T) {
				srv := newTestMCPServer(t, token)
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()

				err := srv.ListenHTTP(ctx, addr)

				if err == nil || !strings.Contains(err.Error(), "requires TLS") {
					t.Fatalf("ListenHTTP(%q) error = %v, want a requires-TLS refusal", addr, err)
				}
				if ctx.Err() != nil {
					t.Fatalf("ListenHTTP(%q) returned only after its context ended", addr)
				}
				srv.mu.RLock()
				built := srv.httpServer != nil
				srv.mu.RUnlock()
				if built {
					t.Fatalf("ListenHTTP(%q) built an http.Server before refusing", addr)
				}
			})
		}
	}
}

// TestListenHTTPBuildsGatedServer checks the server ListenHTTP itself builds
// on loopback h2c, which the listener tests above reach only through
// newHTTPServer: its bind address, its "OPTIONS *" setting, and a handler
// that speaks h2c and applies the gate.
func TestListenHTTPBuildsGatedServer(t *testing.T) {
	const addr = "127.0.0.1:0"
	for _, tt := range []struct {
		token string
		want  int
	}{
		{"", http.StatusOK},
		{listenerToken, http.StatusUnauthorized},
	} {
		t.Run("token="+strconv.FormatBool(tt.token != ""), func(t *testing.T) {
			srv := newTestMCPServer(t, tt.token)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- srv.ListenHTTP(ctx, addr) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("ListenHTTP() after cancel = %v, want nil", err)
					}
				case <-time.After(listenerTimeout):
					t.Error("ListenHTTP did not return after its context ended")
				}
			})

			var hs *http.Server
			for deadline := time.Now().Add(listenerTimeout); hs == nil; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("ListenHTTP never built its http.Server")
				}
				srv.mu.RLock()
				hs = srv.httpServer
				srv.mu.RUnlock()
			}
			if hs.Addr != addr {
				t.Errorf("Addr = %q, want %q", hs.Addr, addr)
			}
			if got, want := hs.DisableGeneralOptionsHandler, tt.token != ""; got != want {
				t.Errorf("DisableGeneralOptionsHandler = %v, want %v", got, want)
			}

			ts := httptest.NewServer(hs.Handler)
			t.Cleanup(ts.Close)
			c := h2cPriorKnowledgeClient()
			t.Cleanup(c.CloseIdleConnections)
			resp := sendRequest(t, c, mcpRequest(t, http.MethodPost, ts.URL+"/mcp", ""))
			if resp.ProtoMajor != 2 {
				t.Errorf("ProtoMajor = %d, want 2", resp.ProtoMajor)
			}
			if resp.StatusCode != tt.want {
				t.Errorf("initialize without Authorization: status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestNoTokenListenerUnchanged(t *testing.T) {
	for _, lc := range listenerCases {
		t.Run(lc.name, func(t *testing.T) {
			ts := startMCPListener(t, newTestMCPServer(t, ""), lc.useTLS)
			resp := sendRequest(t, lc.client(t, ts), mcpRequest(t, http.MethodPost, ts.URL+"/mcp", ""))
			if resp.ProtoMajor != lc.proto {
				t.Errorf("ProtoMajor = %d, want %d", resp.ProtoMajor, lc.proto)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("initialize without Authorization: status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
		})
	}
}
