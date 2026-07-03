package main

import (
	"fmt"
	"net/url"
	"strings"
)

// validateBaseURLOverride normalizes and validates an explicit backend URL
// override (-base-url flag or GO_LLM_BASE_URL env). source names the origin
// for error messages. The value is the openai-compat server ROOT — the client
// appends /v1 paths itself — so a path that ends in /v1 is rejected with a
// hint; other path prefixes stay allowed for reverse-proxy setups. URLs in
// error messages are userinfo-redacted (never leak credentials into logs).
func validateBaseURLOverride(raw, source string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("golem: %s: value is empty; pass the openai-compat server root, e.g. http://127.0.0.1:8081", source)
	}
	u, err := url.ParseRequestURI(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("golem: %s: URL %q must include scheme and host (server root, e.g. http://127.0.0.1:8081)", source, redactBaseURL(v))
	}
	if p := strings.TrimRight(u.Path, "/"); strings.HasSuffix(p, "/v1") {
		return "", fmt.Errorf("golem: %s: pass the server root without the /v1 suffix (the client appends API paths): got %q", source, redactBaseURL(v))
	}
	return v, nil
}
