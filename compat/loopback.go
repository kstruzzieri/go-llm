package compat

import "net"

// isLoopback reports whether the host part of addr is a loopback address.
// An empty host (e.g. ":8080") is NOT considered loopback because Go's
// net/http binds to 0.0.0.0 (all interfaces) in that case.
//
// Mirrors mcp/transport.go:71-84.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
