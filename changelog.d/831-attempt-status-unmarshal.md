### Fixed — Routed run records and Golem traces decode in Go (#831)

`provider.AttemptStatus` now has `UnmarshalJSON`, so a serialized `agent.Result` or `-trace` file from a routed run decodes. An unrecognized status reads as unknown. A number or other non-string status is now an error (earlier readers accepted a number). `null` leaves the value unchanged.
