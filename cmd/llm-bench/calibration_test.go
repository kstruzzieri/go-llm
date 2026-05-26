package main

import (
	"strings"
	"testing"
)

func TestArtifactHash_DeterministicAcrossInvocations(t *testing.T) {
	a := Artifact{
		TraceID: "t1", CandidateModel: "ollama/cand",
		ActualFinalAnswer: "the answer is 4",
		ActualToolCalls:   []string{"read_file"},
		ActualTranscript:  []Turn{{Role: "assistant", Content: "the answer is 4"}},
	}
	b := a
	if artifactHash(a) != artifactHash(b) {
		t.Fatalf("artifactHash not deterministic for identical Artifact")
	}
}

func TestArtifactHash_SensitiveToFinalAnswer(t *testing.T) {
	base := Artifact{TraceID: "t", CandidateModel: "c"}
	base.ActualFinalAnswer = "v1"
	h1 := artifactHash(base)
	base.ActualFinalAnswer = "v2"
	h2 := artifactHash(base)
	if h1 == h2 {
		t.Fatalf("artifactHash not sensitive to ActualFinalAnswer")
	}
}

func TestArtifactHash_SensitiveToToolCallSequence(t *testing.T) {
	base := Artifact{TraceID: "t", CandidateModel: "c"}
	base.ActualToolCalls = []string{"a", "b"}
	h1 := artifactHash(base)
	base.ActualToolCalls = []string{"b", "a"}
	h2 := artifactHash(base)
	if h1 == h2 {
		t.Fatalf("artifactHash should be order-sensitive on ActualToolCalls")
	}
}

func TestArtifactHash_PrefixIsSha256(t *testing.T) {
	h := artifactHash(Artifact{TraceID: "t", CandidateModel: "c"})
	if !strings.HasPrefix(h, "sha256:") {
		t.Fatalf("artifactHash %q does not start with sha256:", h)
	}
	// sha256: + 64 hex chars = 71 total
	if len(h) != 71 {
		t.Fatalf("artifactHash length = %d; want 71", len(h))
	}
}
