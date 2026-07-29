package main

import (
	"strings"
	"testing"
)

func validAssemblyTrace(mode AssemblyMode) Trace {
	return Trace{
		ID:     "case-1-" + string(mode),
		Source: "assembly-corpus",
		System: "answer from context",
		Turns:  []Turn{{Role: "user", Content: "ctx + question"}},
		Golden: Golden{FinalAnswerCriteria: "names the port"},
		AssemblyEval: &AssemblyEval{
			PairID:                "case-1",
			Mode:                  mode,
			CandidateIDs:          []string{"c1", "c2"},
			EstimatedPromptTokens: 100,
		},
	}
}

func TestValidateTraceAssemblyEval(t *testing.T) {
	if err := validateTrace(validAssemblyTrace(AssemblyFlat)); err != nil {
		t.Fatalf("valid flat trace rejected: %v", err)
	}
	if err := validateTrace(validAssemblyTrace(AssemblyProgressive)); err != nil {
		t.Fatalf("valid progressive trace rejected: %v", err)
	}

	bad := validAssemblyTrace(AssemblyFlat)
	bad.AssemblyEval.Mode = "outline"
	if err := validateTrace(bad); err == nil || !strings.Contains(err.Error(), "assembly_eval") {
		t.Fatalf("unknown mode accepted or wrong error: %v", err)
	}

	bad = validAssemblyTrace(AssemblyFlat)
	bad.AssemblyEval.PairID = ""
	if err := validateTrace(bad); err == nil || !strings.Contains(err.Error(), "assembly_eval") {
		t.Fatalf("blank PairID accepted or wrong error: %v", err)
	}

	bad = validAssemblyTrace(AssemblyFlat)
	bad.AssemblyEval.CandidateIDs = nil
	if err := validateTrace(bad); err == nil || !strings.Contains(err.Error(), "assembly_eval") {
		t.Fatalf("empty CandidateIDs accepted or wrong error: %v", err)
	}

	bad = validAssemblyTrace(AssemblyFlat)
	bad.AssemblyEval.EstimatedPromptTokens = 0
	if err := validateTrace(bad); err == nil || !strings.Contains(err.Error(), "assembly_eval") {
		t.Fatalf("non-positive EstimatedPromptTokens accepted or wrong error: %v", err)
	}

	// nil AssemblyEval is a normal non-assembly trace — untouched path.
	plain := validAssemblyTrace(AssemblyFlat)
	plain.AssemblyEval = nil
	if err := validateTrace(plain); err != nil {
		t.Fatalf("plain trace rejected: %v", err)
	}
}
