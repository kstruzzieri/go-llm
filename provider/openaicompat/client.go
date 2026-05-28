package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	baseURL string
	apiKey  string
	hc      *http.Client
}

// ClientOption configures a Client.
type ClientOption func(*Client)

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
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: 5 * time.Minute},
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

// setHeaders applies Authorization (when APIKey is set), Accept, and
// optional Content-Type to req.
func (c *Client) setHeaders(req *http.Request, contentType string) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
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
