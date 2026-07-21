package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
	"github.com/modelcontextprotocol/go-sdk/auth"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpPolicyEvaluatorSpy struct {
	evaluateCalls  int
	resultCalls    int
	last           rag.RetrievalRequest
	decision       rag.RetrievalPolicyDecision
	evaluateErr    error
	resultDecision []rag.RetrievalResultDecision
	resultErr      error
}

func (s *mcpPolicyEvaluatorSpy) Evaluate(_ context.Context, req rag.RetrievalRequest) (rag.RetrievalPolicyDecision, error) {
	s.evaluateCalls++
	s.last = req
	return s.decision, s.evaluateErr
}

func (s *mcpPolicyEvaluatorSpy) EvaluateResults(_ context.Context, _ rag.RetrievalRequest, _ []rag.Chunk) ([]rag.RetrievalResultDecision, error) {
	s.resultCalls++
	return s.resultDecision, s.resultErr
}

type mcpPolicyObserverSpy struct {
	calls int
	last  rag.RetrievalPolicyEvent
	err   error
}

func (s *mcpPolicyObserverSpy) OnRetrievalPolicy(_ context.Context, event rag.RetrievalPolicyEvent) error {
	s.calls++
	s.last = event
	return s.err
}

type mcpPanicPolicyObserver struct{}

func (o *mcpPanicPolicyObserver) OnRetrievalPolicy(context.Context, rag.RetrievalPolicyEvent) error {
	if o == nil {
		panic("typed-nil observer invoked")
	}
	return nil
}

func TestRetrievalPolicyResultMetaIsContentSafe(t *testing.T) {
	outcome := rag.RetrievalPolicyOutcome{
		Applied: true, Disposition: rag.RetrievalPolicyAllowed, ReasonCode: "allowed",
		CandidateCount: 1, CandidateSourceCount: 2, ReturnedCount: 3, ReturnedSourceCount: 4,
		FilteredCount: 5, RedactedCount: 6, StaleDroppedCount: 7, AuditLabelCount: 8,
	}
	result := withRetrievalPolicyMeta(toolResult("CONTENT_SECRET"), outcome)
	data, err := json.Marshal(result.Meta)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		`"disposition":"allowed"`, `"reason_code":"allowed"`,
		`"candidate_count":1`, `"candidate_source_count":2`,
		`"returned_count":3`, `"returned_source_count":4`,
		`"filtered_count":5`, `"redacted_count":6`,
		`"stale_dropped_count":7`, `"audit_label_count":8`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metadata = %s, want %s", text, want)
		}
	}
	for _, forbidden := range []string{
		"QUERY_SECRET", "CONTENT_SECRET", "SOURCE_SECRET", "ID_SECRET",
		"PRINCIPAL_SECRET", "SESSION_SECRET", "LABEL_SECRET", "VALUE_SECRET", "ERROR_SECRET",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("metadata leaked %q: %s", forbidden, text)
		}
	}
	if got := withRetrievalPolicyMeta(toolResult("legacy"), rag.RetrievalPolicyOutcome{}); got.Meta != nil {
		t.Fatalf("unapplied metadata = %#v, want nil", got.Meta)
	}
}

func TestRetrievalPolicyErrorMappingIsFixedAndSafe(t *testing.T) {
	outcome := rag.RetrievalPolicyOutcome{
		Applied: true, Disposition: rag.RetrievalPolicyFailed, ReasonCode: "evaluator_failed",
		CandidateCount: 1, CandidateSourceCount: 2, ReturnedCount: 3, ReturnedSourceCount: 4,
		FilteredCount: 5, RedactedCount: 6, StaleDroppedCount: 7, AuditLabelCount: 8,
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"denied", fmt.Errorf("%w: ERROR_SECRET", rag.ErrPolicyDenied), "policy_denied: retrieval denied"},
		{"evaluator", fmt.Errorf("%w: ERROR_SECRET", rag.ErrPolicyEvaluatorFailed), "policy_evaluator_failed: retrieval policy evaluation failed"},
		{"decision", fmt.Errorf("%w: ERROR_SECRET", rag.ErrPolicyDecisionInvalid), "policy_decision_invalid: retrieval policy decision invalid"},
		{"freshness", fmt.Errorf("%w: ERROR_SECRET", rag.ErrFreshnessUnknown), "freshness_unknown: required retrieval freshness could not be verified"},
		{"unknown applied", errors.New("ERROR_SECRET"), "policy_failed: retrieval policy enforcement failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := retrievalPolicyToolError(outcome, tc.err)
			if result == nil || !result.IsError || !strings.HasPrefix(extractText(result), tc.want) {
				t.Fatalf("result = %#v text = %q, want prefix %q", result, extractText(result), tc.want)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "ERROR_SECRET") {
				t.Fatalf("result leaked raw error: %s", data)
			}
			if result.Meta == nil {
				t.Fatal("policy error omitted safe applied metadata")
			}
		})
	}

	identity := retrievalPolicyRequestError(fmt.Errorf("%w: ERROR_SECRET", errRetrievalPolicyIdentity))
	if identity == nil || !identity.IsError || !strings.HasPrefix(extractText(identity), "policy_identity_failed: retrieval identity resolution failed") {
		t.Fatalf("identity result = %#v text = %q", identity, extractText(identity))
	}
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ERROR_SECRET") || identity.Meta != nil {
		t.Fatalf("identity result leaked detail or policy metadata: %s", data)
	}
	if result := retrievalPolicyToolError(rag.RetrievalPolicyOutcome{}, errors.New("ordinary")); result != nil {
		t.Fatalf("unapplied ordinary error = %#v, want nil", result)
	}
	observerFailure := rag.RetrievalPolicyOutcome{Disposition: rag.RetrievalPolicyFailed, ReasonCode: "observer_failed"}
	result := retrievalPolicyToolError(observerFailure, errors.New("ERROR_SECRET"))
	if result == nil || extractText(result) != "policy_failed: retrieval policy enforcement failed" || result.Meta != nil {
		t.Fatalf("observer failure result = %#v text = %q", result, extractText(result))
	}
	if got := retrievalPolicyPromptError(observerFailure, errors.New("ERROR_SECRET")); got == nil || got.Error() != "policy_failed: retrieval policy enforcement failed" {
		t.Fatalf("observer prompt error = %v", got)
	}
}

