package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/rag"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// RetrievalPolicyMetaKey is the MCP _meta key for an optional retrieval policy request.
const RetrievalPolicyMetaKey = "go-llm/retrieval-policy"

const (
	maxRetrievalPolicyMetaBytes = 16 << 10
	maxRetrievalIdentityBytes   = 4 << 10
	maxRetrievalAuditLabels     = 64
	maxRetrievalAuditBytes      = 256
	maxRetrievalWireCost        = int64(1 << 53)
)

var errRetrievalPolicyIdentity = errors.New("mcp: retrieval policy identity resolution failed")

type retrievalPolicyResultWire struct {
	Disposition          rag.RetrievalPolicyDisposition `json:"disposition"`
	ReasonCode           string                         `json:"reason_code"`
	CandidateCount       int                            `json:"candidate_count"`
	CandidateSourceCount int                            `json:"candidate_source_count"`
	ReturnedCount        int                            `json:"returned_count"`
	ReturnedSourceCount  int                            `json:"returned_source_count"`
	FilteredCount        int                            `json:"filtered_count"`
	RedactedCount        int                            `json:"redacted_count"`
	StaleDroppedCount    int                            `json:"stale_dropped_count"`
	AuditLabelCount      int                            `json:"audit_label_count"`
}

func retrievalPolicyMeta(outcome rag.RetrievalPolicyOutcome) gomcp.Meta {
	if !outcome.Applied {
		return nil
	}
	return gomcp.Meta{RetrievalPolicyMetaKey: retrievalPolicyResultWire{
		Disposition: outcome.Disposition, ReasonCode: outcome.ReasonCode,
		CandidateCount: outcome.CandidateCount, CandidateSourceCount: outcome.CandidateSourceCount,
		ReturnedCount: outcome.ReturnedCount, ReturnedSourceCount: outcome.ReturnedSourceCount,
		FilteredCount: outcome.FilteredCount, RedactedCount: outcome.RedactedCount,
		StaleDroppedCount: outcome.StaleDroppedCount, AuditLabelCount: outcome.AuditLabelCount,
	}}
}

func withRetrievalPolicyMeta(result *gomcp.CallToolResult, outcome rag.RetrievalPolicyOutcome) *gomcp.CallToolResult {
	result.Meta = retrievalPolicyMeta(outcome)
	return result
}

func retrievalPolicyToolError(outcome rag.RetrievalPolicyOutcome, err error) *gomcp.CallToolResult {
	code, message, ok := retrievalPolicyError(outcome, err)
	if !ok {
		return nil
	}
	return withRetrievalPolicyMeta(toolError(code, "%s", message), outcome)
}

func retrievalPolicyError(outcome rag.RetrievalPolicyOutcome, err error) (string, string, bool) {
	switch outcome.ReasonCode {
	case "denied":
		return "policy_denied", "retrieval denied", true
	case "evaluator_failed":
		return "policy_evaluator_failed", "retrieval policy evaluation failed", true
	case "request_invalid", "decision_invalid":
		return "policy_decision_invalid", "retrieval policy decision invalid", true
	case "freshness_unknown":
		return "freshness_unknown", "required retrieval freshness could not be verified", true
	case "observer_failed":
		return "policy_failed", "retrieval policy enforcement failed", true
	}
	if outcome.Applied {
		return "policy_failed", "retrieval policy enforcement failed", true
	}

	switch {
	case errors.Is(err, rag.ErrPolicyDenied):
		return "policy_denied", "retrieval denied", true
	case errors.Is(err, rag.ErrPolicyEvaluatorFailed):
		return "policy_evaluator_failed", "retrieval policy evaluation failed", true
	case errors.Is(err, rag.ErrPolicyDecisionInvalid):
		return "policy_decision_invalid", "retrieval policy decision invalid", true
	case errors.Is(err, rag.ErrFreshnessUnknown):
		return "freshness_unknown", "required retrieval freshness could not be verified", true
	default:
		return "", "", false
	}
}

func retrievalIdentityToolError() *gomcp.CallToolResult {
	return toolError("policy_identity_failed", "retrieval identity resolution failed")
}

