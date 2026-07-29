package main

// Assembly-eval support (#331 PR 3a): a paired flat/progressive corpus where
// both arms of a pair carry the SAME selected candidate set rendered two
// ways. Assembly mode is first-class trace metadata — never encoded in the
// model name, never folded into the paired report's model identity. Pairing
// is (PairID, CandidateModel), so the two arms are distinct traces and the
// existing (TraceID, CandidateModel) duplicate-cell rule is never tripped.

// AssemblyMode names how retrieval context was rendered for one trace arm.
type AssemblyMode string

const (
	AssemblyFlat        AssemblyMode = "flat"        // frozen BuildContext
	AssemblyProgressive AssemblyMode = "progressive" // RenderProgressive, same budget
)

// AssemblyEval is the per-trace assembly-eval metadata. Both arms of a pair
// share PairID and must carry identical CandidateIDs (asserted at report
// time; a mismatch invalidates the case rather than skewing it).
type AssemblyEval struct {
	PairID                string       `json:"pair_id"`
	Mode                  AssemblyMode `json:"mode"`
	CandidateIDs          []string     `json:"candidate_ids"`
	EstimatedPromptTokens int          `json:"estimated_prompt_tokens"`
}
