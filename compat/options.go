package compat

import (
	"path"
	"time"
)

// Option configures a Server at construction time.
type Option func(*Server)

// WithAddr sets the TCP listen address. Default "127.0.0.1:18741".
// Non-loopback addresses require WithTLS.
func WithAddr(addr string) Option {
	return func(s *Server) { s.addr = addr }
}

// WithBasePath sets the HTTP path prefix under which endpoints are mounted.
// Default "/v1". A trailing slash is normalized away.
func WithBasePath(prefix string) Option {
	return func(s *Server) { s.basePath = normalizeBase(prefix) }
}

// WithCORS sets the Access-Control-Allow-Origin value, which is also the one
// browser origin allowed to send state-changing requests. Default ""
// (disabled): the server sends no CORS headers and refuses POSTs that
// browsers mark as cross-origin, so pages on other origins can neither read
// responses nor run models blind. Browser clients opt in by passing their
// exact origin as the browser sends it, e.g. "https://app.example": lower
// case, no path or trailing slash, no default port. ListenAndServe returns
// ErrInvalidCORSOrigin for anything else. The server is unauthenticated, so
// "*" lets any website the user visits call it and read the results.
func WithCORS(origin string) Option {
	return func(s *Server) { s.corsOrigin = origin }
}

// WithAllowedHosts adds names the server accepts in the Host header besides
// loopback names and the WithAddr host, e.g. "host.docker.internal" for a
// container reaching the host, or the names LAN clients use when the server
// binds a wildcard address. Requests naming any other host are refused with
// 403, which blocks DNS rebinding. Entries are exact names, without
// wildcards; matching ignores ports and case. Repeated calls accumulate.
func WithAllowedHosts(hosts ...string) Option {
	return func(s *Server) {
		for _, h := range hosts {
			s.allowedHosts = append(s.allowedHosts, hostname(h))
		}
	}
}

// WithTLS enables HTTPS using the given certificate and private key paths.
// Required when binding to a non-loopback address.
func WithTLS(certFile, keyFile string) Option {
	return func(s *Server) {
		s.tlsCert = certFile
		s.tlsKey = keyFile
	}
}

// WithAliases installs a model-name translation map. Keys are the names
// clients send on the wire (e.g. "gpt-4"); values are canonical provider-
// qualified selectors (e.g. "ollama/qwen3-coder-next:latest") or unqualified
// model names (e.g. "qwen3:8b"). Unqualified values constrain the request by
// model name across providers; qualified values pin the backend.
//
// Passing nil clears any previously configured aliases. The default is empty.
func WithAliases(aliases map[string]string) Option {
	return func(s *Server) {
		if aliases == nil {
			s.aliases = map[string]string{}
			return
		}
		m := make(map[string]string, len(aliases))
		for k, v := range aliases {
			m[k] = v
		}
		s.aliases = m
	}
}

// WithMaxConcurrency caps simultaneous in-flight requests. Default 4.
// The reserve for PriorityHigh requests is derived internally; see
// reserveFor. Values <= 0 are treated as 1.
func WithMaxConcurrency(n int) Option {
	return func(s *Server) {
		if n < 1 {
			n = 1
		}
		s.maxConcurrency = n
	}
}

// WithEmbeddings enables the POST /v1/embeddings endpoint. Default false.
// Opt-in only — embedding requests can trigger surprise model loads.
func WithEmbeddings(enabled bool) Option {
	return func(s *Server) { s.embeddingsEnabled = enabled }
}

// WithShutdownTimeout sets how long Close waits for in-flight requests.
// Default 30 seconds. Values <= 0 are treated as no-op so the default set by
// New survives (callers can wire up optional configuration without a branch).
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.shutdownTimeout = d
		}
	}
}

// normalizeBase canonicalizes an HTTP base path prefix:
//   - empty input returns ""
//   - a leading slash is added when missing
//   - consecutive slashes anywhere in the path are collapsed (path.Clean)
//   - a trailing slash (and "/" or "//..." that collapses to "/") is mapped to ""
//     so callers can join "basePath + /route" unambiguously
func normalizeBase(prefix string) string {
	if prefix == "" {
		return ""
	}
	if prefix[0] != '/' {
		prefix = "/" + prefix
	}
	prefix = path.Clean(prefix)
	if prefix == "/" {
		return ""
	}
	return prefix
}
