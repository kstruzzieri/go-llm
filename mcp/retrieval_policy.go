package mcp

import (
	"bytes"
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
	if bytes.Equal(data, []byte("null")) {
		return rag.RetrievalPolicyRequest{}, true, fmt.Errorf("policy metadata must be an object")
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
