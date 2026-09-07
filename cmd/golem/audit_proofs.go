package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/agentflow"
)

const auditProofAssurance = "structural/checksum; unsigned"

var auditProofViolationMessages = map[string]string{
	"artifact_malformed":     "agentflow proof artifact is malformed",
	"reference_missing":      "agentflow proof reference is missing",
	"reference_invalid":      "agentflow proof reference is invalid",
	"checksum_mismatch":      "agentflow proof checksum does not match",
	"derived_state_mismatch": "agentflow proof derived state is inconsistent",
}

var auditProofIncompleteMessages = map[string]string{
	"proof_missing":      "agentflow proof is missing",
	"source_unreadable":  "agentflow proof source is unreadable",
	"schema_unsupported": "agentflow proof schema is unsupported",
	"source_changed":     "agentflow proof source changed during verification",
	"canceled":           "agentflow proof verification was canceled",
}

type auditProofResponse struct {
	SchemaVersion     *string                `json:"schema_version"`
	Outcome           *string                `json:"outcome"`
	Assurance         *string                `json:"assurance"`
	CheckedSteps      *int64                 `json:"checked_steps"`
	CheckedSources    *int64                 `json:"checked_sources"`
	Violations        []auditProofViolation  `json:"violations"`
	IncompleteReasons []auditProofIncomplete `json:"incomplete_reasons"`
}

type auditProofViolation struct {
	Code    *string `json:"code"`
	Target  *string `json:"target"`
	Message *string `json:"message"`
}

type auditProofIncomplete struct {
	Code    *string `json:"code"`
	Path    *string `json:"path"`
	Message *string `json:"message"`
}

func (r *auditProofResponse) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var err error
	if r.SchemaVersion, err = requiredJSONField[string](fields, "schema_version"); err != nil {
		return err
	}
	if r.Outcome, err = requiredJSONField[string](fields, "outcome"); err != nil {
		return err
	}
	if r.Assurance, err = requiredJSONField[string](fields, "assurance"); err != nil {
		return err
	}
	if r.CheckedSteps, err = requiredJSONField[int64](fields, "checked_steps"); err != nil {
		return err
	}
	if r.CheckedSources, err = requiredJSONField[int64](fields, "checked_sources"); err != nil {
		return err
	}
	violations, err := requiredJSONField[[]auditProofViolation](fields, "violations")
	if err != nil {
		return err
	}
	r.Violations = *violations
	incomplete, err := requiredJSONField[[]auditProofIncomplete](fields, "incomplete_reasons")
	if err != nil {
		return err
	}
	r.IncompleteReasons = *incomplete
	return nil
}

func (v *auditProofViolation) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var err error
	if v.Code, err = requiredJSONField[string](fields, "code"); err != nil {
		return err
	}
	if v.Target, err = requiredJSONField[string](fields, "target"); err != nil {
		return err
	}
	v.Message, err = requiredJSONField[string](fields, "message")
	return err
}

func (r *auditProofIncomplete) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var err error
	if r.Code, err = requiredJSONField[string](fields, "code"); err != nil {
		return err
	}
	if r.Path, err = requiredJSONField[string](fields, "path"); err != nil {
		return err
	}
	r.Message, err = requiredJSONField[string](fields, "message")
	return err
}

