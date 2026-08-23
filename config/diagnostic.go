// Package-internal typed diagnostics (spec §6). The wrapper is unexported;
// consumers use DiagnosticOf. Error text passes through unchanged so pinned
// messages and CLI output are untouched.

package config

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrorCode is the bounded vocabulary config errors expose to consumers.
// Codes never carry paths or values; pair with SubjectKind/Subject.
type ErrorCode string

// SubjectKind classifies a Diagnostic's Subject identifier.
type SubjectKind string

// Exported code constants — consumers never duplicate string literals.
const (
	CodeConfigNotFound          ErrorCode = "config_not_found"
	CodeConfigDiscoveryInvalid  ErrorCode = "config_discovery_invalid"
	CodeIO                      ErrorCode = "io"
	CodeParseError              ErrorCode = "parse_error"
	CodeRenderError             ErrorCode = "render_error"
	CodeDuplicateKeys           ErrorCode = "duplicate_keys"
	CodeProviderRequired        ErrorCode = "provider_required"
	CodeProviderNameInvalid     ErrorCode = "provider_name_invalid"
	CodeProviderEndpointInvalid ErrorCode = "provider_endpoint_invalid"
	CodeProviderFormatInvalid   ErrorCode = "provider_format_invalid"
	CodeSlotPolicyInvalid       ErrorCode = "slot_policy_invalid"
	CodeModelInvalid            ErrorCode = "model_invalid"
	CodeThinkInvalid            ErrorCode = "think_invalid"
	CodeProviderNotFound        ErrorCode = "provider_not_found"
	CodeDefaultsInvalid         ErrorCode = "defaults_invalid"
	CodeKeyReferenceMalformed   ErrorCode = "key_reference_malformed"
	CodeKeyReferenceUnavailable ErrorCode = "key_reference_unavailable"
	CodeInvalidArgument         ErrorCode = "invalid_argument"
	CodeRoleNotFound            ErrorCode = "role_not_found"
	CodeProviderExists          ErrorCode = "provider_exists"
	CodeProviderInUse           ErrorCode = "provider_in_use"
	CodeEligibilityIneligible   ErrorCode = "eligibility_ineligible"
	CodeEligibilityUnknown      ErrorCode = "eligibility_unknown"
	CodeSelectorConflict        ErrorCode = "selector_conflict"
	CodeTargetExists            ErrorCode = "target_exists"
	CodeRevisionConflict        ErrorCode = "revision_conflict"
	CodeDurabilityUncertain     ErrorCode = "durability_uncertain"
)

// Subject kind constants — classify what a Diagnostic's Subject names.
const (
	SubjectNone     SubjectKind = ""
	SubjectProvider SubjectKind = "provider"
	SubjectRole     SubjectKind = "role"
	SubjectUseCase  SubjectKind = "use_case"
)

// Diagnostic is the boundary-safe value projection of a config error.
// Never paths, never secret values, never raw error text.
// A non-none SubjectKind describes the failing site even when Subject is
// empty; consumers must not assume a kind implies a non-empty identifier.
type Diagnostic struct {
	Code        ErrorCode
	SubjectKind SubjectKind
	Subject     string
}

type diagError struct {
	diag Diagnostic
	err  error
}

func (e *diagError) Error() string { return e.err.Error() }
func (e *diagError) Unwrap() error { return e.err }

// diagWrap attaches a diagnostic to err. Subject is sanitized here so no
// call site can forget the discipline.
func diagWrap(code ErrorCode, kind SubjectKind, subject string, err error) error {
	return &diagError{
		diag: Diagnostic{Code: code, SubjectKind: kind, Subject: sanitizeSubject(subject)},
		err:  err,
	}
}

// DiagnosticOf extracts the first attached diagnostic from err's chain.
// Raw sentinel chains (pre-existing call sites) classify by errors.Is.
func DiagnosticOf(err error) (Diagnostic, bool) {
	if err == nil {
		return Diagnostic{}, false
	}
	var de *diagError
	if errors.As(err, &de) {
		return de.diag, true
	}
	switch {
	case errors.Is(err, ErrRevisionConflict):
		return Diagnostic{Code: CodeRevisionConflict}, true
	case errors.Is(err, ErrDurabilityUncertain):
		return Diagnostic{Code: CodeDurabilityUncertain}, true
	case errors.Is(err, ErrConfigNotFound):
		return Diagnostic{Code: CodeConfigNotFound}, true
	}
	return Diagnostic{}, false
}

// sanitizeSubject: fixed order per spec §6 — repair invalid UTF-8 to
// U+FFFD, replace control (Cc) and format (Cf, incl. bidi) runes with
// U+FFFD, THEN truncate rune-safe to 64 bytes. Replacement can grow the
// intermediate value; the final truncation restores the bound.
func sanitizeSubject(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s { // invalid bytes decode as utf8.RuneError
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			r = utf8.RuneError
		}
		b.WriteRune(r)
	}
	return truncateRuneSafe64(b.String())
}

// truncateRuneSafe64 cuts s to at most 64 bytes on a rune boundary —
// the shared bound for diagnostic subjects and mutation reason items.
func truncateRuneSafe64(s string) string {
	if len(s) <= 64 {
		return s
	}
	cut := 64
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
