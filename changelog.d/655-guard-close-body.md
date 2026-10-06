### Fixed — The destination guard closes the request body when it denies a request (#655)

A client from `provider.GuardHTTPClient` that denied a request returned the
error without closing the request body. Denials include a missing or revoked
capability, another provider's capability, a `Request.Host` override, an
off-target URL and a nil URL. The `http.RoundTripper` contract requires the
body to be closed on errors too, and `http.Client` never closes it after a
transport error. A guarded caller that streamed a pipe-backed body therefore
leaked the writer goroutine, blocked on `Write`, on every denial. The guard now
closes the body on every denial. An admitted request still passes its body to
the underlying transport unclosed.