func requiredJSONField[T any](fields map[string]json.RawMessage, name string) (*T, error) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("missing JSON member %q", name)
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func auditProofs(ctx context.Context, root string, runner agentflow.Runner) auditResult {
	result := auditResult{scope: "proofs", assurance: auditProofAssurance}
	info, err := os.Lstat(filepath.Join(root, ".agent"))
	if errors.Is(err, os.ErrNotExist) {
		result.outcome = "not-present"
		return result
	}
	if err != nil || !info.IsDir() {
		result.outcome = "incomplete"
		result.diagnostics = []auditDiagnostic{{code: "source_unreadable", target: ".agent", message: auditProofIncompleteMessages["source_unreadable"]}}
		return result
	}
	stdout, _, exit, err := runner.Run(ctx, []string{"verify-proof", "--integrity-only", "--json"}, nil)
	if err != nil {
		result.outcome = "incomplete"
		diagnostic := auditDiagnostic{code: "agentflow_unavailable", message: "agentflow proof verification unavailable"}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			diagnostic = auditDiagnostic{code: "canceled", message: auditProofIncompleteMessages["canceled"]}
		} else if errors.Is(err, exec.ErrNotFound) {
			diagnostic.message = "agentflow proof verification unavailable: agentflow executable not found"
		}
		result.diagnostics = []auditDiagnostic{diagnostic}
		return result
	}
	if exit == 2 {
		result.outcome = "incomplete"
		result.diagnostics = []auditDiagnostic{{
			code:    "agentflow_unavailable",
			message: "agentflow proof verification unavailable: requires agentflow with --integrity-only support",
		}}
		return result
	}
	var response auditProofResponse
	if err := decodeUniqueJSON(stdout, &response); err != nil || response.SchemaVersion == nil || *response.SchemaVersion != "v1" || response.Outcome == nil || response.Assurance == nil || *response.Assurance != auditProofAssurance || response.CheckedSteps == nil || *response.CheckedSteps < 0 || response.CheckedSources == nil || *response.CheckedSources < 0 || response.Violations == nil || response.IncompleteReasons == nil {
		return unusableAuditProofResult(result)
	}
	sourceChanged := false
	for _, finding := range response.IncompleteReasons {
		sourceChanged = sourceChanged || finding.Code != nil && *finding.Code == "source_changed"
	}
	var diagnostics []auditDiagnostic
	for _, finding := range response.Violations {
		if finding.Code == nil || finding.Target == nil || finding.Message == nil || !localAuditProofIdentifier(*finding.Target, false) {
			return unusableAuditProofResult(result)
		}
		message, ok := auditProofViolationMessages[*finding.Code]
		if !ok {
			return unusableAuditProofResult(result)
		}
		if !sourceChanged {
			diagnostics = append(diagnostics, auditDiagnostic{code: *finding.Code, target: *finding.Target, message: message})
		}
	}
	for _, finding := range response.IncompleteReasons {
		if finding.Code == nil || finding.Path == nil || finding.Message == nil || !localAuditProofIdentifier(*finding.Path, true) {
			return unusableAuditProofResult(result)
		}
		message, ok := auditProofIncompleteMessages[*finding.Code]
		if !ok {
			return unusableAuditProofResult(result)
		}
		diagnostics = append(diagnostics, auditDiagnostic{code: *finding.Code, target: *finding.Path, message: message})
	}
	consistent := false
	switch *response.Outcome {
	case "valid":
		consistent = exit == 0 && len(response.Violations) == 0 && len(response.IncompleteReasons) == 0
	case "violation":
		consistent = exit == 1 && len(response.Violations) > 0 && !sourceChanged
	case "incomplete":
		consistent = exit == 1 && len(response.IncompleteReasons) > 0 && (len(response.Violations) == 0 || sourceChanged)
	}
	if !consistent {
		return unusableAuditProofResult(result)
	}
	result.outcome = *response.Outcome
	result.checked = *response.CheckedSteps
	result.sources = *response.CheckedSources
	result.diagnostics = diagnostics
	return result
}

func decodeUniqueJSON(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return json.Unmarshal(data, dst)
}

// localAuditProofIdentifier checks both slash styles without changing the
// display-only identifier returned to the renderer.
func localAuditProofIdentifier(identifier string, allowEmpty bool) bool {
	if identifier == "" {
		return allowEmpty
	}
	normalized := strings.ReplaceAll(identifier, `\`, "/")
	if path.IsAbs(normalized) || len(normalized) >= 2 && normalized[1] == ':' && isASCIILetter(normalized[0]) {
		return false
	}
	cleaned := path.Clean(normalized)
	return cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func isASCIILetter(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			member, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := member.(string)
			if !ok {
				return errors.New("non-string JSON object member")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate JSON member %q", name)
			}
			seen[name] = struct{}{}
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func unusableAuditProofResult(result auditResult) auditResult {
	result.outcome = "incomplete"
	result.checked = 0
	result.sources = 0
	result.diagnostics = []auditDiagnostic{{
		code:    "agentflow_response_unusable",
		message: "agentflow proof verification returned an incompatible response",
	}}
	return result
}
