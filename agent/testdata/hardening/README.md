# Hardening contract fixtures

These synthetic bytes were hand-authored from the approved #451 policy contract
before running the new assertions. They contain no user data. Do not generate
expectations from implementation helpers, constants, captured output, or an
update-golden command. Fixture reads fail on missing files.

`.input` is literal input; `.want` pins complete provider-visible bytes. The
only variable is the authentic 12-character base32 nonce: its two designated
`{{TOOL_FRAME_NONCE}}` slots occur in the opening and final closing lines.
Inner marker-looking text, LF/CRLF, NUL, Unicode and ANSI sequences remain
literal. The local attributes disable Git newline conversion for both suffixes.

| Group | Required outcome | Distinguishing regression |
| --- | --- | --- |
| Baseline | nil tool / empty goal rejected; reserve wired; token events bounded | existing focused checks |
| Framing | ordinary, empty, trailing LF, Unicode/control and inner markers preserved | changed outer marker or content byte fails exact comparison |
| Run observations | ordinary, synthetic, tag, block, verifier inside one frame | changed outcome/trailer or frame fails |
| Trust text | complete system text before interceptor addendum | any wording/order change fails |
| Assembly | legacy raw fallback; mixed join or omission placeholder | changed selection or framing fails |
| Accounting | 93 bytes / default 24 tokens, exact fit and one below; raw standalone cost; wrapped compactor | dropped transport charge fails |
| ANSI | observation controls preserved; metadata line breaks become spaces | removed ESC or changed line flattening fails |
| Oracle | malformed markers/slots, mismatched keys, trailing bytes rejected | invalid cases must return an error |

Deferred coverage: ZT-602/#431 project trust, ZT-603/#432 MCP description/catalog
trust, ZT-604/#433 terminal output, ZT-605/#434 quarantine, and ZT-606/#435
retrieval screening. These are explicit skipped subtests, not implementation
claims. ANSI preservation at the agent boundary does not test a terminal.
Framing is structural; these tests do not establish model obedience.
#452 owns the broader corpus; #453 owns the dedicated security gate.
