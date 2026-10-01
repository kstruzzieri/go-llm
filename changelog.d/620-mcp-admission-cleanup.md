### Fixed — MCP admission keeps the refusal reason and first-pin notice under cancellation (#620)

- A refused MCP server (`catalog_changed`, `pin_missing`, `pin_conflict`, `invalid_catalog`) whose session close returned `context.Canceled` was reported as `canceled`, hiding the real refusal from the operator. The reason is now classified from the refusal alone; the close error stays in the cause chain, so `errors.Is(err, context.Canceled)` still holds.
- When `Connect` was canceled after a server's catalog was admitted, the "first pin" notice for the pin just written to disk was dropped. The notice is now reported ahead of the `canceled` failure, in server config order.
