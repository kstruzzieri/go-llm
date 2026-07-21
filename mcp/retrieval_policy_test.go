package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDecodeRetrievalPolicyMetaAbsentAndValid(t *testing.T) {
	policy, present, err := decodeRetrievalPolicyMeta(nil)
	if err != nil || present || !reflect.DeepEqual(policy, rag.RetrievalPolicyRequest{}) {
		t.Fatalf("absent = %#v/%v/%v", policy, present, err)
	}
	policy, present, err = decodeRetrievalPolicyMeta(gomcp.Meta{"unrelated": map[string]any{"ignored": true}})
	if err != nil || present || !reflect.DeepEqual(policy, rag.RetrievalPolicyRequest{}) {
		t.Fatalf("unrelated = %#v/%v/%v", policy, present, err)
	}
	meta := policyMetaFromJSON(t, `{
		"principal_id":"local","session_id":"session",
		"scope":{"collection":" docs ","tags":[" public ","public"]},
		"require_fresh":true,"max_results":10,"max_cost":100,
		"audit_labels":{"purpose":"support"}
	}`)
	policy, present, err = decodeRetrievalPolicyMeta(meta)
	if err != nil || !present {
		t.Fatalf("valid = %#v/%v/%v", policy, present, err)
	}
	if policy.PrincipalID != "local" || policy.SessionID != "session" ||
		policy.Scope.Collection != " docs " || !slices.Equal(policy.Scope.Tags, []string{" public ", "public"}) ||
		!policy.RequireFresh || policy.MaxResults != 10 || policy.MaxCost != 100 || policy.AuditLabels["purpose"] != "support" {
		t.Fatalf("policy = %#v", policy)
	}
}

