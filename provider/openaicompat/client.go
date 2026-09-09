package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// Client is a thin HTTP wrapper for OpenAI-compatible endpoints. It handles
// Bearer auth, JSON encoding, error-envelope unwrapping, and produces SSE
// readers for streaming responses. It does NOT itself know about chat /
// completion / embedding shapes — those live with the Provider impl that
// composes this client.
//
// Client is safe for concurrent use; the underlying http.Client and
// configuration are read-only after construction.
type Client struct {
	baseURL   string
	apiKey    string
	userAgent string
	hc        *http.Client
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// sessionIDKey types the context value carrying a per-request session id.
// The id has to vary per request while the Client is built once, so it
// travels on the request context rather than in Client state.
type sessionIDKey struct{}

// withSessionID returns ctx carrying id for the request about to be sent.
// An empty id returns ctx unchanged, so no header is emitted.
func withSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDKey{}, id)
}

// sessionIDFrom returns the session id carried by ctx, or "" when unset.
func sessionIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}

// modulePath is this module's import path, used to find our own version in
// the build info whether go-llm is the program being run or a dependency of
// one.
const modulePath = "github.com/kstruzzieri/go-llm"

// unknownVersion stands in when the build carries no version for this module
// — a plain `go build` or `go test`, where the toolchain stamps "(devel)".
// That literal is not emitted as-is: parentheses open a comment in an HTTP
// field value (RFC 9110 §5.6.5), which would make the agent ambiguous to
// parse.
const unknownVersion = "dev"

// defaultUserAgent identifies this module rather than net/http's generic
// Go-http-client/1.1, which opencode's docs single out as the thing not to
// send.
//
// Only this module's own version is reported. The dependency entry is
// authoritative when go-llm is imported, because info.Main there describes
// the *consumer* (Firn IDE, say) — reporting its version as go-llm's would
// misidentify the client. info.Main is consulted only when it is go-llm
// itself, i.e. one of go-llm's own commands is the program running.
func defaultUserAgent() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return userAgentFromBuildInfo(nil)
	}
	return userAgentFromBuildInfo(info)
}

// userAgentFromBuildInfo holds the version-resolution decision as a pure
// function so it can be tested against constructed build info; the real
// debug.ReadBuildInfo describes the test binary and cannot exercise the
// go-llm-as-dependency case. A nil info means the build carried none.
func userAgentFromBuildInfo(info *debug.BuildInfo) string {
	version := ""
	if info != nil {
		for _, dep := range info.Deps {
			if dep.Path == modulePath {
				version = dep.Version
				if dep.Replace != nil {
					version = dep.Replace.Version
				}
				break
			}
		}
		if version == "" && info.Main.Path == modulePath {
			version = info.Main.Version
		}
	}
	if version == "" || version == "(devel)" {
		version = unknownVersion
	}
	return "go-llm/" + version
}

// defaultUA is resolved once; build info does not change at runtime.
var defaultUA = defaultUserAgent()

// WithHTTPClient overrides the default http.Client (default: a 5-minute-timeout
// client). Pass a pre-configured client to share connection pools, attach
// middleware, or tighten the timeout for low-latency local servers.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) {
		if hc != nil {
			c.hc = hc
		}
	}
}

// WithAPIKey sets the Bearer token sent in the Authorization header. Empty
// disables the header entirely — appropriate for local servers that don't
// enforce auth (vanilla llama.cpp --api, default LM Studio).
func WithAPIKey(key string) ClientOption {
	return func(c *Client) {
		c.apiKey = key
	}
}