func TestRetrievalPolicyIdentityPrecedence(t *testing.T) {
	policyRequest := func(t *testing.T, s *Server, extra *gomcp.RequestExtra) (rag.RetrievalPolicyRequest, bool) {
		t.Helper()
		req := rawArgs(t, `{"query":"q"}`)
		req.Params.Meta = gomcp.Meta{RetrievalPolicyMetaKey: map[string]any{
			"principal_id": "claimed-user", "session_id": "claimed-session",
		}}
		req.Extra = extra
		policy, present, err := s.retrievalPolicyRequest(context.Background(), req)
		if err != nil {
			t.Fatalf("retrievalPolicyRequest() error = %v", err)
		}
		return policy, present
	}

	t.Run("resolver overrides token and local claim", func(t *testing.T) {
		s := &Server{}
		WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) {
			return "resolver-user", nil
		})(s)
		policy, present := policyRequest(t, s, &gomcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: "token-user"}})
		if !present || policy.PrincipalID != "resolver-user" {
			t.Fatalf("policy/present = %#v/%v", policy, present)
		}
	})

	t.Run("empty resolver result remains authoritative", func(t *testing.T) {
		s := &Server{}
		WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) { return "", nil })(s)
		policy, _ := policyRequest(t, s, &gomcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: "token-user"}})
		if policy.PrincipalID != "" {
			t.Fatalf("principal = %q, want authoritative empty", policy.PrincipalID)
		}
	})

	t.Run("token overrides claim", func(t *testing.T) {
		policy, _ := policyRequest(t, &Server{}, &gomcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: "token-user"}})
		if policy.PrincipalID != "token-user" {
			t.Fatalf("principal = %q, want token-user", policy.PrincipalID)
		}
	})

	t.Run("local request accepts claims", func(t *testing.T) {
		policy, _ := policyRequest(t, &Server{}, nil)
		if policy.PrincipalID != "claimed-user" || policy.SessionID != "claimed-session" {
			t.Fatalf("policy = %#v", policy)
		}
	})

	t.Run("remote request without token strips claims", func(t *testing.T) {
		policy, _ := policyRequest(t, &Server{}, &gomcp.RequestExtra{})
		if policy.PrincipalID != "" || policy.SessionID != "" {
			t.Fatalf("policy = %#v, want stripped identity", policy)
		}
	})

	t.Run("empty authenticated user remains empty", func(t *testing.T) {
		policy, _ := policyRequest(t, &Server{}, &gomcp.RequestExtra{TokenInfo: &auth.TokenInfo{}})
		if policy.PrincipalID != "" {
			t.Fatalf("principal = %q, want authenticated empty", policy.PrincipalID)
		}
	})

	t.Run("resolver only without metadata remains legacy", func(t *testing.T) {
		calls := 0
		s := &Server{}
		WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) {
			calls++
			return "ambient-user", nil
		})(s)
		req := rawArgs(t, `{"query":"q"}`)
		req.Extra = &gomcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: "token-user"}}
		policy, present, err := s.retrievalPolicyRequest(context.Background(), req)
		if err != nil || present || calls != 0 || !reflect.DeepEqual(policy, rag.RetrievalPolicyRequest{}) {
			t.Fatalf("policy/present/error/resolver calls = %#v/%v/%v/%d", policy, present, err, calls)
		}
	})

	t.Run("observer only without metadata remains legacy", func(t *testing.T) {
		s := &Server{}
		WithRetrievalPolicyObserver(&mcpPolicyObserverSpy{})(s)
		req := rawArgs(t, `{"query":"q"}`)
		req.Extra = &gomcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: "token-user"}}
		policy, present, err := s.retrievalPolicyRequest(context.Background(), req)
		if err != nil || present || !reflect.DeepEqual(policy, rag.RetrievalPolicyRequest{}) {
			t.Fatalf("policy/present/error = %#v/%v/%v", policy, present, err)
		}
	})

	t.Run("typed nil observer remains legacy", func(t *testing.T) {
		var observer *mcpPanicPolicyObserver
		s := &Server{}
		WithRetrievalPolicyObserver(observer)(s)
		if s.retrievalPolicyObserver != nil {
			t.Fatalf("typed-nil observer retained as %#v", s.retrievalPolicyObserver)
		}
	})

	t.Run("typed nil evaluator without metadata remains legacy", func(t *testing.T) {
		var evaluator *mcpPolicyEvaluatorSpy
		calls := 0
		s := &Server{}
		WithRetrievalPolicyEvaluator(evaluator)(s)
		WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) {
			calls++
			return "ambient-user", nil
		})(s)
		req := rawArgs(t, `{"query":"q"}`)
		req.Extra = &gomcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: "token-user"}}
		policy, present, err := s.retrievalPolicyRequest(context.Background(), req)
		if err != nil || present || calls != 0 || s.retrievalPolicyEvaluator != nil || !reflect.DeepEqual(policy, rag.RetrievalPolicyRequest{}) {
			t.Fatalf("policy/present/error/resolver calls/evaluator = %#v/%v/%v/%d/%#v", policy, present, err, calls, s.retrievalPolicyEvaluator)
		}
	})
}

