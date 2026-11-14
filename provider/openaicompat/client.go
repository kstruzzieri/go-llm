package openaicompat

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/internal/responsebound"
	"github.com/kstruzzieri/go-llm/internal/secretscan"
	"github.com/kstruzzieri/go-llm/provider"
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

	// contentGuard, when non-nil, vets every outbound JSON body (see
	// WithContentGuard). Read-only after construction.
	contentGuard func(string) bool

	// cacheAffinity is the resolved mode — "openai", "openrouter",
	// "opencode", or "none" — and affinitySecret keys affinityKey (nil sends
	// no key). Both are read-only after construction; see WithCacheAffinity.
	cacheAffinity  string
	affinitySecret []byte

	// abacusRouteLLM is true for an Abacus RouteLLM base URL, whose
	// streamed prompt_tokens counts cached tokens at the model's cache
	// discount and which omits its cache counter when nothing is cached;
	// see Chat and ChatStream. Read-only after construction.
	abacusRouteLLM bool
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

// WithContentGuard installs a check on every outbound JSON request body. A body
// that match accepts is not sent: the call returns an error wrapping
// provider.ErrContentDenied. match is given the body's raw text and each decoded
// key and string value, so a credential that json.Marshal escapes on the wire
// is still seen. Tool-call arguments also get one lenient escape-decoding pass
// (secretscan.MatchToolArguments) on the document inside their string wrapper
// or on malformed text, so a credential written as JSON escapes there still
// matches. Headers are never inspected, so the client's own API key still
// authenticates. nil leaves the guard off.
//
// With a guard installed, a body that fails to encode returns a fixed error
// instead of wrapping the encoder's, whose text can quote the offending bytes.
// The Client is safe for concurrent use, so match is called from concurrent
// requests and must be safe for concurrent use too.
func WithContentGuard(match func(string) bool) ClientOption {
	return func(c *Client) {
		c.contentGuard = match
	}
}

// WithCacheAffinity selects how a request's session id (ChatRequest.SessionID)
// reaches the provider's prompt cache:
//
//   - "openai": body prompt_cache_key = the affinity key
//   - "openrouter": header x-session-id = the affinity key
//   - "opencode": header x-opencode-session = the raw session id
//   - "none": nothing
//
// The affinity key is hex(HMAC-SHA256(secret, sessionID))[:32] under a random
// secret each Client mints in NewClient, so it is stable for one session on
// one Client, changes when the process restarts, and cannot be recomputed
// from the session (thread) id. An empty session id sends nothing in any mode.
//
// Empty, the default, infers the mode from the base URL (InferCacheAffinity).
// An unknown mode sends nothing, as "none" does: the option cannot fail, so
// callers validate the value first (config rejects unknown cache_affinity).
func WithCacheAffinity(mode string) ClientOption {
	return func(c *Client) {
		c.cacheAffinity = mode
	}
}

// cacheAffinityHosts maps each host that documents a session cache key to its
// mode. An https base URL matches the host itself or any subdomain of it.
var cacheAffinityHosts = []struct{ host, mode string }{
	{"api.openai.com", "openai"},
	{"openrouter.ai", "openrouter"},
	{"opencode.ai", "opencode"},
}

// InferCacheAffinity returns the cache affinity mode for an https baseURL's
// host: "openai" for api.openai.com, "openrouter" for openrouter.ai,
// "opencode" for opencode.ai — each also matching its subdomains,
// case-insensitively, with one trailing dot, the port, and the path ignored.
// It returns "none" for any other host, a non-https URL (so the raw OpenCode
// session id is never sent in cleartext by inference), or an unparseable URL,
// so a host that documents no key is sent none.
func InferCacheAffinity(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" { // url.Parse lowercases the scheme
		return "none"
	}
	for _, h := range cacheAffinityHosts {
		if hostIs(u, h.host) {
			return h.mode
		}
	}
	return "none"
}

// hostIs reports whether u's host is want or a subdomain of it,
// case-insensitively, with one trailing dot and the port ignored.
func hostIs(u *url.URL, want string) bool {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == want || strings.HasSuffix(host, "."+want)
}

