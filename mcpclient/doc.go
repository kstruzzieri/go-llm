// Package mcpclient adapts external MCP tools into agent.Tool values. It is the
// client counterpart to mcp and does not reuse server handler logic.
//
// Connect requires ConnectOptions with a concrete PinStore from NewPinStore.
// Pins bind complete normalized model-facing catalogs to canonical workspaces
// and operator-selected aliases in private user data outside the workspace.
// Ordinary connections pin the first valid catalog, including an empty catalog;
// RequirePinned connections never create pins. Missing stores are configuration
// errors. Per-alias rejection is an *AdmissionError in warnings; first-pin and
// description-truncation notices are ordinary warnings. Healthy aliases retain
// configuration order in Manager.Tools. Callers must close the Manager, and
// strict all-alias admission callers must reject any AdmissionError themselves.
//
// Inspect never writes a pin and renders quoted definitions. Approve re-fetches
// and publishes only the exact supplied candidate digest if the prior pin
// revision is unchanged. Approval invokes no tools and grants no tool execution
// authority. Its Diff is names-only; Inspection.String includes full definitions
// and belongs only in explicit operator inspection. Publication durability errors
// can mean bytes already exist; they never indicate successful approval.
//
// Catalogs reject incomplete listings, repeated cursors, duplicate/nil/invalid
// tools, over 128 tools, over 100 pages, and schemas over 32 KiB. Descriptions are
// normalized before registration; complete object schemas are pinned unchanged,
// including nested strings. Hashes cover canonical SDK-decoded values, not wire
// number spellings or information already lost to decoding. Order of tools and
// object keys is irrelevant; array order and distinct decoded values matter.
//
// TOFU detects definition drift, not malicious initial definitions, process
// identity, or changed behavior behind unchanged catalogs. New workspaces and
// aliases have fresh trust namespaces. Live catalog-change notifications are not
// handled. Foreign-result provenance and observation fencing remain independent.
package mcpclient
