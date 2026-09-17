### Security — Pin external MCP catalogs per workspace (#432)

- Normalize remote descriptions and pin complete model-facing catalogs by workspace and server alias. Reject incomplete, invalid, duplicate, or oversized catalogs as a whole; changed aliases are blocked while healthy REPL attachments continue.
- Add `golem mcp inspect` and exact-digest `golem mcp approve`, with explicit aliases, safely quoted inspection details, atomic revision-checked publication, and names-only startup/approval diagnostics.
- Require existing matching pins for every MCP alias under `-p` before provider discovery or capability probes. Admission failures return exit 1 and the pre-run `mcp_untrusted` machine result; MCP execution still requires interactive approval.
- Change `mcpclient.Connect` to require concrete `ConnectOptions{Pins, RequirePinned}`. Activate the public HTTP ZT-603 contract with real temporary pins under the existing 500 ms hardening-suite gate.