// isAbacusRouteLLM reports whether baseURL is Abacus RouteLLM
// (routellm.abacus.ai or a subdomain), over any scheme.
func isAbacusRouteLLM(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && hostIs(u, "routellm.abacus.ai")
}

// abacusCacheWeights holds, per served Abacus RouteLLM model, the weight w
// its streamed prompt_tokens gives a cached token, as {numerator,
// denominator}: prompt_tokens = uncached + floor(cached * w). Each w equals
// the model's cached_input_token_rate / input_token_rate in Abacus's
// /v1/models and was measured on 2026-11-03 against exact non-streaming
// counts (docs/llm/benchmarks/2026-11-03-abacus-stream-weight.md). The table
// is also the only set of models whose streamed count is trusted at all, cold
// or warm: a model that bills cache writes adds the written tokens at 1.25
// and sends no written count, every other model probed bills them (Claude,
// gpt-6-luna, gpt-6-astra, gpt-5.6-luna), and /v1/models does not say which
// models do.
var abacusCacheWeights = map[string][2]int{
	"gpt-5-mini":                         {1, 10},  // $0.025 / $0.25
	"gemini-3.5-flash":                   {1, 10},  // $0.15 / $1.50
	"grok-4.3":                           {4, 25},  // $0.20 / $1.25
	"deepseek-ai/DeepSeek-V4-Flash-0731": {7, 110}, // $0.028 / $0.44
}

// abacusStreamPrompt recovers the exact input count from an Abacus streamed
// prompt_tokens (weighted) and its cached count, 0 for a cold frame. ok is
// false for a model without a measured weight, or when the uncached count
// would be negative (the frame does not follow the measured rule).
func abacusStreamPrompt(model string, weighted, cached int) (prompt int, ok bool) {
	w, ok := abacusCacheWeights[model]
	if !ok {
		return 0, false
	}
	uncached := weighted - cached*w[0]/w[1]
	if uncached < 0 {
		return 0, false
	}
	return uncached + cached, true
}

// NewClient builds a Client targeting baseURL. The URL should be the server
// root WITHOUT the /v1 suffix; per-endpoint paths are appended internally
// so callers don't have to remember which endpoints live under /v1. Any
// trailing slash on baseURL is stripped.
//
// The default http.Client uses a 5-minute timeout, matching the
// config.ProviderConfig default. Override via WithHTTPClient for shorter
// (low-latency local) or longer (large-batch embedding) requests.
//
// NewClient resolves the cache affinity mode and mints the client's random
// affinity secret; see WithCacheAffinity.
func NewClient(baseURL string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		userAgent: defaultUA,
		hc:        &http.Client{Timeout: 5 * time.Minute},
	}
	for _, opt := range opts {
		opt(c)
	}
	c.abacusRouteLLM = isAbacusRouteLLM(c.baseURL)
	switch c.cacheAffinity {
	case "":
		c.cacheAffinity = InferCacheAffinity(c.baseURL)
	case "none", "openai", "openrouter", "opencode":
	default:
		c.cacheAffinity = "none"
	}
	// rand.Read crashes the program rather than return an error (Go 1.24+);
	// should that change, a failure leaves no secret and so sends no key.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err == nil {
		c.affinitySecret = secret
	}
	return c
}