// WithUserAgent overrides the default go-llm/<version> User-Agent, letting an
// embedder identify as itself (Firn IDE, say) instead of as the shared module.
// Empty is ignored so the default is never replaced by a blank header.
func WithUserAgent(ua string) ClientOption {
	return func(c *Client) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// NewClient builds a Client targeting baseURL. The URL should be the server
// root WITHOUT the /v1 suffix; per-endpoint paths are appended internally
// so callers don't have to remember which endpoints live under /v1. Any
// trailing slash on baseURL is stripped.
//
// The default http.Client uses a 5-minute timeout, matching the
// config.ProviderConfig default. Override via WithHTTPClient for shorter
// (low-latency local) or longer (large-batch embedding) requests.
func NewClient(baseURL string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		userAgent: defaultUA,
		hc:        &http.Client{Timeout: 5 * time.Minute},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// postJSON sends a JSON request and decodes a JSON response. Non-2xx
// responses are unwrapped into an OpenAI-shape error so callers see the
// server's own error message rather than just an HTTP status code.
//
// The path argument is the endpoint (e.g. "/v1/chat/completions"); it is
// appended to baseURL verbatim.
func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	resp, err := c.post(ctx, path, body, "application/json")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := decodeJSONOrError(resp, out); err != nil {
		return err
	}
	return nil
}

// postSSE sends a JSON request and returns an *sseReader for the streaming
// response body. The caller MUST Close the reader; failing to do so leaks
// the connection. Non-2xx responses are converted to errors before any
// reader is returned, so callers can rely on a returned reader being live.
func (c *Client) postSSE(ctx context.Context, path string, body any) (*sseReader, error) {
	resp, err := c.post(ctx, path, body, "application/json")
	if err != nil {
		return nil, err
	}

	if resp.StatusCode/100 != 2 {
		err := readErrorEnvelope(resp)
		_ = resp.Body.Close()
		return nil, err
	}
	return newSSEReader(resp.Body), nil
}

// getJSON sends a GET request and decodes a JSON response. Used for
// /v1/models discovery and health probing.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("openaicompat: build %s: %w", path, err)
	}
	c.setHeaders(req, "")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("openaicompat: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeJSONOrError(resp, out)
}

// post is the shared transport for postJSON / postSSE. It marshals body
// to JSON, sets headers including auth, and returns the raw response for
// the caller to consume.
func (c *Client) post(ctx context.Context, path string, body any, contentType string) (*http.Response, error) {
	// HTTP trims surrounding SP/HTAB, which would collapse distinct session IDs.
	if id := sessionIDFrom(ctx); strings.Trim(id, " \t") != id {
		return nil, fmt.Errorf("openaicompat: session ID must not start or end with a space or tab")
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: marshal %s body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("openaicompat: build %s: %w", path, err)
	}
	c.setHeaders(req, contentType)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: %s: %w", path, err)
	}
	return resp, nil
}

// setHeaders applies Authorization (when APIKey is set), Accept, User-Agent,
// the opencode session id carried by the request context, and optional
// Content-Type to req.
func (c *Client) setHeaders(req *http.Request, contentType string) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", c.userAgent)
	if id := sessionIDFrom(req.Context()); id != "" {
		req.Header.Set("x-opencode-session", id)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
}

// decodeJSONOrError reads resp.Body. On 2xx, it JSON-decodes into out. On
// non-2xx, it parses an OpenAI error envelope and returns it as an error.
func decodeJSONOrError(resp *http.Response, out any) error {
	if resp.StatusCode/100 != 2 {
		return readErrorEnvelope(resp)
	}
	if out == nil {
		// Drain the body so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("openaicompat: decode response: %w", err)
	}
	return nil
}

// readErrorEnvelope reads resp.Body looking for an OpenAI-shape error.
// Falls back to a plain "<status>: <body>" when the body is not a valid
// error envelope, so HTML error pages and bare strings still produce a
// usable error message rather than swallowing the failure.
func readErrorEnvelope(resp *http.Response) error {
	const limit = 64 * 1024
	body, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	body = bytes.TrimSpace(body)

	var env errorEnvelope
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		return &statusError{statusCode: resp.StatusCode, status: resp.Status, message: env.Error.Message}
	}
	if len(body) > 0 {
		return &statusError{statusCode: resp.StatusCode, status: resp.Status, message: string(body)}
	}
	return &statusError{statusCode: resp.StatusCode, status: resp.Status}
}

type statusError struct {
	statusCode int
	status     string
	message    string
}

func (e *statusError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("openaicompat: %s: %s", e.status, e.message)
	}
	return fmt.Sprintf("openaicompat: %s", e.status)
}

func (e *statusError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}
