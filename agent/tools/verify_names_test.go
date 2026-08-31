package tools

import (
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// TestWorkspaceMutatorNamesMatchTheRuntimeConstants pins the #347 eligibility
// predicate to the real tool names. The constants live in package agent
// because agent cannot import this package (that direction is the import
// cycle), so this is the only side where the pair can be compared. A rename of
// either Spec name without the matching constant would silently stop
// triggering post-write verification.
func TestWorkspaceMutatorNamesMatchTheRuntimeConstants(t *testing.T) {
	cases := []struct {
		tool agent.Tool
		want string
	}{
		{&WriteFile{}, agent.WriteFileToolName},
		{&EditFile{}, agent.EditFileToolName},
	}
	for _, tc := range cases {
		if got := tc.tool.Spec().Name; got != tc.want {
			t.Fatalf("Spec().Name = %q, runtime constant = %q", got, tc.want)
		}
	}
}
