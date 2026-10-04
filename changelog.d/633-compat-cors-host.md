### Security — compat refuses cross-origin browser calls by default and validates the Host header (#633)

`compat.New` no longer sends `Access-Control-Allow-Origin: *` by default. The
server is unauthenticated, so that default let any website the user visited
call the local shim and read model responses. CORS is now off unless
configured: browser clients opt in with `compat.WithCORS`, passing their exact
origin.

Turning CORS off only hides responses, since a page can still send a
`text/plain` POST that needs no preflight and run a model blind. The server
now also refuses, with a 403 `cross_origin_not_allowed` error, POSTs that a
browser marks as coming from another origin (by `Sec-Fetch-Site`, or by an
`Origin` that does not match the `Host`), unless that origin is the
`WithCORS` origin; `WithCORS("*")` turns this check off. Requests without
those headers, as SDKs, CLIs and other non-browser clients send them, are
unaffected.

It also refuses, with a 403 `host_not_allowed` error, any request whose
`Host` header is missing or names something other than a loopback address
(`localhost`, `127.0.0.0/8`, `::1`), the host part of `WithAddr`, or a name
added with the new `compat.WithAllowedHosts`. This blocks DNS rebinding, where
an attacker's domain resolves to 127.0.0.1 and the browser treats the shim as
same-origin, so neither CORS nor the cross-origin check applies. The Host
check runs before CORS and every route, preflights included. Both refusals
are logged.

#### Upgrade note

Browser clients that relied on the `*` default must pass their origin to
`WithCORS`; a malformed origin (for example one with a trailing slash) is
logged and trusts nothing. Clients that reach the server under another name,
such as `host.docker.internal` from a container, or LAN clients of a
`WithTLS` server bound to a wildcard address (`:port`, `0.0.0.0`), must be
listed with `WithAllowedHosts`. Host matching ignores ports and case.
