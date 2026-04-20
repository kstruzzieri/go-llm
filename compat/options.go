package compat

import "time"

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

// WithCORS sets the Access-Control-Allow-Origin value. Default "*".
// Pass the empty string to disable CORS headers entirely.
func WithCORS(origin string) Option {
	return func(s *Server) { s.corsOrigin = origin }
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
// Default 30 seconds.
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.shutdownTimeout = d
		}
	}
}

// normalizeBase trims trailing slashes, forces a leading slash when non-empty,
// and maps empty or "/" to "".
func normalizeBase(prefix string) string {
	if prefix != "" && prefix[0] != '/' {
		prefix = "/" + prefix
	}
	for len(prefix) > 0 && prefix[len(prefix)-1] == '/' {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix
}
