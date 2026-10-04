// Package mcpclient adapts external MCP tools into agent.Tool values. It is the
// client counterpart to mcp and does not reuse server handler logic.
//
// Connect requires ConnectOptions with a concrete PinStore from NewPinStore.
// Pins bind complete normalized model-facing catalogs, and the connection each
// came from, to canonical workspaces and operator-selected aliases in private
// user data outside the workspace. Ordinary connections pin the first valid
// catalog, including an empty catalog; RequirePinned connections never create
// pins. Missing stores are configuration errors. Per-alias rejection is a bare
// *AdmissionError in warnings (a type assertion suffices), whose text may name
// a fixed rule after the reason (launch_invalid); first-pin and
// description-truncation notices are ordinary warnings. Healthy aliases retain
// configuration order in Manager.Tools. Callers must close the Manager, and
// strict all-alias admission callers must reject any AdmissionError themselves.
//
// Server.WithTools selects exact original tool names per alias. Selection
// applies after complete-catalog admission: pins, diffs and Inspect always
// cover every tool, a changed unselected tool still blocks the alias, and a
// selected name absent from the admitted catalog blocks the alias with
// selection_missing. Omitted selection exposes the whole catalog; an explicit
// empty selection exposes none. Every exposed tool keeps per-call approval
// (agent.ApprovalAlways).
//
// Connect, Inspect and Approve prepare each server once and launch only what
// they prepared. A stdio server's launcher is resolved to an absolute path and
// its symlink target, and it runs in a symlink-resolved working directory
// (WithDir, or the process working directory captured at preparation) with the
// platform baseline environment plus WithEnv additions, never the inherited
// environment. InheritEnv forwards the parent's value read at launch, and an
// unset name blocks the alias with env_unset; SetEnv supplies a value and may
// not name a baseline variable. The Windows policy also always sets
// NoDefaultCurrentDirectoryInExePath=1, so a bare program name is not looked up
// in the working directory before PATH; additions may not name it. An HTTP
// endpoint keeps its path and query exactly; userinfo, fragments, dot segments,
// backslashes, zone IDs and non-ASCII hosts are fatal invalid_config errors.
// Every request must target that endpoint and every redirect is refused,
// session close included; a refused close redirect never fails admission,
// Inspect or Approve, and Manager.Close reports it.
//
// The prepared identity is fingerprinted with a per-user HMAC key
// (connection-hmac.pem beside the pin directories, created by NewPinStore when
// missing; an unreadable, insecure or corrupt key fails NewPinStore and is
// never replaced). Version 2 pin records store the fingerprint and per-field
// keyed digests beside the catalog. Connect checks the connection before
// anything is launched or contacted: a difference is connection_changed with
// fixed field labels, and a version 1 record is connection_missing in every
// mode until Approve. A recreated key reports every pin as connection_changed
// (key). Fingerprints, argv, URL paths and queries, and environment values
// never appear in AdmissionError text. Tool-call transport failures reach the
// model only as fixed texts or the server's own JSON-RPC error message, and
// Manager.Close reports a fixed text with causes available through errors.Is
// and errors.As.
//
// Inspect writes no pin record, launches or contacts the candidate (an explicit
// operator action), and renders quoted definitions with the candidate
// connection's local paths, environment names or HTTP origin, never argv, URL
// paths or queries. Approve requires both ApprovalDigests fields from Inspect,
// checks the connection fingerprint before launching anything, re-fetches, and
// publishes only if the catalog digest matches and the prior pin revision is
// unchanged. Approval invokes no tools and grants no tool execution authority.
// Its Diff is names-only; Inspection.String includes full definitions and
// belongs only in explicit operator inspection. Publication durability errors
// can mean bytes already exist; they never indicate successful approval.
//
// Catalogs reject incomplete listings, repeated cursors, duplicate/nil/invalid
// tools, over 128 tools, over 100 pages, and schemas over 32 KiB. Descriptions are
// normalized before registration; complete object schemas are pinned unchanged,
// including nested strings. Hashes cover canonical SDK-decoded values, not wire
// number spellings or information already lost to decoding. Order of tools and
// object keys is irrelevant; array order and distinct decoded values matter.
//
// TOFU detects definition and connection drift, not malicious initial
// definitions, a program replaced at the same path, a file or symlink swapped
// between check and launch, or changed behavior behind unchanged catalogs and
// launchers. The child's PATH keeps only its absolute entries (a quoted Windows
// entry is judged unquoted and kept as written, one holding a separator only
// when wholly quoted); a PATH with none is omitted, leaving each program its
// own default search path. PATH values are not identity: a wrapper (env, npx,
// uvx, sh -c) or a script's #! interpreter binds only the launcher, not what it
// later finds through those entries. Stdio servers keep the host user's
// filesystem and network authority. A backup holding both the key and the pins
// allows offline guessing of low-entropy argv or query secrets. New workspaces
// and aliases have fresh trust namespaces. Live catalog-change notifications
// are not handled. Foreign-result provenance and observation fencing remain
// independent.
package mcpclient
