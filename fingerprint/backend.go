package fingerprint

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// localHosts maps hostnames that resolve to the local machine to "localhost".
// 0.0.0.0 (INADDR_ANY) is included because Ollama binds to it by default;
// while not technically a loopback address, connections to it reach localhost.
var localHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
	"0.0.0.0":   true,
}

// NormalizeBackendID produces a canonical backend ID from a raw URL.
// It lowercases scheme and host, canonicalizes loopback addresses to "localhost",
// strips trailing slashes, userinfo, query, and fragment.
func NormalizeBackendID(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("fingerprint: invalid backend URL %q: %w", rawURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("fingerprint: invalid backend URL %q: missing scheme or host", rawURL)
	}

	// Lowercase scheme
	u.Scheme = strings.ToLower(u.Scheme)

	// Split host and port
	host := u.Hostname()
	port := u.Port()

	// Lowercase and canonicalize host
	host = strings.ToLower(host)
	if localHosts[host] {
		host = "localhost"
	}

	// Rebuild host with port, handling IPv6 bracket insertion correctly
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else {
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		} else {
			u.Host = host
		}
	}

	// Strip trailing slashes from path
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = "" // Force String() to derive from cleaned Path

	// Strip userinfo, query, fragment
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""

	return u.String(), nil
}

// IsLocalBackend reports whether the given backend URL points to the local machine.
// Checks for localhost, 127.0.0.1, [::1], and 0.0.0.0.
func IsLocalBackend(backendURL string) bool {
	u, err := url.Parse(backendURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return localHosts[host]
}
