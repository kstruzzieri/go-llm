package interceptor

import "slices"

// CredentialRuleNames exposes the read rule's name lists to external tests,
// which check the model-facing descriptions that spell the set out (#627).
func CredentialRuleNames() []string {
	return slices.Concat(credentialDirs, credentialFiles, envTemplates)
}
