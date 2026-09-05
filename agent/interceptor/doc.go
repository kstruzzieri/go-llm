// Package interceptor provides the default interceptors for the agent
// pipeline: three detectors (#436) and two guards (#439).
//
// Detectors: zero-width characters (raw and JSON-escaped), encoded
// instructions (base64 in all four stdlib forms, hex, line-folded runs, one
// decoded layer), and typoglycemia (scrambled-interior) instruction phrases.
// Their policy is shared. Strong, imperative phrases follow the origin:
// content the loop classified user, system, model-authored or
// workspace-local is TAGGED; everything else (foreign, unknown, invalid) is
// BLOCKED. A strong phrase anywhere dominates a weak indicator anywhere;
// earliest position breaks ties within one severity. Weak indicators
// ("system prompt", "you are now") only tag, at low risk, so a benign
// foreign document that mentions a system prompt is not blocked. Detectors
// are telemetry and risk inputs, never the sole gate for workspace content
// (#429). None inspects model output; that policy is #438/#437.
//
// Guards inspect tool calls only and add no model-visible trailer.
// Invariants is a declarative table of per-tool argument bounds: protected
// and credential path components (and the exact basename .env on direct
// reads), and an inline shell script that pipes a recognized remote fetch
// into a shell. A violation BLOCKS the call before Plan and approval
// regardless of origin, because invariants are policy, not detection; the
// finding's Rule names the invariant and the guard reads arguments with the
// tool decoder's own name equivalence. Egress classifies an exec-class argv
// as privileged, network, package-manager, interpreter or unknown (anything
// outside an explicit quiet set, including any wrapper or subcommand form
// it cannot parse) and TAGS it; the class and a short label ride the
// finding so an approver can render a badge on the prompt. Both are finite
// recognizers over literal words: nothing here evaluates a shell, resolves
// an executable, or touches the filesystem or network.
package interceptor
