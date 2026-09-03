// Package interceptor provides the default detection-class interceptors for
// the agent pipeline (#436): zero-width characters (raw and JSON-escaped),
// encoded instructions (base64 in all four stdlib forms, hex, line-folded
// runs, one decoded layer), and typoglycemia (scrambled-interior)
// instruction phrases.
//
// Policy is shared. Strong, imperative phrases follow the origin: content the
// loop classified user, system, model-authored or workspace-local is TAGGED;
// everything else (foreign, unknown, invalid) is BLOCKED. A strong phrase
// anywhere dominates a weak indicator anywhere; earliest position breaks
// ties within one severity. Weak indicators ("system prompt", "you are now")
// only tag, at low risk, so a benign foreign document that mentions a system
// prompt is not blocked. Detectors are telemetry and risk inputs, never the
// sole gate for workspace content (#429). None inspects model output; that
// policy is #438/#437.
package interceptor