// affinityKey returns hex(HMAC-SHA256(secret, sessionID))[:32], or "" when
// sessionID is empty or the client has no secret, so a failure sends no key
// rather than one anybody could recompute.
func (c *Client) affinityKey(sessionID string) string {
	if sessionID == "" || c.affinitySecret == nil {
		return ""
	}
	m := hmac.New(sha256.New, c.affinitySecret)
	_, _ = m.Write([]byte(sessionID))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// promptCacheKey returns the chat body's prompt_cache_key for sessionID: the
// affinity key in "openai" mode, "" (omitted) otherwise.
func (c *Client) promptCacheKey(sessionID string) string {
	if c.cacheAffinity != "openai" {
		return ""
	}
	return c.affinityKey(sessionID)
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

	if err := decodeJSONOrError(resp, c.apiKey, out); err != nil {
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
		err := readErrorEnvelope(resp, c.apiKey)
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
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("openaicompat: %s: %w", path, err)
	}
	resp.Body = responsebound.Wrap(ctx, resp.Body)
	defer func() { _ = resp.Body.Close() }()
	return decodeJSONOrError(resp, c.apiKey, out)
}

// post is the shared transport for postJSON / postSSE. It marshals body
// to JSON, sets headers including auth, and returns the raw response for
// the caller to consume.
func (c *Client) post(ctx context.Context, path string, body any, contentType string) (*http.Response, error) {
	// HTTP trims surrounding SP/HTAB, which would collapse distinct session IDs.
	if id := sessionIDFrom(ctx); strings.Trim(id, " \t") != id {
		return nil, fmt.Errorf("openaicompat: session ID must not start or end with a space or tab")
	}
	buf, err := c.marshalBody(path, body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("openaicompat: build %s: %w", path, err)
	}
	c.setHeaders(req, contentType)
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: %s: %w", path, err)
	}
	resp.Body = responsebound.Wrap(ctx, resp.Body)
	return resp, nil
}

// do redacts errors from HTTP parsing before a response exists, while
// preserving their causes for cancellation and routing classification. A
// bounded request (responsebound) is sent through a per-request copy of the
// client that hands back any redirect unfollowed.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := responsebound.HTTPClient(req.Context(), c.hc).Do(req)
	return resp, redactError(c.apiKey, err)
}

// redactError returns err with apiKey, raw or JSON-escaped, replaced in its
// text, keeping err as the cause. nil and a key-less client return err as is.
func redactError(apiKey string, err error) error {
	key := strings.TrimRight(apiKey, " \t") // Match HTTP's trailing header whitespace trimming.
	if err == nil || key == "" {
		return err
	}
	quoted := strconv.Quote(key)
	message := strings.NewReplacer(
		key, "[redacted]",
		quoted[1:len(quoted)-1], "[redacted]",
	).Replace(err.Error())
	return &redactedError{err: err, message: message}
}

// marshalBody encodes body for post. Without a content guard that is plain
// json.Marshal. With one, a body the guard matches is refused with
// provider.ErrContentDenied before anything is sent, and an encoding failure is
// reported without the encoder's error text, which can quote the offending
// bytes. Headers are not part of the body and are never inspected.
func (c *Client) marshalBody(path string, body any) ([]byte, error) {
	match := c.contentGuard
	if match == nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("openaicompat: marshal %s body: %w", path, err)
		}
		return buf, nil
	}

	// Check the concrete fields first. An unrelated encoding fault must not
	// hide a credential and turn a sensitive denial into a marshal error.
	if unencodedBodyMatches(body, match) {
		return nil, contentDenied(path)
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: marshal %s body failed", path)
	}
	if secretscan.MatchJSON(buf, match) {
		return nil, contentDenied(path)
	}
	return buf, nil
}

func contentDenied(path string) error {
	return fmt.Errorf("openaicompat: %s: %w", path, provider.ErrContentDenied)
}

// unencodedBodyMatches checks the string fields in the three POST shapes
// before json.Marshal can fail on an unrelated value. The final MatchJSON in
// marshalBody still checks the exact wire representation, including optional
// fields and JSON decoding. post only receives these concrete value types.
func unencodedBodyMatches(body any, match func(string) bool) bool {
	stringsMatch := func(values []string) bool { return slices.ContainsFunc(values, match) }
	switch r := body.(type) {
	case chatRequest:
		if match(r.Model) || stringsMatch(r.Stop) || match(r.ToolChoice) || match(r.ReasoningEffort) || match(r.PromptCacheKey) || chatRawFieldsMatch(r, match) {
			return true
		}
		for _, m := range r.Messages {
			if match(m.Role) || match(m.Content) || match(m.ReasoningContent) || match(m.Name) || match(m.ToolCallID) {
				return true
			}
			for _, tc := range m.ToolCalls {
				if match(tc.ID) || match(tc.Type) || match(tc.Function.Name) {
					return true
				}
			}
		}
		for _, t := range r.Tools {
			if match(t.Type) || match(t.Function.Name) || match(t.Function.Description) {
				return true
			}
		}
		for key, value := range r.ChatTemplateKwargs {
			if match(key) {
				return true
			}
			if encoded, err := json.Marshal(value); err == nil && secretscan.MatchJSON(encoded, match) {
				return true
			}
		}
	case completionRequest:
		return match(r.Model) || match(r.Prompt) || match(r.Suffix) || stringsMatch(r.Stop)
	case embedRequest:
		return match(r.Model) || stringsMatch(r.Input)
	}
	return false
}