func retrievalPolicyRequestError(err error) *gomcp.CallToolResult {
	if errors.Is(err, errRetrievalPolicyIdentity) {
		return retrievalIdentityToolError()
	}
	return toolError("validation", "invalid retrieval policy metadata")
}

func (s *Server) retrievalPolicyRequest(ctx context.Context, req gomcp.Request) (rag.RetrievalPolicyRequest, bool, error) {
	policy, present, err := decodeRetrievalPolicyMeta(gomcp.Meta(req.GetParams().GetMeta()))
	if err != nil {
		return rag.RetrievalPolicyRequest{}, present, err
	}
	if !present && isNilRetrievalPolicyValue(s.retrievalPolicyEvaluator) {
		return policy, false, nil
	}

	extra := req.GetExtra()
	if s.retrievalPrincipalResolver != nil {
		principal, err := s.retrievalPrincipalResolver(ctx, req)
		if err != nil {
			return rag.RetrievalPolicyRequest{}, present, fmt.Errorf("%w: %w", errRetrievalPolicyIdentity, err)
		}
		policy.PrincipalID = principal
	} else if extra != nil && extra.TokenInfo != nil {
		policy.PrincipalID = extra.TokenInfo.UserID
	} else if extra != nil {
		policy.PrincipalID = ""
	}
	session, _ := req.GetSession().(*gomcp.ServerSession)
	if session != nil && session.ID() != "" {
		policy.SessionID = session.ID()
	} else if extra != nil {
		policy.SessionID = ""
	}
	return policy, present, nil
}

type retrievalPolicyScopeWire struct {
	Collection string      `json:"collection,omitempty"`
	Tags       managedTags `json:"tags,omitempty"`
}

type retrievalPolicyWire struct {
	PrincipalID  string                   `json:"principal_id,omitempty"`
	SessionID    string                   `json:"session_id,omitempty"`
	Scope        retrievalPolicyScopeWire `json:"scope,omitempty"`
	RequireFresh bool                     `json:"require_fresh,omitempty"`
	MaxResults   int                      `json:"max_results,omitempty"`
	MaxCost      int64                    `json:"max_cost,omitempty"`
	AuditLabels  map[string]string        `json:"audit_labels,omitempty"`
}