func TestDecodeRetrievalPolicyMetaRejectsInvalid(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	invalidPrincipal := mutatePolicyMeta(t, `{"principal_id":"valid"}`, func(value map[string]any) {
		value["principal_id"] = invalidUTF8
	})
	invalidSession := mutatePolicyMeta(t, `{"session_id":"valid"}`, func(value map[string]any) {
		value["session_id"] = invalidUTF8
	})
	invalidCollection := mutatePolicyMeta(t, `{"scope":{"collection":"valid"}}`, func(value map[string]any) {
		value["scope"].(map[string]any)["collection"] = invalidUTF8
	})
	invalidTag := mutatePolicyMeta(t, `{"scope":{"tags":["valid"]}}`, func(value map[string]any) {
		value["scope"].(map[string]any)["tags"].([]any)[0] = invalidUTF8
	})
	invalidAuditKey := mutatePolicyMeta(t, `{"audit_labels":{"valid":"value"}}`, func(value map[string]any) {
		labels := value["audit_labels"].(map[string]any)
		delete(labels, "valid")
		labels[invalidUTF8] = "value"
	})
	invalidAuditValue := mutatePolicyMeta(t, `{"audit_labels":{"key":"valid"}}`, func(value map[string]any) {
		value["audit_labels"].(map[string]any)["key"] = invalidUTF8
	})
	maxIntMeta := policyMetaFromJSON(t, fmt.Sprintf(`{"max_cost":%d}`, int64(math.MaxInt64)))
	maxIntValue := maxIntMeta[RetrievalPolicyMetaKey].(map[string]any)["max_cost"]
	if _, ok := maxIntValue.(float64); !ok {
		t.Fatalf("max_cost decoded as %T, want float64", maxIntValue)
	}

	tests := []struct {
		name      string
		meta      gomcp.Meta
		category  string
		forbidden string
	}{
		{name: "null", meta: policyMetaFromJSON(t, `null`), category: "object"},
		{name: "null principal_id", meta: policyMetaFromJSON(t, `{"principal_id":null}`), category: "null"},
		{name: "null session_id", meta: policyMetaFromJSON(t, `{"session_id":null}`), category: "null"},
		{name: "null scope", meta: policyMetaFromJSON(t, `{"scope":null}`), category: "null"},
		{name: "null require_fresh", meta: policyMetaFromJSON(t, `{"require_fresh":null}`), category: "null"},
		{name: "null max_results", meta: policyMetaFromJSON(t, `{"max_results":null}`), category: "null"},
		{name: "null max_cost", meta: policyMetaFromJSON(t, `{"max_cost":null}`), category: "null"},
		{name: "null audit_labels", meta: policyMetaFromJSON(t, `{"audit_labels":null}`), category: "null"},
		{name: "null scope collection", meta: policyMetaFromJSON(t, `{"scope":{"collection":null}}`), category: "null"},
		{name: "null scope tags", meta: policyMetaFromJSON(t, `{"scope":{"tags":null}}`), category: "null"},
		{name: "null tag element", meta: policyMetaFromJSON(t, `{"scope":{"tags":[null]}}`), category: "null"},
		{name: "null audit label value", meta: policyMetaFromJSON(t, `{"audit_labels":{"purpose":null}}`), category: "null"},
		{name: "non-object", meta: policyMetaFromJSON(t, `"claim"`), category: "decode", forbidden: "claim"},
		{name: "unknown top-level", meta: policyMetaFromJSON(t, `{"unknown":true}`), category: "unknown"},
		{name: "unknown nested scope", meta: policyMetaFromJSON(t, `{"scope":{"unknown":true}}`), category: "unknown"},
		{name: "canonical size", meta: canonicalSizedPolicyMeta(t, maxRetrievalPolicyMetaBytes+1), category: "metadata"},
		{name: "principal invalid UTF-8", meta: invalidPrincipal, category: "UTF-8"},
		{name: "session invalid UTF-8", meta: invalidSession, category: "UTF-8"},
		{name: "principal size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"principal_id":%q}`, strings.Repeat("p", maxRetrievalIdentityBytes+1))), category: "principal_id"},
		{name: "session size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"session_id":%q}`, strings.Repeat("s", maxRetrievalIdentityBytes+1))), category: "session_id"},
		{name: "collection size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"scope":{"collection":%q}}`, strings.Repeat("c", rag.MaxManagedMetadataBytes+1))), category: "collection"},
		{name: "collection invalid UTF-8", meta: invalidCollection, category: "UTF-8"},
		{name: "tag count", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"scope":{"tags":%s}}`, jsonStrings(t, slices.Repeat([]string{"tag"}, rag.MaxManagedTags+1)))), category: "tags"},
		{name: "tag size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"scope":{"tags":[%q]}}`, strings.Repeat("t", rag.MaxManagedTagBytes+1))), category: "tag"},
		{name: "tag invalid UTF-8", meta: invalidTag, category: "UTF-8"},
		{name: "audit count", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"audit_labels":%s}`, jsonAuditLabels(t, maxRetrievalAuditLabels+1, 1))), category: "audit_labels"},
		{name: "audit invalid key UTF-8", meta: invalidAuditKey, category: "UTF-8"},
		{name: "audit invalid value UTF-8", meta: invalidAuditValue, category: "UTF-8"},
		{name: "audit key size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"audit_labels":{%q:"value"}}`, strings.Repeat("k", maxRetrievalAuditBytes+1))), category: "audit label"},
		{name: "audit value size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"audit_labels":{"key":%q}}`, strings.Repeat("v", maxRetrievalAuditBytes+1))), category: "audit label"},
		{name: "negative max_results", meta: policyMetaFromJSON(t, `{"max_results":-1}`), category: "max_results"},
		{name: "fractional max_results", meta: policyMetaFromJSON(t, `{"max_results":1.5}`), category: "decode", forbidden: "1.5"},
		{name: "max_results over limit", meta: policyMetaFromJSON(t, `{"max_results":101}`), category: "max_results"},
		{name: "negative max_cost", meta: policyMetaFromJSON(t, `{"max_cost":-1}`), category: "max_cost"},
		{name: "fractional max_cost", meta: policyMetaFromJSON(t, `{"max_cost":1.5}`), category: "decode", forbidden: "1.5"},
		{name: "max_cost above wire ceiling", meta: policyMetaFromJSON(t, `{"max_cost":9007199254740994}`), category: "max_cost", forbidden: "9007199254740994"},
		{name: "max_cost precision-lost int64 overflow", meta: maxIntMeta, category: "decode", forbidden: "9223372036854776000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, present, err := decodeRetrievalPolicyMeta(tc.meta)
			if !present || err == nil || !reflect.DeepEqual(policy, rag.RetrievalPolicyRequest{}) {
				t.Fatalf("decode = %#v/%v/%v, want zero/true/error", policy, present, err)
			}
			if !strings.Contains(err.Error(), tc.category) {
				t.Fatalf("error = %q, want category %q", err, tc.category)
			}
			if tc.forbidden != "" && strings.Contains(err.Error(), tc.forbidden) {
				t.Fatalf("error = %q, leaked rejected value %q", err, tc.forbidden)
			}
		})
	}
}

