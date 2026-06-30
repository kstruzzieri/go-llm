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