func TestRetrievalPolicyTransportSessionOverridesClaim(t *testing.T) {
	evaluator := &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true}}
	s := &Server{retrievalPolicyEvaluator: evaluator}
	s.mcpServer = gomcp.NewServer(&gomcp.Implementation{Name: "test", Version: "0"}, nil)
	s.mcpServer.AddTool(&gomcp.Tool{
		Name: "policy-session", InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		policy, _, err := s.retrievalPolicyRequest(ctx, req)
		if err != nil {
			return nil, err
		}
		_, err = evaluator.Evaluate(ctx, rag.RetrievalRequest{Query: "q", Policy: policy})
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "ok"}}}, err
	})

	httpServer := httptest.NewServer(streamableHTTPHandler(s))
	defer httpServer.Close()
	client := gomcp.NewClient(&gomcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &gomcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if session.ID() == "" {
		t.Fatal("streamable transport session ID is empty")
	}
	_, err = session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "policy-session",
		Meta: gomcp.Meta{RetrievalPolicyMetaKey: map[string]any{
			"principal_id": "claimed-user", "session_id": "claimed-session",
		}},
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := evaluator.last.Policy.SessionID; got != session.ID() || got == "claimed-session" {
		t.Fatalf("evaluator session = %q, want transport session %q", got, session.ID())
	}
}

func TestRetrievalPolicyIdentityFailureIsSanitized(t *testing.T) {
	cause := errors.New("IDENTITY_SECRET")
	evaluator := &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true}}
	observer := &mcpPolicyObserverSpy{}
	embedCalls := 0
	store := &answerStore{}
	retriever, err := rag.NewRetrieverWithEmbedder(
		rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
			embedCalls++
			return rag.EmbedResult{}, nil
		}),
		store,
		rag.WithRetrievalPolicyEvaluator(evaluator),
		rag.WithRetrievalPolicyObserver(observer),
	)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{retrievalPolicyEvaluator: evaluator, retrievalPolicyObserver: observer, retriever: retriever}
	WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) {
		return "", cause
	})(s)
	req := rawArgs(t, `{"query":"q"}`)
	req.Params.Meta = policyMetaFromJSON(t, `{"principal_id":"claimed-user","session_id":"claimed-session"}`)

	policy, present, err := s.retrievalPolicyRequest(context.Background(), req)
	if !present || !errors.Is(err, errRetrievalPolicyIdentity) || !errors.Is(err, cause) {
		t.Fatalf("policy/present/error = %#v/%v/%v", policy, present, err)
	}
	if !reflect.DeepEqual(policy, rag.RetrievalPolicyRequest{}) {
		t.Fatalf("failed identity policy = %#v, want zero", policy)
	}
	if evaluator.evaluateCalls != 0 || evaluator.resultCalls != 0 || embedCalls != 0 || store.searchCalls != 0 || observer.calls != 0 {
		t.Fatalf("work after identity failure = evaluator:%d/%d embed:%d store:%d observer:%d",
			evaluator.evaluateCalls, evaluator.resultCalls, embedCalls, store.searchCalls, observer.calls)
	}
}

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