func TestDecodeRetrievalPolicyMetaAcceptsBoundaries(t *testing.T) {
	maxCost := policyMetaFromJSON(t, `{"max_cost":9007199254740992}`)
	if _, ok := maxCost[RetrievalPolicyMetaKey].(map[string]any)["max_cost"].(float64); !ok {
		t.Fatal("max_cost did not take the SDK float64 wire path")
	}

	tests := []struct {
		name string
		meta gomcp.Meta
	}{
		{name: "canonical size", meta: canonicalSizedPolicyMeta(t, maxRetrievalPolicyMetaBytes)},
		{name: "identity size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"principal_id":%q,"session_id":%q}`, strings.Repeat("p", maxRetrievalIdentityBytes), strings.Repeat("s", maxRetrievalIdentityBytes)))},
		{name: "collection size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"scope":{"collection":%q}}`, strings.Repeat("c", rag.MaxManagedMetadataBytes)))},
		{name: "tag count", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"scope":{"tags":%s}}`, jsonStrings(t, slices.Repeat([]string{"tag"}, rag.MaxManagedTags))))},
		{name: "tag size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"scope":{"tags":[%q]}}`, strings.Repeat("t", rag.MaxManagedTagBytes)))},
		{name: "audit count", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"audit_labels":%s}`, jsonAuditLabels(t, maxRetrievalAuditLabels, 1)))},
		{name: "audit key and value size", meta: policyMetaFromJSON(t, fmt.Sprintf(`{"audit_labels":{%q:%q}}`, strings.Repeat("k", maxRetrievalAuditBytes), strings.Repeat("v", maxRetrievalAuditBytes)))},
		{name: "max results", meta: policyMetaFromJSON(t, `{"max_results":100}`)},
		{name: "max cost wire ceiling", meta: maxCost},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, present, err := decodeRetrievalPolicyMeta(tc.meta)
			if !present || err != nil {
				t.Fatalf("decode = %#v/%v/%v, want policy/true/nil", policy, present, err)
			}
		})
	}
}

func TestDecodeRetrievalPolicyMetaOwnsMutableValues(t *testing.T) {
	meta := policyMetaFromJSON(t, `{"scope":{"tags":["tag"]},"audit_labels":{"purpose":"support"}}`)
	policy, present, err := decodeRetrievalPolicyMeta(meta)
	if err != nil || !present {
		t.Fatalf("decode = %#v/%v/%v", policy, present, err)
	}
	value := meta[RetrievalPolicyMetaKey].(map[string]any)
	value["scope"].(map[string]any)["tags"].([]any)[0] = "input-mutated"
	value["audit_labels"].(map[string]any)["purpose"] = "input-mutated"
	if policy.Scope.Tags[0] != "tag" || policy.AuditLabels["purpose"] != "support" {
		t.Fatalf("policy aliases meta input: %#v", policy)
	}

	wireTags := managedTags{"wire-tag"}
	wireAudit := map[string]string{"purpose": "wire-value"}
	policy, err = validateRetrievalPolicyWire(retrievalPolicyWire{
		Scope: retrievalPolicyScopeWire{Tags: wireTags}, AuditLabels: wireAudit,
	})
	if err != nil {
		t.Fatal(err)
	}
	wireTags[0], wireAudit["purpose"] = "wire-mutated", "wire-mutated"
	if policy.Scope.Tags[0] != "wire-tag" || policy.AuditLabels["purpose"] != "wire-value" {
		t.Fatalf("policy aliases wire input: %#v", policy)
	}
	policy.Scope.Tags[0], policy.AuditLabels["purpose"] = "policy-mutated", "policy-mutated"
	if wireTags[0] != "wire-mutated" || wireAudit["purpose"] != "wire-mutated" {
		t.Fatalf("wire input aliases returned policy: %#v/%#v", wireTags, wireAudit)
	}
}

func policyMetaFromJSON(t *testing.T, raw string) gomcp.Meta {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("unmarshal policy metadata: %v", err)
	}
	return gomcp.Meta{RetrievalPolicyMetaKey: value}
}

func mutatePolicyMeta(t *testing.T, raw string, mutate func(map[string]any)) gomcp.Meta {
	t.Helper()
	meta := policyMetaFromJSON(t, raw)
	mutate(meta[RetrievalPolicyMetaKey].(map[string]any))
	return meta
}

func jsonStrings(t *testing.T, values []string) string {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func jsonAuditLabels(t *testing.T, count, valueBytes int) string {
	t.Helper()
	labels := make(map[string]string, count)
	for i := range count {
		labels[fmt.Sprintf("label-%02d", i)] = strings.Repeat("v", valueBytes)
	}
	data, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func canonicalSizedPolicyMeta(t *testing.T, size int) gomcp.Meta {
	t.Helper()
	labels := make(map[string]string, 16)
	for i := range 16 {
		labels[fmt.Sprintf("k%02d", i)] = ""
	}
	wire := map[string]any{
		"principal_id": strings.Repeat("p", maxRetrievalIdentityBytes),
		"session_id":   strings.Repeat("s", maxRetrievalIdentityBytes),
		"scope": map[string]any{
			"collection": strings.Repeat("c", rag.MaxManagedMetadataBytes),
		},
		"audit_labels": labels,
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	remaining := size - len(data)
	for i := 0; i < 16 && remaining > 0; i++ {
		padding := min(remaining, maxRetrievalAuditBytes)
		labels[fmt.Sprintf("k%02d", i)] = strings.Repeat("v", padding)
		remaining -= padding
	}
	data, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 0 || len(data) != size {
		t.Fatalf("canonical metadata size = %d, want %d", len(data), size)
	}
	return policyMetaFromJSON(t, string(data))
}
