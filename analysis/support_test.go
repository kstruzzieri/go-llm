package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// recordingChat is a fake ChatFunc that returns queued replies in call order
// and records each call's use-case and request. replies[0] answers the first
// call (extract), replies[1] the second (verify).
type recordingChat struct {
	replies []string
	err     error
	calls   []recordedCall
}

type recordedCall struct {
	useCase string
	req     provider.ChatRequest
}

func (rc *recordingChat) fn() ChatFunc {
	return func(_ context.Context, useCase string, req provider.ChatRequest) (*provider.ChatResponse, error) {
		rc.calls = append(rc.calls, recordedCall{useCase: useCase, req: req})
		if rc.err != nil {
			return nil, rc.err
		}
		i := len(rc.calls) - 1
		content := ""
		if i < len(rc.replies) {
			content = rc.replies[i]
		}
		return &provider.ChatResponse{Content: content, Done: true}, nil
	}
}

func TestNewSupportJudge_NilChat(t *testing.T) {
	if _, err := NewSupportJudgeWithChat(nil, ""); err == nil {
		t.Fatal("expected error for nil chat")
	}
}

func TestNewSupportJudge_OK(t *testing.T) {
	rc := &recordingChat{}
	j, err := NewSupportJudgeWithChat(rc.fn(), "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if j == nil {
		t.Fatal("expected non-nil judge")
	}
}

func TestSupportReport_JSONShape(t *testing.T) {
	r := SupportReport{
		Status: StatusPartial,
		Claims: []ClaimSupport{{
			ID: "C1", Claim: "x", Status: StatusSupported,
			EvidenceIDs: []string{"E1"}, Contradicted: false, Reason: "ok",
		}},
		Evidence:               []EvidenceRef{{ID: "E1", ChunkID: "h", Source: "a.go", StartLine: 1, EndLine: 2}},
		MissingEvidence:        []string{"gap"},
		MissingEvidenceQueries: []string{"q"},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"status"`, `"claims"`, `"evidence"`, `"missing_evidence"`,
		`"missing_evidence_queries"`, `"evidence_ids"`, `"chunk_id"`,
		`"start_line"`, `"end_line"`, `"contradicted"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshaled report missing %s: %s", want, data)
		}
	}
}

func TestErrSupportVerifyMalformed_Sentinel(t *testing.T) {
	wrapped := fmt.Errorf("wrapped: %w", ErrSupportVerifyMalformed)
	if !errors.Is(wrapped, ErrSupportVerifyMalformed) {
		t.Fatal("sentinel must match through wrapping via errors.Is")
	}
}

