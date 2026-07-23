package mcpclient

import "regexp"

// maxComposedNameLen caps the model-facing tool name. The SDK permits 128 chars
// and '.', but provider function-name schemas are narrower (OpenAI: 64,
// [A-Za-z0-9_-]); we validate to the strict provider ceiling on our side.
const maxComposedNameLen = 64

var (
	nameRE  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	aliasRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

func validAlias(a string) bool { return aliasRE.MatchString(a) }

// composeName builds the namespaced "mcp__<alias>__<remote>" name and reports
// whether it is usable as a provider tool name. A remote name with characters
// outside the provider set, or an over-long composed name, is rejected so the
// caller can skip+warn rather than register an invalid tool.
func composeName(alias, remote string) (string, bool) {
	name := "mcp__" + alias + "__" + remote
	if len(name) > maxComposedNameLen || !nameRE.MatchString(name) {
		return "", false
	}
	return name, true
}
