package analysis

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

// The same answer+evidence pair is judged two ways elsewhere: the MCP quote
// verifier (mcp/tools_rag_answer.go) catches an invented literal quote, while
// this semantic judge catches an unsupported claim. Here we assert the
// semantic side: an answer whose claim is not entailed is unsupported, and an
// answer whose claim is entailed is supported, over identical evidence.
func TestSemanticJudge_FixtureDefectClasses(t *testing.T) {
	evidence := []rag.SearchResult{ev("h1", "doc.md", "The service retries 3 times on failure.", 1, 1)}

	supported := &recordingChat{replies: []string{
		`{"claims":["the service retries three times"]}`,
		`{"verdicts":[{"claim_id":"C1","status":"supported","evidence_ids":["E1"]}]}`,
	}}
	j1, _ := NewSupportJudgeWithChat(supported.fn(), "m")
	r1, err := j1.Judge(context.Background(), "It retries three times.", evidence)
	if err != nil || r1.Status != StatusSupported {
		t.Fatalf("entailed claim should be supported: status=%v err=%v", r1.Status, err)
	}

	unsupported := &recordingChat{replies: []string{
		`{"claims":["the service sends an email on failure"]}`,
		`{"verdicts":[{"claim_id":"C1","status":"unsupported","evidence_ids":[],"missing_query":"email on failure"}]}`,
	}}
	j2, _ := NewSupportJudgeWithChat(unsupported.fn(), "m")
	r2, _ := j2.Judge(context.Background(), "It emails you on failure.", evidence)
	if r2.Status != StatusUnsupported {
		t.Fatalf("non-entailed claim should be unsupported, got %v", r2.Status)
	}
}