func TestLastJSONObjectWith(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		key   string
		want  string // "" means nil
	}{
		{"plain", `{"claims":["a"]}`, "claims", `{"claims":["a"]}`},
		{"fenced", "```json\n{\"claims\":[\"a\"]}\n```", "claims", `{"claims":["a"]}`},
		{"prefatory then real", `here is an example {"foo":1} and the answer {"claims":["a","b"]}`, "claims", `{"claims":["a","b"]}`},
		{"trailing prose", `{"claims":["a"]} hope that helps`, "claims", `{"claims":["a"]}`},
		{"brace in string", `{"claims":["uses {x} syntax"]}`, "claims", `{"claims":["uses {x} syntax"]}`},
		{"missing key", `{"foo":1}`, "claims", ""},
		{"no json", `sorry, no json here`, "claims", ""},
		{"last wins", `{"claims":["old"]} {"claims":["new"]}`, "claims", `{"claims":["new"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lastJSONObjectWith(tt.reply, tt.key)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("got %q, want nil", got)
				}
				return
			}
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractClaims_WellFormed(t *testing.T) {
	rc := &recordingChat{replies: []string{`{"claims":["the sky is blue","water is wet"]}`}}
	j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
	claims, err := j.extractClaims(context.Background(), "answer text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("got %d claims, want 2", len(claims))
	}
	if claims[0].ID != "C1" || claims[1].ID != "C2" {
		t.Errorf("labels = %q,%q want C1,C2", claims[0].ID, claims[1].ID)
	}
	if claims[0].Claim != "the sky is blue" {
		t.Errorf("claim[0] = %q", claims[0].Claim)
	}
	if claims[0].Status != StatusUnsupported {
		t.Errorf("default status = %q, want unsupported (fail closed)", claims[0].Status)
	}
	if len(rc.calls) != 1 || rc.calls[0].useCase != config.UseCaseExtract {
		t.Fatalf("expected one call with useCase %q, got %+v", config.UseCaseExtract, rc.calls)
	}
}

func TestExtractClaims_FallbackWholeAnswer(t *testing.T) {
	for _, reply := range []string{`not json at all`, `{"claims":[]}`, `{"claims":["   "]}`} {
		rc := &recordingChat{replies: []string{reply}}
		j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
		claims, err := j.extractClaims(context.Background(), "the whole answer")
		if err != nil {
			t.Fatalf("reply %q: unexpected error: %v", reply, err)
		}
		if len(claims) != 1 || claims[0].Claim != "the whole answer" || claims[0].ID != "C1" {
			t.Fatalf("reply %q: want single whole-answer claim, got %+v", reply, claims)
		}
	}
}

func TestExtractClaims_ChatError(t *testing.T) {
	rc := &recordingChat{err: errors.New("boom")}
	j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
	if _, err := j.extractClaims(context.Background(), "x"); err == nil {
		t.Fatal("expected error propagated from chat")
	}
}

func ev(id, source, content string, start, end int) rag.SearchResult {
	return rag.SearchResult{Chunk: rag.Chunk{ID: id, Source: source, Content: content, StartLine: start, EndLine: end}}
}

func TestBuildEvidenceBlocks_LabelsAndRefs(t *testing.T) {
	evidence := []rag.SearchResult{
		ev("h1", "a.go", "alpha", 1, 2),
		ev("h2", "b.go", "beta", 3, 4),
	}
	text, refs := buildEvidenceBlocks(evidence, 0)
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2", len(refs))
	}
	if refs[0].ID != "E1" || refs[0].ChunkID != "h1" || refs[0].Source != "a.go" || refs[0].StartLine != 1 || refs[0].EndLine != 2 {
		t.Errorf("ref[0] = %+v", refs[0])
	}
	if refs[1].ID != "E2" {
		t.Errorf("ref[1].ID = %q, want E2", refs[1].ID)
	}
	if !strings.Contains(text, "E1: a.go (lines 1-2)") || !strings.Contains(text, "alpha") {
		t.Errorf("text missing E1 block: %q", text)
	}
}

func TestBuildEvidenceBlocks_BudgetDropsTail(t *testing.T) {
	evidence := []rag.SearchResult{
		ev("h1", "a.go", strings.Repeat("x", 40), 1, 1),
		ev("h2", "b.go", strings.Repeat("y", 40), 2, 2),
		ev("h3", "c.go", strings.Repeat("z", 40), 3, 3),
	}
	_, refs := buildEvidenceBlocks(evidence, 60) // only the first block fits under 60
	if len(refs) != 1 || refs[0].ID != "E1" {
		t.Fatalf("budget should keep only E1, got %d refs", len(refs))
	}
}

func TestBuildEvidenceBlocks_KeepsFirstEvenIfOversized(t *testing.T) {
	evidence := []rag.SearchResult{ev("h1", "a.go", strings.Repeat("x", 500), 1, 1)}
	text, refs := buildEvidenceBlocks(evidence, 10) // first block alone exceeds budget
	if len(refs) != 1 || refs[0].ID != "E1" {
		t.Fatalf("first block must always be kept, got %d refs", len(refs))
	}
	if !strings.Contains(text, "E1:") {
		t.Errorf("expected E1 block present: %q", text)
	}
}

func claimsFixture() []ClaimSupport {
	return []ClaimSupport{
		{ID: "C1", Claim: "alpha", Status: StatusUnsupported},
		{ID: "C2", Claim: "beta", Status: StatusUnsupported},
	}
}

func refsFixture() []EvidenceRef {
	return []EvidenceRef{{ID: "E1", ChunkID: "h1", Source: "a.go"}}
}

func TestVerifyClaims_MapsByClaimID(t *testing.T) {
	reply := `{"verdicts":[
		{"claim_id":"C2","status":"supported","evidence_ids":["E1"],"contradicted":false,"reason":"ok","missing_query":""},
		{"claim_id":"C1","status":"unsupported","evidence_ids":[],"contradicted":false,"reason":"not found","missing_query":"find alpha"}
	]}`
	rc := &recordingChat{replies: []string{reply}}
	j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
	claims, missing, queries, err := j.verifyClaims(context.Background(), claimsFixture(), "E1: a.go\n...", refsFixture())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// C1 stays text "alpha" even though verdict order differs; mapping is by id.
	if claims[0].ID != "C1" || claims[0].Claim != "alpha" || claims[0].Status != StatusUnsupported {
		t.Errorf("C1 = %+v", claims[0])
	}
	if claims[1].ID != "C2" || claims[1].Status != StatusSupported || len(claims[1].EvidenceIDs) != 1 {
		t.Errorf("C2 = %+v", claims[1])
	}
	if rc.calls[0].useCase != config.UseCaseVerify {
		t.Errorf("useCase = %q, want %q", rc.calls[0].useCase, config.UseCaseVerify)
	}
	if len(missing) == 0 {
		t.Error("expected missing evidence reasons for unsupported C1")
	}
	if len(queries) != 1 || queries[0] != "find alpha" {
		t.Errorf("queries = %v, want [find alpha]", queries)
	}
}

func TestVerifyClaims_MissingVerdictFailsClosed(t *testing.T) {
	reply := `{"verdicts":[{"claim_id":"C1","status":"supported","evidence_ids":["E1"]}]}`
	rc := &recordingChat{replies: []string{reply}}
	j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
	claims, _, _, err := j.verifyClaims(context.Background(), claimsFixture(), "blocks", refsFixture())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims[1].Status != StatusUnsupported || claims[1].Reason == "" {
		t.Errorf("C2 with no verdict should be unsupported with a reason: %+v", claims[1])
	}
}

func TestVerifyClaims_UnknownStatusFailsClosed(t *testing.T) {
	reply := `{"verdicts":[{"claim_id":"C1","status":"maybe","evidence_ids":["E1"]},{"claim_id":"C2","status":"supported","evidence_ids":["E1"]}]}`
	rc := &recordingChat{replies: []string{reply}}
	j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
	claims, _, _, _ := j.verifyClaims(context.Background(), claimsFixture(), "blocks", refsFixture())
	if claims[0].Status != StatusUnsupported {
		t.Errorf("unknown status should fail closed to unsupported, got %q", claims[0].Status)
	}
}

func TestVerifyClaims_DowngradeSupportedWithoutEvidence(t *testing.T) {
	// supported/partial with no valid evidence id -> downgrade to unsupported.
	reply := `{"verdicts":[{"claim_id":"C1","status":"supported","evidence_ids":["E9"]},{"claim_id":"C2","status":"partial","evidence_ids":[]}]}`
	rc := &recordingChat{replies: []string{reply}}
	j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
	claims, _, _, _ := j.verifyClaims(context.Background(), claimsFixture(), "blocks", refsFixture())
	if claims[0].Status != StatusUnsupported || len(claims[0].EvidenceIDs) != 0 {
		t.Errorf("C1 supported with only invalid E9 should downgrade: %+v", claims[0])
	}
	if claims[1].Status != StatusUnsupported {
		t.Errorf("C2 partial with no evidence should downgrade: %+v", claims[1])
	}
}

func TestVerifyClaims_ContradictedFailsClosed(t *testing.T) {
	reply := `{"verdicts":[{"claim_id":"C1","status":"supported","evidence_ids":["E1"],"contradicted":true},{"claim_id":"C2","status":"supported","evidence_ids":["E1"]}]}`
	rc := &recordingChat{replies: []string{reply}}
	j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
	claims, missing, _, _ := j.verifyClaims(context.Background(), claimsFixture(), "blocks", refsFixture())
	if claims[0].Status != StatusUnsupported || !claims[0].Contradicted {
		t.Errorf("contradicted claim should fail closed to unsupported: %+v", claims[0])
	}
	if len(missing) == 0 {
		t.Error("contradicted claim should produce missing-evidence detail")
	}
}

func TestVerifyClaims_Malformed(t *testing.T) {
	rc := &recordingChat{replies: []string{`sorry, I could not produce JSON`}}
	j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
	_, _, _, err := j.verifyClaims(context.Background(), claimsFixture(), "blocks", refsFixture())
	if !errors.Is(err, ErrSupportVerifyMalformed) {
		t.Fatalf("err = %v, want ErrSupportVerifyMalformed", err)
	}
}

func TestVerifyClaims_DedupQueries(t *testing.T) {
	reply := `{"verdicts":[
		{"claim_id":"C1","status":"unsupported","evidence_ids":[],"missing_query":"q"},
		{"claim_id":"C2","status":"unsupported","evidence_ids":[],"missing_query":"q"}
	]}`
	rc := &recordingChat{replies: []string{reply}}
	j, _ := NewSupportJudgeWithChat(rc.fn(), "m")
	_, _, queries, _ := j.verifyClaims(context.Background(), claimsFixture(), "blocks", refsFixture())
	if len(queries) != 1 {
		t.Errorf("duplicate queries should dedup to 1, got %v", queries)
	}
}

func TestAggregateStatus(t *testing.T) {
	c := func(s SupportStatus, contra bool) ClaimSupport {
		return ClaimSupport{Status: s, Contradicted: contra}
	}
	tests := []struct {
		name   string
		claims []ClaimSupport
		want   SupportStatus
	}{
		{"empty", nil, StatusUnsupported},
		{"all supported", []ClaimSupport{c(StatusSupported, false), c(StatusSupported, false)}, StatusSupported},
		{"all unsupported", []ClaimSupport{c(StatusUnsupported, false), c(StatusUnsupported, false)}, StatusUnsupported},
		{"mixed", []ClaimSupport{c(StatusSupported, false), c(StatusUnsupported, false)}, StatusPartial},
		{"partial present", []ClaimSupport{c(StatusPartial, false), c(StatusSupported, false)}, StatusPartial},
		{"contradicted overrides", []ClaimSupport{c(StatusSupported, false), c(StatusSupported, true)}, StatusUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := aggregateStatus(tt.claims); got != tt.want {
				t.Errorf("aggregateStatus = %q, want %q", got, tt.want)
			}
		})
	}
}
