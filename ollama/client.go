package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kstruzzieri/go-llm/internal/responsebound"
)

const (
	defaultBaseURL = "http://localhost:11434"
	defaultTimeout = 5 * time.Minute
)

// Client communicates with the Ollama REST API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets the Ollama server URL (default: http://localhost:11434).
func WithBaseURL(url string) Option {
	return func(c *Client) {
		c.baseURL = url
	}
}

// WithTimeout sets the HTTP client timeout (default: 5 minutes).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		c.httpClient.Timeout = d
	}
}

// WithHTTPClient replaces the default HTTP client entirely.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// NewClient creates an Ollama client with sensible defaults.
func NewClient(opts ...Option) *Client {
	c := &Client{
		baseURL: defaultBaseURL,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// IsAvailable checks if Ollama is running and responsive.
func (c *Client) IsAvailable(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// doJSON sends a JSON request and decodes the response into dst. A bounded
// request (responsebound) follows no redirect, reads its body under the
// limit, and reports a rejection whose body could not be read as a
// *responsebound.RejectionReadError rather than an *APIError.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, dst any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("ollama: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("ollama: create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := responsebound.HTTPClient(ctx, c.httpClient).Do(req)
	if err != nil {
		return fmt.Errorf("ollama: request failed: %w", err)
	}
	resp.Body = responsebound.Wrap(ctx, resp.Body)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		apiErr, readErr := readAPIError(resp.StatusCode, resp.Body)
		if readErr != nil && responsebound.Bounded(ctx) {
			return &responsebound.RejectionReadError{StatusCode: resp.StatusCode, Err: readErr}
		}
		return apiErr
	}

	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			return fmt.Errorf("ollama: decode response: %w", err)
		}
	}
	return nil
}

// doStream sends a JSON request and calls fn for each newline-delimited JSON object.
func (c *Client) doStream(ctx context.Context, path string, body any, fn func(json.RawMessage) error) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("ollama: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return newAPIError(resp.StatusCode, resp.Body)
	}

	scanner := bufio.NewScanner(resp.Body)
	// Increase buffer to 1MB to handle large streaming responses.
	// The default 64KB limit can be exceeded by models returning large
	// JSON chunks (e.g., long completions in a single streaming frame).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := fn(json.RawMessage(line)); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("ollama: read stream: %w", err)
	}
	return nil
}

// newAPIError constructs an APIError from a non-2xx response body.
// It attempts to parse the Ollama JSON error format {"error":"..."} and
// falls back to the raw body text. Reads at most 4KB to prevent memory
// exhaustion from misbehaving upstreams.
func newAPIError(statusCode int, body io.Reader) *APIError {
	apiErr, _ := readAPIError(statusCode, body)
	return apiErr
}

// readAPIError is newAPIError that also returns the body read failure, if
// any. A body cut at the 4KB cap is not a failure.
func readAPIError(statusCode int, body io.Reader) (*APIError, error) {
	respBody, readErr := io.ReadAll(io.LimitReader(body, 4096))
	msg := string(respBody)
	var errResp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
		msg = errResp.Error
	}
	return &APIError{StatusCode: statusCode, Message: msg}, readErr
}