// chatRawFieldsMatch reports whether match accepts a json.RawMessage in r: a
// tool call's arguments or a tool's parameter schema. Arguments get the extra
// pass for the string envelope that wraps the real document on the wire, and for
// malformed text, so a credential spelled with JSON escapes inside it is still
// seen. The checks run on the values as given, not as json.Marshal would write
// them.
func chatRawFieldsMatch(r chatRequest, match func(string) bool) bool {
	for _, m := range r.Messages {
		for _, tc := range m.ToolCalls {
			if secretscan.MatchToolArguments(tc.Function.Arguments, match) {
				return true
			}
		}
	}
	for _, t := range r.Tools {
		if secretscan.MatchJSON(t.Function.Parameters, match) {
			return true
		}
	}
	return false
}

// setHeaders applies Authorization (when APIKey is set), Accept, User-Agent,
// the cache affinity header for the session id carried by the request context
// (see WithCacheAffinity), and optional Content-Type to req.
func (c *Client) setHeaders(req *http.Request, contentType string) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", c.userAgent)
	id := sessionIDFrom(req.Context())
	switch c.cacheAffinity {
	case "opencode":
		if id != "" {
			req.Header.Set("x-opencode-session", id)
		}
	case "openrouter":
		if key := c.affinityKey(id); key != "" {
			req.Header.Set("x-session-id", key)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
}

// decodeJSONOrError reads resp.Body. On 2xx, it JSON-decodes into out. On
// non-2xx, it parses an OpenAI error envelope and returns it as an error.
func decodeJSONOrError(resp *http.Response, apiKey string, out any) error {
	if resp.StatusCode/100 != 2 {
		return readErrorEnvelope(resp, apiKey)
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
//
// A non-empty apiKey is replaced with "[redacted]" in the message and the
// status: servers that echo the Authorization header, in the body or in the
// server-chosen reason phrase, would otherwise put the key into bootstrap
// warnings and logs.
//
// For an ordinary request a failed body read is ignored, as it always was. For
// a bounded request (responsebound) it returns a *responsebound.RejectionReadError
// carrying the status and the redacted read failure instead: a rejection whose
// body stalled, reset or ended short is not a clean rejection. A clean end,
// empty included, and a body cut at the cap below are not failed reads.
func readErrorEnvelope(resp *http.Response, apiKey string) error {
	const limit = 64 * 1024
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit))
	if readErr != nil && resp.Request != nil && responsebound.Bounded(resp.Request.Context()) {
		return &responsebound.RejectionReadError{StatusCode: resp.StatusCode, Err: redactError(apiKey, readErr)}
	}
	body = bytes.TrimSpace(body)

	redact := func(m string) string {
		if apiKey == "" {
			return m
		}
		return strings.ReplaceAll(m, apiKey, "[redacted]")
	}
	status := redact(resp.Status)

	var env errorEnvelope
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		return &statusError{statusCode: resp.StatusCode, status: status, message: redact(env.Error.Message)}
	}
	if len(body) > 0 {
		return &statusError{statusCode: resp.StatusCode, status: status, message: redact(string(body))}
	}
	return &statusError{statusCode: resp.StatusCode, status: status}
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

type redactedError struct {
	err     error
	message string
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.err }
