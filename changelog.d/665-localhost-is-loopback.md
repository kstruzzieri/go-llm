### Changed — `localhost` destinations always dial loopback (#665)

The destination guard no longer resolves a `localhost` base URL. It dials
127.0.0.1, then ::1, on the destination's port, as RFC 6761 permits and
browsers do. A hosts file that maps `localhost` elsewhere can no longer
redirect a local provider, and it no longer stops `golem.New`'s config-driven
bootstrap, Golem or `go-llm-mcp` at startup with `provider.ErrDestinationDenied`
(#654): requests reach the local backend instead. A backend listening on only
one of the two stacks is still reached.