// decodeRetrievalPolicyMeta strictly decodes a bounded policy request. MCP _meta
// numbers arrive as float64, so max_cost's practical wire ceiling is exactly 2^53.
func decodeRetrievalPolicyMeta(meta gomcp.Meta) (rag.RetrievalPolicyRequest, bool, error) {
	value, present := meta[RetrievalPolicyMetaKey]
	if !present {
		return rag.RetrievalPolicyRequest{}, false, nil
	}
	if value == nil {
		return rag.RetrievalPolicyRequest{}, true, fmt.Errorf("policy metadata must be an object")
	}
	if !retrievalPolicyMetaUTF8Valid(value) {
		return rag.RetrievalPolicyRequest{}, true, fmt.Errorf("policy metadata contains invalid UTF-8")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return rag.RetrievalPolicyRequest{}, true, fmt.Errorf("encode policy metadata: unsupported value")
	}
	if len(data) > maxRetrievalPolicyMetaBytes {
		return rag.RetrievalPolicyRequest{}, true, fmt.Errorf("policy metadata exceeds %d-byte limit", maxRetrievalPolicyMetaBytes)
	}
	var canonical any
	if err := json.Unmarshal(data, &canonical); err != nil {
		return rag.RetrievalPolicyRequest{}, true, fmt.Errorf("decode policy metadata: invalid JSON")
	}
	if retrievalPolicyMetaContainsNull(canonical) {
		return rag.RetrievalPolicyRequest{}, true, fmt.Errorf("policy metadata must not contain null")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire retrievalPolicyWire
	if err := decoder.Decode(&wire); err != nil {
		return rag.RetrievalPolicyRequest{}, true, retrievalPolicyDecodeError(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return rag.RetrievalPolicyRequest{}, true, fmt.Errorf("decode policy metadata: trailing value")
	}
	policy, err := validateRetrievalPolicyWire(wire)
	return policy, true, err
}

func validateRetrievalPolicyWire(wire retrievalPolicyWire) (rag.RetrievalPolicyRequest, error) {
	for _, item := range []struct {
		name  string
		value string
		limit int
	}{
		{"principal_id", wire.PrincipalID, maxRetrievalIdentityBytes},
		{"session_id", wire.SessionID, maxRetrievalIdentityBytes},
	} {
		if !utf8.ValidString(item.value) || len(item.value) > item.limit {
			return rag.RetrievalPolicyRequest{}, fmt.Errorf("%s exceeds %d-byte limit or is not valid UTF-8", item.name, item.limit)
		}
	}
	if wire.MaxResults < 0 || wire.MaxResults > maxRAGTopK {
		return rag.RetrievalPolicyRequest{}, fmt.Errorf("max_results must be between 0 and %d", maxRAGTopK)
	}
	if wire.MaxCost < 0 || wire.MaxCost > maxRetrievalWireCost {
		return rag.RetrievalPolicyRequest{}, fmt.Errorf("max_cost must be between 0 and %d", maxRetrievalWireCost)
	}
	if err := validateRAGSearchScope(wire.Scope.Collection, wire.Scope.Tags); err != nil {
		return rag.RetrievalPolicyRequest{}, err
	}
	if len(wire.AuditLabels) > maxRetrievalAuditLabels {
		return rag.RetrievalPolicyRequest{}, fmt.Errorf("audit_labels exceed %d-entry limit", maxRetrievalAuditLabels)
	}
	for key, value := range wire.AuditLabels {
		if !utf8.ValidString(key) || !utf8.ValidString(value) || len(key) > maxRetrievalAuditBytes || len(value) > maxRetrievalAuditBytes {
			return rag.RetrievalPolicyRequest{}, fmt.Errorf("audit label exceeds %d-byte limit or is not valid UTF-8", maxRetrievalAuditBytes)
		}
	}
	return rag.RetrievalPolicyRequest{
		PrincipalID: wire.PrincipalID, SessionID: wire.SessionID,
		Scope:        rag.RetrievalScope{Collection: wire.Scope.Collection, Tags: append([]string(nil), wire.Scope.Tags...)},
		RequireFresh: wire.RequireFresh, MaxResults: wire.MaxResults, MaxCost: wire.MaxCost,
		AuditLabels: maps.Clone(wire.AuditLabels),
	}, nil
}

func retrievalPolicyMetaUTF8Valid(value any) bool {
	switch value := value.(type) {
	case string:
		return utf8.ValidString(value)
	case []any:
		for _, item := range value {
			if !retrievalPolicyMetaUTF8Valid(item) {
				return false
			}
		}
	case []string:
		for _, item := range value {
			if !utf8.ValidString(item) {
				return false
			}
		}
	case managedTags:
		for _, item := range value {
			if !utf8.ValidString(item) {
				return false
			}
		}
	case map[string]any:
		for key, item := range value {
			if !utf8.ValidString(key) || !retrievalPolicyMetaUTF8Valid(item) {
				return false
			}
		}
	case map[string]string:
		for key, item := range value {
			if !utf8.ValidString(key) || !utf8.ValidString(item) {
				return false
			}
		}
	}
	return true
}

func retrievalPolicyMetaContainsNull(value any) bool {
	if value == nil {
		return true
	}
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			if retrievalPolicyMetaContainsNull(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range value {
			if retrievalPolicyMetaContainsNull(item) {
				return true
			}
		}
	}
	return false
}

func retrievalPolicyDecodeError(err error) error {
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) {
		if typeError.Field != "" {
			return fmt.Errorf("decode policy metadata field %s: invalid type or numeric range", typeError.Field)
		}
		return fmt.Errorf("decode policy metadata: invalid type or numeric range")
	}
	if strings.Contains(err.Error(), "unknown field") {
		return fmt.Errorf("decode policy metadata: unknown field")
	}
	if strings.Contains(err.Error(), "tag") {
		return fmt.Errorf("decode policy metadata tags: invalid")
	}
	return fmt.Errorf("decode policy metadata: invalid field value")
}
