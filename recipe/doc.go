// Package recipe parses reusable, versioned prompt bundles.
// Parse preserves template text; ValidateTemplates checks references and Expand
// substitutes declared inputs with a caller-supplied byte limit.
package recipe
